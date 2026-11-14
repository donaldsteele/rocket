// Package peers holds the trust-on-first-use (TOFU) store: the senders this
// host has approved, each pinned to its full ed25519 public key, persisted to
// disk as JSON. It also holds the server-side drop tokens that remember
// browser guests (droptokens.go).
package peers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/node"
)

// Trusted is one approved sender.
type Trusted struct {
	Fingerprint string    `json:"fingerprint"`
	Name        string    `json:"name"`
	Pub         string    `json:"pub"` // base64 ed25519 public key, pinned
	ApprovedAt  time.Time `json:"approved_at"`
}

// Store is the on-disk trust store, safe for concurrent use.
type Store struct {
	path string
	mu   sync.RWMutex
	set  map[string]Trusted // keyed by fingerprint

	saveMu sync.Mutex // serializes writes of the file
}

// Open loads the trust store from dataDir, creating an empty one if absent.
// Entries without a pinned public key (written by pre-1.0 builds), or whose
// fingerprint does not derive from their key, are dropped: those senders are
// prompted again on their next offer.
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
	dropped := false
	for _, t := range list {
		pub, err := node.DecodePublicKey(t.Pub)
		if err != nil || node.FingerprintOf(pub) != t.Fingerprint {
			dropped = true
			continue
		}
		s.set[t.Fingerprint] = t
	}
	if dropped {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// IsTrusted reports whether fingerprint has been approved AND pub (base64)
// matches the key pinned for it. A known fingerprint presenting a different
// key is not trusted.
func (s *Store) IsTrusted(fingerprint, pub string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.set[fingerprint]
	return ok && pub != "" && t.Pub == pub
}

// Approve records fingerprint as trusted, pinned to pub (base64), and
// persists the store.
func (s *Store) Approve(fingerprint, name, pub string) error {
	s.mu.Lock()
	s.set[fingerprint] = Trusted{Fingerprint: fingerprint, Name: name, Pub: pub, ApprovedAt: time.Now()}
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
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	return writeJSONAtomic(s.path, s.List())
}

// writeJSONAtomic writes v as indented JSON to path via a temp file and
// rename, mode 0600.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
