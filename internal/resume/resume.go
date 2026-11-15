// Package resume keeps what rocket needs to continue interrupted transfers
// (protocol v2, docs/protocol.md "Resume"): a folder of small JSON records,
// one file per transfer (Dir), and on the sending side the Outbox of sends
// that gave up after their retries, so the person can resume or discard
// them. The receiving side's records (what it kept of a broken transfer)
// live in internal/receiver on top of Dir.
package resume

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
)

// KeepFor is how long an interrupted transfer stays resumable.
const KeepFor = 24 * time.Hour

// SweepEvery is how often expired records are removed while rocket runs
// (they are also removed at start-up).
const SweepEvery = time.Hour

// ErrBadID is returned for a record ID that is not a transfer ID.
var ErrBadID = errors.New("bad transfer id")

// Dir is a folder of JSON records named <id>.json, where id is a transfer
// ID (protocol.ValidID). Writes are atomic (a temporary file renamed over
// the record). Safe for concurrent use within a process.
type Dir struct {
	path string
	mu   sync.Mutex
}

// OpenDir opens the record folder path, creating it (mode 0700) if needed.
func OpenDir(path string) (*Dir, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	return &Dir{path: path}, nil
}

// Path is the folder's location.
func (d *Dir) Path() string { return d.path }

func (d *Dir) file(id string) (string, error) {
	if !protocol.ValidID(id) {
		return "", ErrBadID
	}
	return filepath.Join(d.path, id+".json"), nil
}

// Save writes record id.
func (d *Dir) Save(id string, v any) error {
	p, err := d.file(id)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Load reads record id into v. A missing record is an error matching
// os.ErrNotExist.
func (d *Dir) Load(id string, v any) error {
	p, err := d.file(id)
	if err != nil {
		return err
	}
	d.mu.Lock()
	b, err := os.ReadFile(p)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("record %s: %w", id, err)
	}
	return nil
}

