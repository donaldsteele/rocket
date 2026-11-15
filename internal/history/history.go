// Package history keeps a log of finished transfers in the data folder as
// JSON Lines (history.jsonl), one entry per transfer, newest last. The file is
// capped at Cap entries: it is compacted when opened and whenever it has grown
// well past the cap. Each Add appends one line and closes the file again, so a
// second rocket process on the same data folder (rocket mcp, rocket send) can
// add entries too.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Cap is the number of entries kept.
const Cap = 1000

// FileName is the history file inside the data folder.
const FileName = "history.jsonl"

// Directions.
const (
	DirIn  = "in"
	DirOut = "out"
)

// Result is how a transfer ended.
type Result = string

// Results.
const (
	ResultOK        Result = "ok"
	ResultDeclined  Result = "declined"
	ResultCancelled Result = "cancelled"
	ResultError     Result = "error"
	ResultTimeout   Result = "timeout"
)

// Entry is one finished transfer.
type Entry struct {
	ID       string    `json:"id"`
	Dir      string    `json:"dir"`  // "in" or "out"
	Name     string    `json:"name"` // transfer label: the first top-level file or folder
	PeerName string    `json:"peer_name"`
	PeerFP   string    `json:"peer_fp,omitempty"` // empty for browser guests and unverified host:port sends
	Files    int       `json:"files"`             // file count (folders not counted)
	Bytes    int64     `json:"bytes"`             // bytes offered (total of the manifest)
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	Result   Result    `json:"result"`
	Error    string    `json:"error,omitempty"`
	// Encrypted is true for every peer transfer (protocol 2 encrypts every
	// body) and false for browser drops and entries written before 1.1.
	Encrypted bool `json:"encrypted"`
}

// Store is the history file, safe for concurrent use within a process.
type Store struct {
	path string
	mu   sync.Mutex
	// added counts lines appended since the last compaction; once it passes
	// Cap the file is compacted again.
	added int
}

// Open opens (creating if needed) the history in dataDir and compacts it to
// the newest Cap valid entries.
func Open(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, FileName)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.compact(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path is the history file's location.
func (s *Store) Path() string { return s.path }

// Add appends e, filling Ended when unset.
func (s *Store) Add(e Entry) error {
	if e.Ended.IsZero() {
		e.Ended = time.Now()
	}
	if e.Started.IsZero() {
		e.Started = e.Ended
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	s.added++
	if s.added > Cap {
		return s.compact()
	}
	return nil
}

// List returns up to limit entries, newest first. limit <= 0 or above Cap
// means Cap.
func (s *Store) List(limit int) ([]Entry, error) {
	if limit <= 0 || limit > Cap {
		limit = Cap
	}
	s.mu.Lock()
	all, _, err := s.read()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, min(limit, len(all)))
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out, nil
}

// Clear removes every entry.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.added = 0
	return os.WriteFile(s.path, nil, 0o600)
}

// read parses the file. Lines that are not valid entries (a torn write, a
// hand edit) are skipped and reported through dirty.
func (s *Store) read() (entries []Entry, dirty bool, err error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil || e.ID == "" {
			dirty = true
			continue
		}
		entries = append(entries, e)
	}
	if sc.Err() != nil {
		dirty = true
	}
	return entries, dirty, nil
}

// compact rewrites the file with the newest Cap valid entries when it holds
// more than that or any invalid line. Called with s.mu held.
func (s *Store) compact() error {
	s.added = 0
	all, dirty, err := s.read()
	if err != nil {
		return err
	}
	if len(all) <= Cap && !dirty {
		return nil
	}
	if len(all) > Cap {
		all = all[len(all)-Cap:]
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range all {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
