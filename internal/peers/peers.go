// Package peers holds the trust-on-first-use (TOFU) store: the senders this
// host has approved, each pinned to its full ed25519 public key, persisted to
// disk as JSON. It also holds the server-side drop tokens that remember
// browser guests (droptokens.go).
//
// A sender can also be approved ahead of time from the command line
// (rocket trust add), by its public key or by its fingerprint alone. An
// entry added by fingerprint has no key yet: the first offer whose key
// derives to that fingerprint is trusted, and its key is pinned from then
// on (Pin), so a fingerprint entry becomes a key entry on first use.
//
// The file is shared with other rocket processes on the same data folder
// (the command line edits it while the UI runs), so the store reloads it
// when it changes on disk and rereads it before every change it writes.
package peers

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/node"
)

// Trusted is one approved sender.
type Trusted struct {
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	// Pub is the base64 ed25519 public key, pinned. It is empty for a
	// sender approved ahead of time by fingerprint alone, until its first
	// offer.
	Pub        string    `json:"pub"`
	ApprovedAt time.Time `json:"approved_at"`
	// ByFingerprint marks a sender approved ahead of time by fingerprint
	// alone (rocket trust add): Pub is empty until its first offer pins
	// it. An entry with no key and without this mark (from an early build)
	// is dropped.
	ByFingerprint bool `json:"by_fingerprint,omitempty"`
}

// Store is the on-disk trust store, safe for concurrent use.
type Store struct {
	path string
	mu   sync.RWMutex
	set  map[string]Trusted // keyed by fingerprint
	stat fileStamp          // the file as last read or written

	saveMu sync.Mutex // serializes changes of the file
}

// fileStamp tells whether the file changed since it was read.
type fileStamp struct {
	mod  time.Time
	size int64
	ok   bool // the file existed
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size(), ok: true}
}