// Remove deletes record id; a missing record is not an error.
func (d *Dir) Remove(id string) error {
	p, err := d.file(id)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// IDs lists the records, sorted. Files that are not records are ignored.
func (d *Dir) IDs() ([]string, error) {
	d.mu.Lock()
	entries, err := os.ReadDir(d.path)
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && e.Type().IsRegular() && protocol.ValidID(id) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Outgoing is a send that broke for a network reason and gave up after its
// retries: what the sender needs to offer the same files to the same peer
// again (the person's Resume) or to tell the receiver to drop what it kept
// (Discard). GET /api/resumable and ResumableJSON list them.
type Outgoing struct {
	ID       string `json:"transfer_id"`
	Name     string `json:"name"` // transfer label
	PeerName string `json:"peer_name"`
	PeerFP   string `json:"peer_fp,omitempty"` // the fingerprint asked for; empty for a bare host:port
	Addr     string `json:"addr"`              // host:port it was sent to
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	// ManifestHash is the hash of the manifest offered: an offer of the same
	// files gets the receiver's resume point.
	ManifestHash string `json:"manifest_hash"`
	// Paths are the local files and folders a desktop send was given; empty
	// on Android, where the app keeps its content URIs and gives a Source
	// again.
	Paths []string `json:"paths,omitempty"`
	// Picked lists the files of a send started from the desktop's browser
	// page, as the page named them (folder-relative paths) and in its order;
	// such a send has no Paths, and its Resume needs the page to pick the
	// same files again (checked against this list).
	Picked []PickedFile `json:"picked,omitempty"`
	// ReceiverFP and RemoteID name the receiver's side of the last accepted
	// attempt, for the cancel notice a Discard sends.
	ReceiverFP  string    `json:"receiver_fp,omitempty"`
	RemoteID    string    `json:"remote_id,omitempty"`
	Error       string    `json:"error"`
	Interrupted time.Time `json:"interrupted"`
	Expires     time.Time `json:"expires"`
}

// PickedFile is one file picked in a browser page: its name (a bare name,
// or a path relative to the picked folder) and size in bytes.
type PickedFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Outbox is the sender's list of interrupted sends, in <data-dir>/outgoing.
// A record being resumed (Claim) is hidden from Get and List until the
// resumed send releases it; that send removes it once the receiver accepts
// it, so a resume that fails before then leaves the record in place.
type Outbox struct {
	dir *Dir
	// Now is the clock; tests override it.
	Now func() time.Time

	mu      sync.Mutex
	claimed map[string]bool // records a running send resumes
}

// OutboxDir is the Outbox folder inside the data folder.
const OutboxDir = "outgoing"

// OpenOutbox opens the outbox in dataDir.
func OpenOutbox(dataDir string) (*Outbox, error) {
	d, err := OpenDir(filepath.Join(dataDir, OutboxDir))
	if err != nil {
		return nil, err
	}
	return &Outbox{dir: d, Now: time.Now}, nil
}

// Put records o, setting Interrupted and Expires.
func (b *Outbox) Put(o Outgoing) error {
	o.Interrupted = b.Now()
	o.Expires = o.Interrupted.Add(KeepFor)
	return b.dir.Save(o.ID, o)
}

// Get returns record id, or false when there is none, it expired, or a
// send is resuming it (Claim).
func (b *Outbox) Get(id string) (Outgoing, bool) {
	b.mu.Lock()
	claimed := b.claimed[id]
	b.mu.Unlock()
	if claimed {
		return Outgoing{}, false
	}
	return b.load(id)
}

// Claim returns record id for a send that resumes it, and hides it (Get,
// List) until Release, so that it is resumed or discarded only once at a
// time. It reports false when there is no such live record or it is
// already claimed. The resuming send removes the record once the receiver
// accepts it, and releases it however it ends.
func (b *Outbox) Claim(id string) (Outgoing, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.claimed[id] {
		return Outgoing{}, false
	}
	o, ok := b.load(id)
	if !ok {
		return Outgoing{}, false
	}
	if b.claimed == nil {
		b.claimed = map[string]bool{}
	}
	b.claimed[id] = true
	return o, true
}

// Release ends a Claim: a record still there is listed again.
func (b *Outbox) Release(id string) {
	b.mu.Lock()
	delete(b.claimed, id)
	b.mu.Unlock()
}

func (b *Outbox) load(id string) (Outgoing, bool) {
	var o Outgoing
	if b.dir.Load(id, &o) != nil || o.ID != id || b.Now().After(o.Expires) {
		return Outgoing{}, false
	}
	return o, true
}

// Remove forgets record id.
func (b *Outbox) Remove(id string) error { return b.dir.Remove(id) }

// List returns the live records no send is resuming, most recently
// interrupted first.
func (b *Outbox) List() []Outgoing {
	ids, _ := b.dir.IDs()
	out := []Outgoing{}
	for _, id := range ids {
		if o, ok := b.Get(id); ok {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Interrupted.After(out[j].Interrupted) })
	return out
}

// Supersede forgets the interrupted sends of the manifest with hash
// manifestHash to the same peer (by fingerprint when the record has one,
// else by address) and returns how many: a later send of the same files
// that completed, whether through Resume, the same command run again or a
// fresh pick, leaves nothing to resume.
func (b *Outbox) Supersede(manifestHash, peerFP, addr string) int {
	if manifestHash == "" {
		return 0
	}
	n := 0
	for _, o := range b.List() {
		if o.ManifestHash != manifestHash {
			continue
		}
		same := o.PeerFP != "" && o.PeerFP == peerFP || o.PeerFP == "" && peerFP == "" && o.Addr == addr
		if same && b.Remove(o.ID) == nil {
			n++
		}
	}
	return n
}

// Sweep removes expired and unreadable records and returns how many.
func (b *Outbox) Sweep() int {
	ids, _ := b.dir.IDs()
	n := 0
	for _, id := range ids {
		var o Outgoing
		if err := b.dir.Load(id, &o); err == nil && o.ID == id && !b.Now().After(o.Expires) {
			continue
		}
		if b.dir.Remove(id) == nil {
			n++
		}
	}
	return n
}
