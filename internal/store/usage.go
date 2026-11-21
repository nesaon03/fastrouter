package store

import (
	"database/sql"
	"strings"
	"time"
)

// Phase 2 columns, added in place so Phase 1 databases keep their history.
var migrations = []string{
	`ALTER TABLE nodes ADD COLUMN default_model TEXT DEFAULT ''`,
	`ALTER TABLE usage ADD COLUMN provider TEXT DEFAULT ''`,
	`ALTER TABLE usage ADD COLUMN key_id TEXT DEFAULT ''`,
	`ALTER TABLE usage ADD COLUMN input INTEGER DEFAULT 0`,
	`ALTER TABLE usage ADD COLUMN output INTEGER DEFAULT 0`,
	`ALTER TABLE usage ADD COLUMN cache_read INTEGER DEFAULT 0`,
	`ALTER TABLE usage ADD COLUMN cache_write INTEGER DEFAULT 0`,
	`ALTER TABLE usage ADD COLUMN reasoning INTEGER DEFAULT 0`,
	`ALTER TABLE usage ADD COLUMN cost REAL DEFAULT 0`,
	`ALTER TABLE connections ADD COLUMN tokens_in INTEGER DEFAULT 0`,
	`ALTER TABLE connections ADD COLUMN tokens_out INTEGER DEFAULT 0`,
	`ALTER TABLE connections ADD COLUMN cost REAL DEFAULT 0`,
	`CREATE INDEX IF NOT EXISTS usage_ts ON usage(ts)`,
	`ALTER TABLE nodes ADD COLUMN aliases TEXT DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS bench_runs (id TEXT PRIMARY KEY, started_at TEXT, finished_at TEXT, status TEXT,
		source TEXT, options TEXT, summary TEXT, results TEXT)`,
	`CREATE INDEX IF NOT EXISTS bench_runs_started ON bench_runs(started_at)`,
	// Runtime model discovery. Deliberately absent from ReplaceAll's table list:
	// a config rewrite (dashboard edit, re-import) must not forget what the
	// providers actually serve.
	`CREATE TABLE IF NOT EXISTS discovered_models (
		provider TEXT NOT NULL, model TEXT NOT NULL,
		first_seen TEXT, last_seen TEXT, last_ok_at TEXT DEFAULT '',
		status TEXT, source TEXT, misses INTEGER DEFAULT 0, caps TEXT DEFAULT '',
		PRIMARY KEY (provider, model))`,
	`CREATE INDEX IF NOT EXISTS discovered_provider ON discovered_models(provider)`,
}

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// UsageEvent is one finished upstream attempt.
type UsageEvent struct {
	At           time.Time
	ConnectionID string
	Provider     string
	KeyID        string // local API key id ("" when auth is off)
	Model        string
	Status       int // HTTP status, or 0 for a transport error
	Latency      time.Duration

	Input, Output, CacheRead, CacheWrite, Reasoning int64   // tokens, from the usage fields
	Cost                                            float64 // USD, from the pricing table
}

