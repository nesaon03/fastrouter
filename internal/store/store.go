// Package store persists FastRouter's configuration in SQLite (pure Go driver).
// Secrets are sealed with secret.Box before they touch disk.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nesaon03/fastrouter/internal/secret"
	_ "modernc.org/sqlite"
)

// Node is a custom OpenAI- or Anthropic-compatible endpoint.
type Node struct {
	ID, Name, Prefix, Type, APIType, BaseURL string
	Aliases                                  []string // extra prefixes that also reach this endpoint
	DefaultModel                             string   // model for tests and bare "prefix" / "prefix/default" requests ("" = first listed)
}

// Connection is one provider account.
type Connection struct {
	ID, Provider, Name, AuthType string
	Priority                     int
	Active                       bool
	Status                       string            // ok | needs-plugin | unknown-provider
	Secrets                      map[string]string // decrypted, memory only
	Meta                         map[string]any    // non-secret details
}

// Combo is a named list of models tried by a strategy.
type Combo struct {
	ID, Name, Kind, Strategy string
	Models                   []string
}

// APIKey is a local key that clients use to call /v1.
type APIKey struct {
	ID, Name string
	Key      string // decrypted, memory only
	Active   bool
	Access   json.RawMessage
}

// CustomModel is a user-added model id under a provider alias.
type CustomModel struct {
	ProviderAlias, ID, Type, Name string
	Caps                          json.RawMessage
}

// Warning is an import note shown in the dashboard.
type Warning struct{ Kind, Subject, Message string }

// Snapshot is the full configuration.
type Snapshot struct {
	Nodes        []Node
	Connections  []Connection
	Combos       []Combo
	APIKeys      []APIKey
	CustomModels []CustomModel
	Warnings     []Warning
	// Discovered is the run-time model-discovery overlay (see DiscoveredModel).
	// It is not imported configuration and survives a config rewrite.
	Discovered        []DiscoveredModel
	Settings          map[string]json.RawMessage // non-secret 9Router settings, by key
	DashboardPassword string                     // kept exactly as imported (bcrypt hash)
}

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB
	box *secret.Box
}

const schema = `
CREATE TABLE IF NOT EXISTS nodes (id TEXT PRIMARY KEY, name TEXT, prefix TEXT, type TEXT, api_type TEXT, base_url TEXT, ord INTEGER);
CREATE TABLE IF NOT EXISTS connections (id TEXT PRIMARY KEY, provider TEXT, name TEXT, auth_type TEXT, priority INTEGER,
  active INTEGER, status TEXT, secrets BLOB, meta TEXT, ord INTEGER, last_used_at TEXT, last_status INTEGER, uses INTEGER DEFAULT 0, failures INTEGER DEFAULT 0);
CREATE TABLE IF NOT EXISTS combos (id TEXT PRIMARY KEY, name TEXT, kind TEXT, strategy TEXT, models TEXT, ord INTEGER);
CREATE TABLE IF NOT EXISTS api_keys (id TEXT PRIMARY KEY, name TEXT, secret BLOB, active INTEGER, access TEXT, ord INTEGER);
CREATE TABLE IF NOT EXISTS custom_models (provider_alias TEXT, model_id TEXT, type TEXT, name TEXT, caps TEXT, ord INTEGER);
CREATE TABLE IF NOT EXISTS warnings (kind TEXT, subject TEXT, message TEXT, ord INTEGER);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT, secret BLOB);
CREATE TABLE IF NOT EXISTS usage (ts TEXT, connection_id TEXT, model TEXT, status INTEGER, latency_ms INTEGER);
`

// Open opens (or creates) dataDir/fastrouter.db and the key file dataDir/secret.key.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	box, err := secret.LoadOrCreate(filepath.Join(dataDir, "secret.key"))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "fastrouter.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; reads happen once at startup
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return &Store{db: db, box: box}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) seal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return s.box.Seal(b)
}

func (s *Store) open(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}
	plain, err := s.box.Open(b)
	if err != nil {
		return fmt.Errorf("decrypt (wrong secret.key?): %w", err)
	}
	return json.Unmarshal(plain, v)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ReplaceAll overwrites the configuration with snap in one transaction.
