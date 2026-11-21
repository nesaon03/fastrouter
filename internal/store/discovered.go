package store

import (
	"database/sql"
	"fmt"
	"time"
)

// DiscoveredModel is a model id found at run time from a provider's own model
// list, instead of from the compiled catalog. New models therefore show up and
// route without a new FastRouter build.
//
// Discovery is an overlay, never a source of truth: catalog.Provider.Upstream
// still decides which id is sent upstream, and a discovered id that is only a
// wire id is skipped when it is merged (see router.Build).
type DiscoveredModel struct {
	Provider  string // provider id, or a custom node's id
	Model     string // exactly as the provider lists it
	FirstSeen string
	LastSeen  string
	LastOKAt  string // last time a request for it succeeded ("" = never seen working)
	Status    string // listed | verified | dead
	Source    string // api | manual
	Misses    int    // consecutive polls that did not list it
	Caps      string // JSON, e.g. {"vision":true}
}

// Discovery statuses.
const (
	DiscoveredListed   = "listed"   // the provider lists it
	DiscoveredVerified = "verified" // it also answered a request
	DiscoveredDead     = "dead"     // gone for two polls in a row, or model_not_found
)

const tsLayout = time.RFC3339

// SaveDiscovered records one provider's model list: rows that were not listed
// this time gain a miss and become dead after two misses, so one bad poll does
// not erase a working model.
func (s *Store) SaveDiscovered(provider string, found []DiscoveredModel, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := now.UTC().Format(tsLayout)
	keep := make(map[string]bool, len(found))
	for _, m := range found {
		if m.Model == "" || keep[m.Model] {
			continue
		}
		keep[m.Model] = true
		source := firstNonEmptyStr(m.Source, "api")
		// A model the provider lists again is advertised again: the two-miss rule
		// below is what protects against one bad poll, and a per-account 404 is not
		// evidence of anything (plans differ, so one account may not have a model
		// another one serves).
		if _, err := tx.Exec(`INSERT INTO discovered_models
			(provider, model, first_seen, last_seen, last_ok_at, status, source, misses, caps)
			VALUES (?,?,?,?,'',?,?,0,?)
			ON CONFLICT(provider, model) DO UPDATE SET
			  last_seen = excluded.last_seen,
			  misses    = 0,
			  status    = excluded.status,
			  caps      = CASE WHEN excluded.caps <> '' THEN excluded.caps ELSE discovered_models.caps END`,
			provider, m.Model, at, at, firstNonEmptyStr(m.Status, DiscoveredListed), source, m.Caps); err != nil {
			return err
		}
	}

	type gone struct {
		model  string
		misses int
	}
	var absent []gone
	rows, err := tx.Query(`SELECT model, misses FROM discovered_models WHERE provider = ?`, provider)
	if err != nil {
		return err
	}
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.model, &g.misses); err != nil {
			rows.Close()
			return err
		}
		if !keep[g.model] {
			absent = append(absent, g)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range absent {
		misses := g.misses + 1
		status := DiscoveredListed
		if misses >= 2 {
			status = DiscoveredDead
		}
		if _, err := tx.Exec(`UPDATE discovered_models SET misses = ?, status = ? WHERE provider = ? AND model = ?`,
			misses, status, provider, g.model); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetDiscoveredStatus moves one discovered model's status (dead on a 404, or
// verified after a successful request).
func (s *Store) SetDiscoveredStatus(provider, model, status string, now time.Time) error {
	if provider == "" || model == "" {
		return nil
	}
	at := now.UTC().Format(tsLayout)
	if status == DiscoveredVerified {
		_, err := s.db.Exec(`UPDATE discovered_models SET status = ?, last_ok_at = ? WHERE provider = ? AND model = ?`,
			status, at, provider, model)
		return err
	}
	// Only flip rows that exist: discovery must not invent an entry for a model
	// nobody listed.
	_, err := s.db.Exec(`UPDATE discovered_models SET status = ?, misses = MAX(misses, 2) WHERE provider = ? AND model = ?`,
		status, provider, model)
	return err
}

// loadDiscovered reads the discovery overlay into snap.
func (s *Store) loadDiscovered(snap *Snapshot) error {
	rows, err := s.db.Query(`SELECT provider, model, first_seen, last_seen, COALESCE(last_ok_at,''),
		status, COALESCE(source,'api'), misses, COALESCE(caps,'')
		FROM discovered_models ORDER BY provider, model`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m DiscoveredModel
		if err := rows.Scan(&m.Provider, &m.Model, &m.FirstSeen, &m.LastSeen, &m.LastOKAt,
			&m.Status, &m.Source, &m.Misses, &m.Caps); err != nil {
			return err
		}
		snap.Discovered = append(snap.Discovered, m)
	}
	return rows.Err()
}

// DiscoveredFor returns the discovered models for one provider ("" = all).
func (s *Store) DiscoveredFor(provider string) ([]DiscoveredModel, error) {
	q := `SELECT provider, model, first_seen, last_seen, COALESCE(last_ok_at,''),
		status, COALESCE(source,'api'), misses, COALESCE(caps,'')
		FROM discovered_models`
	var (
		rows *sql.Rows
		err  error
	)
	if provider == "" {
		rows, err = s.db.Query(q + ` ORDER BY provider, model`)
	} else {
		rows, err = s.db.Query(q+` WHERE provider = ? ORDER BY model`, provider)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscoveredModel
	for rows.Next() {
		var m DiscoveredModel
		if err := rows.Scan(&m.Provider, &m.Model, &m.FirstSeen, &m.LastSeen, &m.LastOKAt,
			&m.Status, &m.Source, &m.Misses, &m.Caps); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClearDiscovered drops one provider's overlay (dashboard "forget").
func (s *Store) ClearDiscovered(provider string) error {
	if provider == "" {
		return fmt.Errorf("store: clear discovered: empty provider")
	}
	_, err := s.db.Exec(`DELETE FROM discovered_models WHERE provider = ?`, provider)
	return err
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