// WriteUsage records a batch of events in one transaction.
func (s *Store) WriteUsage(events []UsageEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT INTO usage (ts, connection_id, model, status, latency_ms, provider, key_id,
		input, output, cache_read, cache_write, reasoning, cost) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	upd, err := tx.Prepare(`UPDATE connections SET last_used_at=?, last_status=?, uses=uses+1, failures=failures+?,
		tokens_in=tokens_in+?, tokens_out=tokens_out+?, cost=cost+? WHERE id=?`)
	if err != nil {
		return err
	}
	for _, e := range events {
		ts := e.At.UTC().Format(time.RFC3339Nano)
		fail := 0
		if e.Status < 200 || e.Status > 299 {
			fail = 1
		}
		if _, err := ins.Exec(ts, e.ConnectionID, e.Model, e.Status, e.Latency.Milliseconds(), e.Provider, e.KeyID,
			e.Input, e.Output, e.CacheRead, e.CacheWrite, e.Reasoning, e.Cost); err != nil {
			return err
		}
		if e.ConnectionID != "" {
			if _, err := upd.Exec(ts, e.Status, fail, e.Input, e.Output, e.Cost, e.ConnectionID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// UsageRow is one aggregate line of a usage report.
type UsageRow struct {
	Key        string  `json:"key"` // connection id, model, or api key id
	Name       string  `json:"name,omitempty"`
	Provider   string  `json:"provider,omitempty"`
	Requests   int64   `json:"requests"`
	Failures   int64   `json:"failures"`
	Input      int64   `json:"input_tokens"`
	Output     int64   `json:"output_tokens"`
	CacheRead  int64   `json:"cache_read_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	Reasoning  int64   `json:"reasoning_tokens"`
	Cost       float64 `json:"cost_usd"`
	AvgLatency int64   `json:"avg_latency_ms"`
}

// UsageReport aggregates usage since a time, grouped by "account", "model" or "key".
func (s *Store) UsageReport(since time.Time, by string) ([]UsageRow, error) {
	col := map[string]string{"account": "u.connection_id", "model": "u.provider || '/' || u.model", "key": "u.key_id", "provider": "u.provider"}[by]
	if col == "" {
		col = "u.connection_id"
	}
	q := `SELECT ` + col + `, COALESCE(MAX(c.name),''), COALESCE(MAX(u.provider),''), COUNT(*),
		SUM(CASE WHEN u.status BETWEEN 200 AND 299 THEN 0 ELSE 1 END),
		SUM(u.input), SUM(u.output), SUM(u.cache_read), SUM(u.cache_write), SUM(u.reasoning), SUM(u.cost), CAST(AVG(u.latency_ms) AS INTEGER)
		FROM usage u LEFT JOIN connections c ON c.id = u.connection_id
		WHERE u.ts >= ? GROUP BY 1 ORDER BY 4 DESC`
	rows, err := s.db.Query(q, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Key, &r.Name, &r.Provider, &r.Requests, &r.Failures, &r.Input, &r.Output,
			&r.CacheRead, &r.CacheWrite, &r.Reasoning, &r.Cost, &r.AvgLatency); err != nil {
			return nil, err
		}
		if by != "account" {
			r.Name = ""
		}
		if by == "key" {
			r.Provider = ""
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageEntry is one recorded attempt, for the recent-requests list.
type UsageEntry struct {
	At        string  `json:"at"`
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	Account   string  `json:"account,omitempty"`
	KeyID     string  `json:"keyId,omitempty"`
	Status    int     `json:"status"`
	LatencyMs int64   `json:"latency_ms"`
	Input     int64   `json:"input_tokens"`
	Output    int64   `json:"output_tokens"`
	CacheRead int64   `json:"cache_read_tokens"`
	Cost      float64 `json:"cost_usd"`
}

// UsageEntries lists attempts newest first. provider filters when not "".
func (s *Store) UsageEntries(since time.Time, provider string, limit, offset int) ([]UsageEntry, int64, error) {
	where := `u.ts >= ?`
	args := []any{since.UTC().Format(time.RFC3339Nano)}
	if provider != "" {
		where += ` AND u.provider = ?`
		args = append(args, provider)
	}
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage u WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT u.ts, COALESCE(u.provider,''), u.model, COALESCE(c.name,''), COALESCE(u.key_id,''), u.status, u.latency_ms,
		u.input, u.output, u.cache_read, u.cost FROM usage u LEFT JOIN connections c ON c.id = u.connection_id
		WHERE `+where+` ORDER BY u.ts DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []UsageEntry{}
	for rows.Next() {
		var e UsageEntry
		if err := rows.Scan(&e.At, &e.Provider, &e.Model, &e.Account, &e.KeyID, &e.Status, &e.LatencyMs, &e.Input, &e.Output, &e.CacheRead, &e.Cost); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// UsageBucket is one hour of traffic (UTC hour start).
type UsageBucket struct {
	Hour     time.Time
	Requests int64
	Tokens   int64
	Cost     float64
}

// UsageHours aggregates traffic per UTC hour since a time.
func (s *Store) UsageHours(since time.Time) ([]UsageBucket, error) {
	rows, err := s.db.Query(`SELECT substr(ts,1,13), COUNT(*), SUM(input+output), SUM(cost) FROM usage WHERE ts >= ? GROUP BY 1 ORDER BY 1`,
		since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageBucket{}
	for rows.Next() {
		var h string
		var b UsageBucket
		if err := rows.Scan(&h, &b.Requests, &b.Tokens, &b.Cost); err != nil {
			return nil, err
		}
		if b.Hour, err = time.Parse("2006-01-02T15", h); err == nil {
			out = append(out, b)
		}
	}
	return out, rows.Err()
}

// WorkingModels returns, per provider, the upstream model with the most
// successful requests in the last 30 days (used to pick live-test models).
func (s *Store) WorkingModels() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT provider, model, COUNT(*) AS n FROM usage
		WHERE status = 200 AND ts >= ? GROUP BY provider, model ORDER BY n DESC`, time.Now().Add(-30*24*time.Hour).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, m string
		var n int64
		if rows.Scan(&p, &m, &n) == nil && out[p] == "" {
			out[p] = m
		}
	}
	return out, rows.Err()
}

// LastModelStatus returns the HTTP status of the latest request per provider
// and model within the last 7 days.
func (s *Store) LastModelStatus() (map[string]map[string]int, error) {
	rows, err := s.db.Query(`SELECT provider, model, status FROM (
		SELECT provider, model, status, ROW_NUMBER() OVER (PARTITION BY provider, model ORDER BY ts DESC) AS rn
		FROM usage WHERE ts >= ?) WHERE rn = 1`, time.Now().Add(-7*24*time.Hour).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var p, m string
		var st int
		if rows.Scan(&p, &m, &st) == nil {
			if out[p] == nil {
				out[p] = map[string]int{}
			}
			out[p][m] = st
		}
	}
	return out, rows.Err()
}
