// Package peers holds the trust-on-first-use (TOFU) store: the set of sender
// fingerprints this host has approved, persisted to disk as JSON.
package peers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Trusted is one approved sender.
type Trusted struct {
	Fingerprint string    `json:"fingerprint"`
	Name        string    `json:"name"`
	ApprovedAt  time.Time `json:"approved_at"`
}

// Store is the on-disk trust store, safe for concurrent use.
type Store struct {
	path string
	mu   sync.RWMutex
	set  map[string]Trusted // keyed by fingerprint
}

// Open loads the trust store from dataDir, creating an empty one if absent.
func Open(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "trusted.json"), set: map[string]Trusted{}}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Trusted
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, t := range list {
		s.set[t.Fingerprint] = t
	}
	return s, nil
}

// IsTrusted reports whether fingerprint has been approved.
func (s *Store) IsTrusted(fingerprint string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.set[fingerprint]
	return ok
}

// Approve records fingerprint as trusted and persists the store.
func (s *Store) Approve(fingerprint, name string) error {
	s.mu.Lock()
	s.set[fingerprint] = Trusted{Fingerprint: fingerprint, Name: name, ApprovedAt: time.Now()}
	s.mu.Unlock()
	return s.save()
}

// Revoke removes a fingerprint from the trust store.
func (s *Store) Revoke(fingerprint string) error {
	s.mu.Lock()
	delete(s.set, fingerprint)
	s.mu.Unlock()
	return s.save()
}

// List returns all trusted senders.
func (s *Store) List() []Trusted {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Trusted, 0, len(s.set))
	for _, t := range s.set {
		out = append(out, t)
	}
	return out
}

func (s *Store) save() error {
	s.mu.RLock()
	list := make([]Trusted, 0, len(s.set))
	for _, t := range s.set {
		list = append(list, t)
	}
	s.mu.RUnlock()
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