// Open loads the trust store from dataDir, creating an empty one if absent.
// Entries without a valid fingerprint, or whose key does not derive to
// their fingerprint, are dropped: those senders are prompted again on their
// next offer.
func Open(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "trusted.json"), set: map[string]Trusted{}}
	dropped, err := s.load()
	if err != nil {
		return nil, err
	}
	if dropped {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// load reads the file into s.set (s.mu held for writing, or s not yet
// shared). It reports whether invalid entries were left out.
func (s *Store) load() (dropped bool, err error) {
	st := stampOf(s.path)
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.set, s.stat = map[string]Trusted{}, fileStamp{}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var list []Trusted
	if err := json.Unmarshal(b, &list); err != nil {
		return false, err
	}
	set := map[string]Trusted{}
	for _, t := range list {
		if !Valid(t) {
			dropped = true
			continue
		}
		set[t.Fingerprint] = t
	}
	s.set, s.stat = set, st
	return dropped, nil
}

// Valid reports whether t can be trusted as stored: a 16-hex fingerprint,
// and a key that derives to it, or no key yet when it was approved by
// fingerprint.
func Valid(t Trusted) bool {
	if !IsFingerprint(t.Fingerprint) {
		return false
	}
	if t.Pub == "" {
		return t.ByFingerprint
	}
	pub, err := node.DecodePublicKey(t.Pub)
	return err == nil && node.FingerprintOf(pub) == t.Fingerprint
}

// IsFingerprint reports whether s is a full fingerprint: 16 lowercase hex.
func IsFingerprint(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// refresh rereads the file if another process changed it. A file that
// cannot be read or parsed leaves the store as it was.
func (s *Store) refresh() {
	s.mu.RLock()
	same := stampOf(s.path) == s.stat
	s.mu.RUnlock()
	if same {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
}

// reloadLocked rereads a changed file (s.mu held for writing), keeping the
// current entries if it cannot be read.
func (s *Store) reloadLocked() {
	if stampOf(s.path) == s.stat {
		return
	}
	old, oldStat := s.set, s.stat
	if _, err := s.load(); err != nil {
		s.set, s.stat = old, oldStat
	}
}

// IsTrusted reports whether fingerprint has been approved AND pub (base64)
// matches the key pinned for it. A known fingerprint presenting a different
// key is not trusted. A fingerprint approved without a key is trusted for
// a key that derives to it; the caller pins that key once the offer is
// verified (Pin).
func (s *Store) IsTrusted(fingerprint, pub string) bool {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.set[fingerprint]
	if !ok || pub == "" {
		return false
	}
	if t.Pub != "" {
		return t.Pub == pub
	}
	if !t.ByFingerprint {
		return false
	}
	k, err := node.DecodePublicKey(pub)
	return err == nil && node.FingerprintOf(k) == fingerprint
}

// Approve records fingerprint as trusted, pinned to pub (base64), and
// persists the store.
func (s *Store) Approve(fingerprint, name, pub string) error {
	return s.change(func(set map[string]Trusted) {
		set[fingerprint] = Trusted{Fingerprint: fingerprint, Name: name, Pub: pub, ApprovedAt: time.Now()}
	})
}

// ErrKeyMismatch: the public key given does not derive to the fingerprint.
var ErrKeyMismatch = errors.New("the public key does not match the fingerprint")

// Add approves a sender ahead of time: by its public key (base64; the
// fingerprint is derived from it, and must equal fingerprint if that is
// given too), or by its fingerprint alone when pub is empty, in which case
// its key is pinned on its first offer. Adding by fingerprint a sender
// that is already pinned keeps its key. It returns the entry stored.
func (s *Store) Add(fingerprint, name, pub string) (Trusted, error) {
	if pub != "" {
		k, err := node.DecodePublicKey(pub)
		if err != nil {
			return Trusted{}, err
		}
		fp := node.FingerprintOf(k)
		if fingerprint != "" && fingerprint != fp {
			return Trusted{}, ErrKeyMismatch
		}
		fingerprint = fp
	}
	if !IsFingerprint(fingerprint) {
		return Trusted{}, errors.New("not a fingerprint (16 hex characters) or a public key")
	}
	var out Trusted
	err := s.change(func(set map[string]Trusted) {
		t := Trusted{Fingerprint: fingerprint, Name: name, Pub: pub, ApprovedAt: time.Now(), ByFingerprint: pub == ""}
		if old, ok := set[fingerprint]; ok {
			if pub == "" && old.Pub != "" {
				t.Pub, t.ByFingerprint = old.Pub, false
			}
			if name == "" {
				t.Name = old.Name
			}
		}
		set[fingerprint], out = t, t
	})
	return out, err
}

// Pin stores pub as the key of a sender approved by fingerprint alone, and
// name as its name if it was added without one. It does nothing when the
// sender is unknown, already has a key, or pub does not derive to
// fingerprint.
func (s *Store) Pin(fingerprint, name, pub string) error {
	k, err := node.DecodePublicKey(pub)
	if err != nil || node.FingerprintOf(k) != fingerprint {
		return nil
	}
	if t, ok := s.Get(fingerprint); !ok || t.Pub != "" || !t.ByFingerprint {
		return nil
	}
	return s.change(func(set map[string]Trusted) {
		if t, ok := set[fingerprint]; ok && t.Pub == "" && t.ByFingerprint {
			t.Pub, t.ByFingerprint = pub, false
			if t.Name == "" {
				t.Name = name
			}
			set[fingerprint] = t
		}
	})
}

// Revoke removes a fingerprint from the trust store.
func (s *Store) Revoke(fingerprint string) error {
	return s.change(func(set map[string]Trusted) { delete(set, fingerprint) })
}

// change rereads the file if another process changed it, applies fn and
// writes the result, so that a change made meanwhile elsewhere is kept.
func (s *Store) change(fn func(map[string]Trusted)) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	s.reloadLocked()
	fn(s.set)
	list := s.listLocked()
	s.mu.Unlock()
	return s.write(list)
}

// Get returns the trusted sender with this fingerprint, if any.
func (s *Store) Get(fingerprint string) (Trusted, bool) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.set[fingerprint]
	return t, ok
}

// List returns all trusted senders.
func (s *Store) List() []Trusted {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLocked()
}

func (s *Store) listLocked() []Trusted {
	out := make([]Trusted, 0, len(s.set))
	for _, t := range s.set {
		out = append(out, t)
	}
	return out
}

// save writes the store as it is (Open, after dropping entries).
func (s *Store) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	list := s.listLocked()
	s.mu.RUnlock()
	return s.write(list)
}

// write writes list to the file and remembers its stamp (s.saveMu held).
func (s *Store) write(list []Trusted) error {
	if err := writeJSONAtomic(s.path, list); err != nil {
		return err
	}
	st := stampOf(s.path)
	s.mu.Lock()
	s.stat = st
	s.mu.Unlock()
	return nil
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
