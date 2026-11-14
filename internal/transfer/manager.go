package transfer

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/history"
)

// ErrCancelled is wrapped when a transfer was cancelled, here or (reported by
// the receiver with 410 Gone) at the other end.
var ErrCancelled = errors.New("transfer cancelled")

// ErrTimedOut is wrapped when the receiver did not answer the offer in time.
var ErrTimedOut = errors.New("the receiver did not answer in time")

// Transfer states.
const (
	StateWaiting = "waiting" // offer sent, waiting for the receiver's decision
	StateActive  = "active"  // bytes moving
)

// Info is the live view of one transfer, as GET /api/transfers shows it.
type Info struct {
	ID        string    `json:"transfer_id"`
	Dir       string    `json:"dir"`  // "in" or "out"
	Name      string    `json:"name"` // transfer label
	PeerName  string    `json:"peer_name"`
	PeerFP    string    `json:"peer_fp,omitempty"`
	Files     int       `json:"files"`
	Total     int64     `json:"total"`
	Sent      int64     `json:"sent"`
	File      string    `json:"file,omitempty"`
	FileIndex int       `json:"file_index"`
	State     string    `json:"state"`
	Started   time.Time `json:"started"`
}

// Manager tracks the transfers in flight in both directions: their state and
// progress, and the cancel function that stops each. One mutex guards it all.
type Manager struct {
	mu sync.Mutex
	m  map[string]*managed
}

type managed struct {
	info      Info
	cancel    func()
	cancelled bool
}

// NewManager returns an empty manager.
func NewManager() *Manager { return &Manager{m: map[string]*managed{}} }

// Start registers a transfer and returns a context, derived from parent,
// that Cancel(info.ID) cancels. abort, if not nil, is also called on Cancel:
// a receiver uses it to unblock a pending body read. Started and State are
// filled when empty. Call Finish when the transfer ends, whatever the
// outcome.
func (mg *Manager) Start(parent context.Context, info Info, abort func()) context.Context {
	ctx, cancel := context.WithCancel(parent)
	if info.Started.IsZero() {
		info.Started = time.Now()
	}
	if info.State == "" {
		info.State = StateActive
	}
	stop := cancel
	if abort != nil {
		stop = func() { cancel(); abort() }
	}
	mg.mu.Lock()
	mg.m[info.ID] = &managed{info: info, cancel: stop}
	mg.mu.Unlock()
	return ctx
}

// Progress records a progress report; the first one marks the transfer
// active.
func (mg *Manager) Progress(id string, p Progress) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if t := mg.m[id]; t != nil {
		t.info.Sent, t.info.Total, t.info.File, t.info.FileIndex = p.Sent, p.Total, p.File, p.FileIndex
		if p.Files > 0 {
			t.info.Files = p.Files
		}
		t.info.State = StateActive
	}
}

// Cancel stops a transfer. It reports whether the transfer was known.
func (mg *Manager) Cancel(id string) bool {
	mg.mu.Lock()
	t := mg.m[id]
	if t != nil {
		t.cancelled = true
	}
	mg.mu.Unlock()
	if t == nil {
		return false
	}
	t.cancel()
	return true
}

// Cancelled reports whether Cancel was called for id.
func (mg *Manager) Cancelled(id string) bool {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	t := mg.m[id]
	return t != nil && t.cancelled
}

// Finish removes a transfer, releases its context and returns its last view
// and whether it was cancelled here.
func (mg *Manager) Finish(id string) (info Info, cancelled bool) {
	mg.mu.Lock()
	t := mg.m[id]
	delete(mg.m, id)
	mg.mu.Unlock()
	if t == nil {
		return Info{ID: id}, false
	}
	t.cancel()
	return t.info, t.cancelled
}

// List returns the transfers in flight, oldest first.
func (mg *Manager) List() []Info {
	mg.mu.Lock()
	out := make([]Info, 0, len(mg.m))
	for _, t := range mg.m {
		out = append(out, t.info)
	}
	mg.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.Before(out[j].Started)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Len is the number of transfers in flight.
func (mg *Manager) Len() int {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return len(mg.m)
}

// Outcome classifies how a transfer ended for the history: ok, declined,
// timeout, cancelled or error.
func Outcome(err error) history.Result {
	switch {
	case err == nil:
		return history.ResultOK
	case errors.Is(err, ErrCancelled), errors.Is(err, context.Canceled):
		return history.ResultCancelled
	case errors.Is(err, ErrTimedOut), errors.Is(err, context.DeadlineExceeded):
		return history.ResultTimeout
	case errors.Is(err, ErrDeclined):
		return history.ResultDeclined
	default:
		return history.ResultError
	}
}