// Usage history is kept.
func (s *Store) ReplaceAll(snap *Snapshot) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Per-account counters survive a config rewrite (dashboard edits).
	stats := map[string][4]any{}
	if rows, err := tx.Query(`SELECT id,last_used_at,last_status,uses,failures FROM connections`); err == nil {
		for rows.Next() {
			var id string
			var at sql.NullString
			var st, uses, fails sql.NullInt64
			if rows.Scan(&id, &at, &st, &uses, &fails) == nil {
				stats[id] = [4]any{at, st, uses, fails}
			}
		}
		rows.Close()
	}
	for _, t := range []string{"nodes", "connections", "combos", "api_keys", "custom_models", "warnings", "settings"} {
		if _, err := tx.Exec("DELETE FROM " + t); err != nil {
			return err
		}
	}
	for i, n := range snap.Nodes {
		if _, err := tx.Exec(`INSERT INTO nodes (id,name,prefix,type,api_type,base_url,ord,aliases,default_model) VALUES (?,?,?,?,?,?,?,?,?)`,
			n.ID, n.Name, n.Prefix, n.Type, n.APIType, n.BaseURL, i, strings.Join(n.Aliases, ","), n.DefaultModel); err != nil {
			return err
		}
	}
	for i, c := range snap.Connections {
		sec, err := s.seal(c.Secrets)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO connections (id,provider,name,auth_type,priority,active,status,secrets,meta,ord) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			c.ID, c.Provider, c.Name, c.AuthType, c.Priority, c.Active, c.Status, sec, mustJSON(c.Meta), i); err != nil {
			return err
		}
		if v, ok := stats[c.ID]; ok {
			if _, err := tx.Exec(`UPDATE connections SET last_used_at=?, last_status=?, uses=?, failures=? WHERE id=?`, v[0], v[1], v[2], v[3], c.ID); err != nil {
				return err
			}
		}
	}
	for i, c := range snap.Combos {
		if _, err := tx.Exec(`INSERT INTO combos VALUES (?,?,?,?,?,?)`, c.ID, c.Name, c.Kind, c.Strategy, mustJSON(c.Models), i); err != nil {
			return err
		}
	}
	for i, k := range snap.APIKeys {
		sec, err := s.seal(k.Key)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO api_keys VALUES (?,?,?,?,?,?)`, k.ID, k.Name, sec, k.Active, string(k.Access), i); err != nil {
			return err
		}
	}
	for i, m := range snap.CustomModels {
		if _, err := tx.Exec(`INSERT INTO custom_models VALUES (?,?,?,?,?,?)`, m.ProviderAlias, m.ID, m.Type, m.Name, string(m.Caps), i); err != nil {
			return err
		}
	}
	for i, w := range snap.Warnings {
		if _, err := tx.Exec(`INSERT INTO warnings VALUES (?,?,?,?)`, w.Kind, w.Subject, w.Message, i); err != nil {
			return err
		}
	}
	for k, v := range snap.Settings {
		if SealedSetting(k, v) { // M7: proxy pools and credential URLs are encrypted too
			sec, err := s.seal(v)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO settings (key,secret) VALUES (?,?)`, k, sec); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`INSERT INTO settings (key,value) VALUES (?,?)`, k, string(v)); err != nil {
			return err
		}
	}
	if snap.DashboardPassword != "" {
		sec, err := s.seal(snap.DashboardPassword)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO settings (key,secret) VALUES ('password',?)`, sec); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Load reads the whole configuration, decrypting secrets into memory.
func (s *Store) Load() (*Snapshot, error) {
	snap := &Snapshot{Settings: map[string]json.RawMessage{}}
	rows, err := s.db.Query(`SELECT id,name,prefix,type,api_type,base_url,COALESCE(aliases,''),COALESCE(default_model,'') FROM nodes ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n Node
		var aliases string
		if err := rows.Scan(&n.ID, &n.Name, &n.Prefix, &n.Type, &n.APIType, &n.BaseURL, &aliases, &n.DefaultModel); err != nil {
			return nil, err
		}
		if aliases != "" {
			n.Aliases = strings.Split(aliases, ",")
		}
		snap.Nodes = append(snap.Nodes, n)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT id,provider,name,auth_type,priority,active,status,secrets,meta FROM connections ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Connection
		var sec []byte
		var meta string
		if err := rows.Scan(&c.ID, &c.Provider, &c.Name, &c.AuthType, &c.Priority, &c.Active, &c.Status, &sec, &meta); err != nil {
			return nil, err
		}
		if err := s.open(sec, &c.Secrets); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(meta), &c.Meta)
		snap.Connections = append(snap.Connections, c)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT id,name,kind,strategy,models FROM combos ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Combo
		var models string
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Strategy, &models); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(models), &c.Models)
		snap.Combos = append(snap.Combos, c)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT id,name,secret,active,access FROM api_keys ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k APIKey
		var sec []byte
		var access string
		if err := rows.Scan(&k.ID, &k.Name, &sec, &k.Active, &access); err != nil {
			return nil, err
		}
		if err := s.open(sec, &k.Key); err != nil {
			return nil, err
		}
		k.Access = json.RawMessage(access)
		snap.APIKeys = append(snap.APIKeys, k)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT provider_alias,model_id,type,name,caps FROM custom_models ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m CustomModel
		var caps string
		if err := rows.Scan(&m.ProviderAlias, &m.ID, &m.Type, &m.Name, &caps); err != nil {
			return nil, err
		}
		m.Caps = json.RawMessage(caps)
		snap.CustomModels = append(snap.CustomModels, m)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT kind,subject,message FROM warnings ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var w Warning
		if err := rows.Scan(&w.Kind, &w.Subject, &w.Message); err != nil {
			return nil, err
		}
		snap.Warnings = append(snap.Warnings, w)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT key,value,secret FROM settings`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var v sql.NullString
		var sec []byte
		if err := rows.Scan(&k, &v, &sec); err != nil {
			return nil, err
		}
		if k == "password" {
			if err := s.open(sec, &snap.DashboardPassword); err != nil {
				return nil, err
			}
			continue
		}
		if !v.Valid && len(sec) > 0 {
			var raw json.RawMessage
			if err := s.open(sec, &raw); err != nil {
				return nil, err
			}
			snap.Settings[k] = raw
			continue
		}
		snap.Settings[k] = json.RawMessage(v.String)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.loadDiscovered(snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// SealedSetting reports whether a setting is stored encrypted: 9Router's proxy
// pools always, and any value that embeds a credential.
func SealedSetting(k string, v json.RawMessage) bool {
	return k == "backup.proxyPools" || secret.LooksLikeCredential(string(v))
}

// ConnStats are an account's request counters.
type ConnStats struct {
	LastUsedAt string
	LastStatus int
	Uses       int64
	Failures   int64
}

// ConnectionStats returns request counters by connection id.
func (s *Store) ConnectionStats() (map[string]ConnStats, error) {
	rows, err := s.db.Query(`SELECT id,last_used_at,last_status,uses,failures FROM connections`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ConnStats{}
	for rows.Next() {
		var id string
		var at sql.NullString
		var st, uses, fails sql.NullInt64
		if err := rows.Scan(&id, &at, &st, &uses, &fails); err != nil {
			return nil, err
		}
		out[id] = ConnStats{at.String, int(st.Int64), uses.Int64, fails.Int64}
	}
	return out, rows.Err()
}

// RawDB exposes the database for tests.
func (s *Store) RawDB() *sql.DB { return s.db }

// SaveConnection inserts or updates one connection (sign-ins and token refresh).
func (s *Store) SaveConnection(c Connection) error {
	sec, err := s.seal(c.Secrets)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO connections (id,provider,name,auth_type,priority,active,status,secrets,meta,ord)
		VALUES (?,?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(ord),-1)+1 FROM connections))
		ON CONFLICT(id) DO UPDATE SET provider=excluded.provider, name=excluded.name, auth_type=excluded.auth_type,
		priority=excluded.priority, active=excluded.active, status=excluded.status, secrets=excluded.secrets, meta=excluded.meta`,
		c.ID, c.Provider, c.Name, c.AuthType, c.Priority, c.Active, c.Status, sec, mustJSON(c.Meta))
	return err
}
