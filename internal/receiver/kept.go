package receiver

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/resume"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// KeptDir is the folder of kept transfers inside the data folder.
const KeptDir = "resume"

// ResumableSink is a Sink that can keep what an accepted transfer stored
// when it breaks for a network reason, and continue it later. Its Outputs
// implement Keeper. FSSink is one.
type ResumableSink interface {
	Sink
	// Resume continues transfer id of manifest m from state, which an
	// Output's Keep returned: entries before Kept.Next are stored, and the
	// entry in flight continues where it stopped.
	Resume(id string, m protocol.Manifest, state json.RawMessage, progress transfer.ProgressFn) (Output, error)
	// Check reports whether what state describes is still there as it was.
	Check(state json.RawMessage) error
	// Discard removes what state describes.
	Discard(state json.RawMessage)
}

// Keeper is an Output that can end without removing what it stored.
type Keeper interface {
	// Keep ends a failed output, leaving what it stored, and describes it.
	Keep() (KeptOutput, error)
}

// KeptOutput is what Keeper.Keep returns.
type KeptOutput struct {
	Next  int                  // entries before Next are complete
	Point protocol.ResumePoint // where the sender may continue
	Done  int64                // bytes stored
	State json.RawMessage      // the sink's own description
}

// Kept is a transfer kept for a resume: the record in <data-dir>/resume.
type Kept struct {
	ID           string            `json:"id"`       // transfer ID of the attempt that first kept it
	Attempts     []string          `json:"attempts"` // transfer IDs of the later attempts offered to continue it
	SenderName   string            `json:"sender_name"`
	SenderFP     string            `json:"sender_fp"`
	SenderPub    string            `json:"sender_pub"` // the key that sent it; only that key may continue it
	ManifestHash string            `json:"manifest_hash"`
	Manifest     protocol.Manifest `json:"manifest"`
	// Point is where the next attempt may start; Done the bytes stored.
	Point    protocol.ResumePoint `json:"point"`
	Next     int                  `json:"next"`
	Done     int64                `json:"done"`
	Location string               `json:"location"`
	Sink     json.RawMessage      `json:"sink"`
	KeptAt   time.Time            `json:"kept_at"`
	Expires  time.Time            `json:"expires"`
}

// maxAttempts bounds Kept.Attempts.
const maxAttempts = 32

// KeptView is how a kept transfer is listed (GET /api/resumable
// "incoming", ResumableJSON).
type KeptView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"` // transfer label
	SenderName string    `json:"sender_name"`
	SenderFP   string    `json:"sender_fp"`
	Files      int       `json:"files"`
	Total      int64     `json:"total"`
	Done       int64     `json:"done"`
	Location   string    `json:"location"`
	KeptAt     time.Time `json:"kept_at"`
	Expires    time.Time `json:"expires"`
}

func (k *Kept) view() KeptView {
	return KeptView{
		ID: k.ID, Name: transfer.Label(k.Manifest), SenderName: k.SenderName, SenderFP: k.SenderFP,
		Files: transfer.Files(k.Manifest), Total: k.Manifest.Total, Done: k.Done, Location: k.Location,
		KeptAt: k.KeptAt, Expires: k.Expires,
	}
}

// KeptStore holds the kept transfers of a receiver. Safe for concurrent
// use.
type KeptStore struct {
	dir  *resume.Dir
	sink ResumableSink
	// Now is the clock; tests override it.
	Now func() time.Time

	mu   sync.Mutex
	busy map[string]bool // kept IDs a running transfer continues
}

// OpenKept opens the kept transfers in dataDir/resume, stored through sink.
func OpenKept(dataDir string, sink ResumableSink) (*KeptStore, error) {
	d, err := resume.OpenDir(filepath.Join(dataDir, KeptDir))
	if err != nil {
		return nil, err
	}
	return &KeptStore{dir: d, sink: sink, Now: time.Now, busy: map[string]bool{}}, nil
}

// load returns record id when it is readable and not expired.
func (ks *KeptStore) load(id string) (*Kept, bool) {
	var k Kept
	if ks.dir.Load(id, &k) != nil || k.ID != id || ks.Now().After(k.Expires) {
		return nil, false
	}
	return &k, true
}

// all returns the live records.
func (ks *KeptStore) all() []*Kept {
	ids, _ := ks.dir.IDs()
	var out []*Kept
	for _, id := range ids {
		if k, ok := ks.load(id); ok {
			out = append(out, k)
		}
	}
	return out
}

// save writes k, renewing its expiry.
func (ks *KeptStore) save(k *Kept) error {
	k.KeptAt = ks.Now()
	k.Expires = k.KeptAt.Add(resume.KeepFor)
	return ks.dir.Save(k.ID, k)
}

