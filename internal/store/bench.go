package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// BenchRun is one stored Speed Test run. Options, Summary and Results are JSON
// produced by the bench package; none of them ever hold a key.
type BenchRun struct {
	ID         string          `json:"id"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt time.Time       `json:"finishedAt,omitzero"`
	Status     string          `json:"status"` // running | done | stopped | failed
	Source     string          `json:"source"` // dashboard | cli
	Options    json.RawMessage `json:"options"`
	Summary    json.RawMessage `json:"summary"`
	Results    json.RawMessage `json:"results,omitempty"`
}

// keepBenchRuns caps the history so the table stays small.
const keepBenchRuns = 200

// SaveBenchRun inserts or replaces a run and trims old history.
func (s *Store) SaveBenchRun(r BenchRun) error {
	fin := ""
	if !r.FinishedAt.IsZero() {
		fin = r.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO bench_runs (id, started_at, finished_at, status, source, options, summary, results)
		VALUES (?,?,?,?,?,?,?,?)`, r.ID, r.StartedAt.UTC().Format(time.RFC3339Nano), fin, r.Status, r.Source,
		string(orNull(r.Options)), string(orNull(r.Summary)), string(orNull(r.Results)))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM bench_runs WHERE id NOT IN (SELECT id FROM bench_runs ORDER BY started_at DESC LIMIT ?)`, keepBenchRuns)
	return err
}

func orNull(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

// BenchRuns lists recent runs, newest first, without per-request results.
func (s *Store) BenchRuns(limit int) ([]BenchRun, error) {
	rows, err := s.db.Query(`SELECT id, started_at, finished_at, status, source, options, summary, '' FROM bench_runs
		ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchRun{}
	for rows.Next() {
		r, err := scanBench(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BenchRun returns one run with its results.
func (s *Store) BenchRun(id string) (BenchRun, error) {
	r, err := scanBench(s.db.QueryRow(`SELECT id, started_at, finished_at, status, source, options, summary, results FROM bench_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// DeleteBenchRun removes one run ("" removes all).
func (s *Store) DeleteBenchRun(id string) error {
	if id == "" {
		_, err := s.db.Exec(`DELETE FROM bench_runs`)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM bench_runs WHERE id = ?`, id)
	return err
}

// ErrNotFound is returned for a missing row.
var ErrNotFound = errors.New("not found")

func scanBench(sc interface{ Scan(...any) error }) (BenchRun, error) {
	var r BenchRun
	var start, fin, opts, sum, res sql.NullString
	if err := sc.Scan(&r.ID, &start, &fin, &r.Status, &r.Source, &opts, &sum, &res); err != nil {
		return r, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339Nano, start.String)
	r.FinishedAt, _ = time.Parse(time.RFC3339Nano, fin.String)
	for _, x := range []struct {
		s *sql.NullString
		d *json.RawMessage
	}{{&opts, &r.Options}, {&sum, &r.Summary}, {&res, &r.Results}} {
		if x.s.String != "" {
			*x.d = json.RawMessage(x.s.String)
		}
	}
	return r, nil
}
