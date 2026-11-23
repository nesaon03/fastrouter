package store

import "encoding/json"

// SaveSwarmMemory stores one (owner, task class) learning document.
func (s *Store) SaveSwarmMemory(owner, class string, doc []byte) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO swarm_memory (owner,class,doc) VALUES (?,?,?)`, owner, class, doc)
	return err
}

// SwarmMemory returns the learning document, or nil when none exists yet.
func (s *Store) SwarmMemory(owner, class string) (json.RawMessage, error) {
	var doc []byte
	err := s.db.QueryRow(`SELECT doc FROM swarm_memory WHERE owner=? AND class=?`, owner, class).Scan(&doc)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(doc), nil
}

// ClearSwarmMemory forgets everything learned for one owner; an empty owner
// clears all lessons (the dashboard's Reset learning button).
func (s *Store) ClearSwarmMemory(owner string) error {
	if owner == "" {
		_, err := s.db.Exec(`DELETE FROM swarm_memory`)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM swarm_memory WHERE owner=?`, owner)
	return err
}