// Find returns the live kept transfer from the sender with key senderPub
// (and fingerprint senderFP) of the manifest with hash manifestHash, if
// one exists, is not being continued, and is still intact on disk (one
// that is not is discarded).
func (ks *KeptStore) Find(senderFP, senderPub, manifestHash string) *Kept {
	for _, k := range ks.all() {
		if k.SenderFP != senderFP || k.SenderPub != senderPub || k.ManifestHash != manifestHash {
			continue
		}
		ks.mu.Lock()
		busy := ks.busy[k.ID]
		ks.mu.Unlock()
		if busy {
			continue
		}
		if ks.sink.Check(k.Sink) != nil {
			ks.Discard(k.ID)
			continue
		}
		return k
	}
	return nil
}

// Offered records that transfer tid was offered to continue kept transfer
// id, so that a cancel notice for tid finds it.
func (ks *KeptStore) Offered(id, tid string) {
	k, ok := ks.load(id)
	if !ok {
		return
	}
	k.Attempts = append(k.Attempts, tid)
	if len(k.Attempts) > maxAttempts {
		k.Attempts = k.Attempts[len(k.Attempts)-maxAttempts:]
	}
	_ = ks.dir.Save(k.ID, k)
}

// ByTransfer returns the kept transfer that transfer tid made or was
// offered to continue.
func (ks *KeptStore) ByTransfer(tid string) *Kept {
	for _, k := range ks.all() {
		if k.ID == tid {
			return k
		}
		for _, a := range k.Attempts {
			if a == tid {
				return k
			}
		}
	}
	return nil
}

// errKeptGone is returned by Claim.
var errKeptGone = errors.New("the kept part of this transfer is gone; send it again")

// Claim marks kept transfer id as being continued from point p and returns
// it. It fails when the record is gone, expired, already being continued,
// or no longer at p.
func (ks *KeptStore) Claim(id string, p protocol.ResumePoint) (*Kept, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	k, ok := ks.load(id)
	if !ok || ks.busy[id] || k.Point != p {
		return nil, errKeptGone
	}
	ks.busy[id] = true
	return k, nil
}

// Release ends a Claim.
func (ks *KeptStore) Release(id string) {
	ks.mu.Lock()
	delete(ks.busy, id)
	ks.mu.Unlock()
}

// Remove forgets record id without touching what it kept (the transfer
// was completed, or its files were removed otherwise).
func (ks *KeptStore) Remove(id string) { _ = ks.dir.Remove(id) }

// Discard removes kept transfer id and what it kept, unless a transfer is
// continuing it. It reports whether there was such a record.
func (ks *KeptStore) Discard(id string) bool {
	ks.mu.Lock()
	busy := ks.busy[id]
	ks.mu.Unlock()
	var k Kept
	if busy || ks.dir.Load(id, &k) != nil {
		return false
	}
	ks.sink.Discard(k.Sink)
	_ = ks.dir.Remove(id)
	return true
}

// Keep records what out kept of transfer tid (from the upload up), as a
// new record or, when it continued kept transfer prev, as prev renewed.
func (ks *KeptStore) Keep(prev *Kept, up *Upload, ko KeptOutput, location string) (*Kept, error) {
	k := &Kept{
		ID: up.TransferID, SenderName: up.SenderName, SenderFP: up.SenderFP, SenderPub: up.SenderPub,
		Manifest: up.Manifest,
	}
	if prev != nil {
		k.ID, k.Attempts = prev.ID, prev.Attempts
	}
	mh, err := up.Manifest.Hash()
	if err != nil {
		return nil, err
	}
	k.ManifestHash = mh
	k.Point, k.Next, k.Done, k.Sink, k.Location = ko.Point, ko.Next, ko.Done, ko.State, location
	if err := ks.save(k); err != nil {
		ks.sink.Discard(ko.State)
		return nil, err
	}
	return k, nil
}

// List returns the kept transfers, most recently kept first.
func (ks *KeptStore) List() []KeptView {
	out := []KeptView{}
	for _, k := range ks.all() {
		out = append(out, k.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KeptAt.After(out[j].KeptAt) })
	return out
}

// Sweep removes expired kept transfers (with what they kept) and
// unreadable records, and returns the IDs it removed.
func (ks *KeptStore) Sweep() []string {
	ids, _ := ks.dir.IDs()
	var gone []string
	for _, id := range ids {
		ks.mu.Lock()
		busy := ks.busy[id]
		ks.mu.Unlock()
		if busy {
			continue
		}
		var k Kept
		err := ks.dir.Load(id, &k)
		switch {
		case err != nil && errors.Is(err, os.ErrNotExist):
			continue
		case err == nil && k.ID == id && !ks.Now().After(k.Expires):
			continue
		case err == nil:
			ks.sink.Discard(k.Sink)
		}
		if ks.dir.Remove(id) == nil {
			gone = append(gone, id)
		}
	}
	return gone
}
