package transfer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/protocol/securestream"
)

// ErrCancelled is wrapped when a transfer was cancelled, here or (reported by
// the receiver with 410 Gone) at the other end.
var ErrCancelled = errors.New("transfer cancelled")

// ErrTimedOut is wrapped when the receiver did not answer the offer in time.
var ErrTimedOut = errors.New("the receiver did not answer in time")

// ErrInterrupted is wrapped when an accepted transfer broke for a network
// reason and is left resumable: on the sender after its last retry, on the
// receiver when it kept what it had stored.
var ErrInterrupted = errors.New("transfer interrupted")

// ErrConnLost is wrapped when the connection carrying a body went away (the
// request ended without a cancel on this side).
var ErrConnLost = errors.New("connection lost")

// IsNetwork reports whether err is a network failure: the connection was
// refused, reset, closed, timed out or lost, or a body was cut short. A
// cancel, a refusal by the other side, a bad body and a local file error
// are not.
func IsNetwork(err error) bool {
	switch {
	case err == nil, errors.Is(err, ErrCancelled), errors.Is(err, context.Canceled):
		return false
	case errors.Is(err, ErrInterrupted), errors.Is(err, ErrConnLost), errors.Is(err, securestream.ErrTruncated),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.EPIPE), errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.ENETDOWN):
		return true
	}
	// *url.Error wraps whatever a client request hit, so look inside it.
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() && !errors.Is(err, context.DeadlineExceeded) {
			return true
		}
		err = ue.Err
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return true
	}
	var to interface{ Timeout() bool }
	return errors.As(err, &to) && to.Timeout() && !errors.Is(err, context.DeadlineExceeded)
}

// Transfer states.
const (
	StateWaiting = "waiting" // offer sent, waiting for the receiver's decision
	StateActive  = "active"  // bytes moving
	// StateReconnecting: the connection broke; the sender waits to offer
	// the transfer again (Attempt of Attempts).
	StateReconnecting = "reconnecting"
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
	Encrypted bool      `json:"encrypted"` // a peer transfer (encrypted body), not a browser drop
	// Attempt and Attempts count the sender's retries after a network
	// failure (state reconnecting: "Reconnecting 2/5"); 0 before any.
	Attempt  int `json:"attempt,omitempty"`
	Attempts int `json:"attempts,omitempty"`
}

// Manager tracks the transfers in flight in both directions: their state and
// progress, and the cancel function that stops each. One mutex guards it all.
type Manager struct {
	mu sync.Mutex
	m  map[string]*managed
}

type managed struct {
	info      Info
	cancel    context.CancelFunc
	abort     func() // nil, or what Cancel also calls (see Start)
	cancelled bool
}

// NewManager returns an empty manager.
func NewManager() *Manager { return &Manager{m: map[string]*managed{}} }

// Start registers a transfer and returns a context, derived from parent,
// that Cancel(info.ID) cancels. abort, if not nil, is also called on Cancel,
// and only then: a receiver uses it to unblock a pending body read by
// setting a past read deadline on the connection. Finish must not do that:
// after a body has been read to its end the server keeps a background read
// on the connection, and a past deadline makes that read fail, which
// cancels the connection's context and with it every later request the
// client sends on that keep-alive connection. Started and State are filled
// when empty. Call Finish when the transfer ends, whatever the outcome.
func (mg *Manager) Start(parent context.Context, info Info, abort func()) context.Context {
	ctx, cancel := context.WithCancel(parent)
	if info.Started.IsZero() {
		info.Started = time.Now()
	}
	if info.State == "" {
		info.State = StateActive
	}
	mg.mu.Lock()
	mg.m[info.ID] = &managed{info: info, cancel: cancel, abort: abort}
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
	if t.abort != nil {
		t.abort()
	}
	return true
}

// Stop stops a transfer like Cancel, except that it does not count as
// cancelled here: the other end asked for it. It reports whether the
// transfer was known.
func (mg *Manager) Stop(id string) bool {
	mg.mu.Lock()
	t := mg.m[id]
	mg.mu.Unlock()
	if t == nil {
		return false
	}
	t.cancel()
	if t.abort != nil {
		t.abort()
	}
	return true
}

// Retrying marks transfer id as reconnecting: retry attempt of attempts.
func (mg *Manager) Retrying(id string, attempt, attempts int) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if t := mg.m[id]; t != nil {
		t.info.State, t.info.Attempt, t.info.Attempts = StateReconnecting, attempt, attempts
	}
}

// Cancelled reports whether Cancel was called for id.
func (mg *Manager) Cancelled(id string) bool {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	t := mg.m[id]
	return t != nil && t.cancelled
}

// Finish removes a transfer, releases its context (without calling abort)
// and returns its last view and whether it was cancelled here.
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
// timeout, cancelled, interrupted or error.
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
	case errors.Is(err, ErrInterrupted):
		return history.ResultInterrupted
	default:
		return history.ResultError
	}
}
