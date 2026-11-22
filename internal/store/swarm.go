package store

import (
	"encoding/json"
	"time"
)

// SwarmRunRow is the metadata of a persisted swarm debate. Doc is only loaded
// when the full transcript is requested, so the Combos page poll stays small.
type SwarmRunRow struct {
	ID        string          `json:"id"`
	Combo     string          `json:"combo"`
	Started   string          `json:"started"`
	Status    string          `json:"status"`
	Consensus bool            `json:"consensus"`
	Doc       json.RawMessage `json:"-"`
}

// SaveSwarmRun stores the full transcript of one finished swarm execution.
func (s *Store) SaveSwarmRun(id, combo string, started time.Time, status string, consensus bool, doc []byte) error {
	c := 0
	if consensus {
		c = 1
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO swarm_runs (id,combo,started,status,consensus,doc) VALUES (?,?,?,?,?,?)`,
		id, combo, started.UTC().Format(time.RFC3339Nano), status, c, doc)
	return err
}

// SwarmRuns returns recent persisted debates, newest first, without transcripts.
func (s *Store) SwarmRuns(limit int) ([]SwarmRunRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id,combo,started,status,consensus FROM swarm_runs ORDER BY started DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SwarmRunRow{}
	for rows.Next() {
		var r SwarmRunRow
		var c int
		if err := rows.Scan(&r.ID, &r.Combo, &r.Started, &r.Status, &c); err != nil {
			return nil, err
		}
		r.Consensus = c != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SwarmRun returns the full transcript document for one debate.
func (s *Store) SwarmRun(id string) (json.RawMessage, error) {
	var doc []byte
	if err := s.db.QueryRow(`SELECT doc FROM swarm_runs WHERE id=?`, id).Scan(&doc); err != nil {
		return nil, err
	}
	return json.RawMessage(doc), nil
}
