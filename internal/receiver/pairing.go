package receiver

import (
	"sync"
	"time"
)

// MaxPairing is the longest pairing window that can be opened at once.
const MaxPairing = time.Hour

// DefaultPairing is the pairing window the UIs open.
const DefaultPairing = 5 * time.Minute

// Pairing is a window, opened on purpose by the person at the receiving
// device, in which an offer from a rocket sender that is not yet trusted
// is accepted without asking and its key is trusted (pinned) from then on,
// as if the person had ticked "Always accept". It lets a headless receiver
// (rocket recv on a Raspberry Pi) learn its senders without anyone at a
// screen. Browser drops have no key and are never paired. The zero value
// is closed; a nil *Pairing is always closed.
type Pairing struct {
	mu    sync.Mutex
	until time.Time
	timer *time.Timer      // reports the window running out
	now   func() time.Time // tests
	// OnChange, if set, runs after the window opens or closes (Start,
	// Stop, or running out), with the new state.
	OnChange func(until time.Time, open bool)
}

func (p *Pairing) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Start opens the window for d (capped at MaxPairing; d <= 0 closes it)
// and returns when it closes.
func (p *Pairing) Start(d time.Duration) time.Time {
	if d <= 0 {
		p.Stop()
		return time.Time{}
	}
	if d > MaxPairing {
		d = MaxPairing
	}
	p.mu.Lock()
	p.until = p.clock().Add(d)
	until, cb := p.until, p.OnChange
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(d, func() { p.ranOut(until) })
	p.mu.Unlock()
	if cb != nil {
		cb(until, true)
	}
	return until
}

// Stop closes the window.
func (p *Pairing) Stop() {
	p.mu.Lock()
	was := !p.until.IsZero() && p.clock().Before(p.until)
	p.until = time.Time{}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	cb := p.OnChange
	p.mu.Unlock()
	if cb != nil && was {
		cb(time.Time{}, false)
	}
}

// ranOut reports the window that was to close at until, unless it was
// extended or stopped meanwhile.
func (p *Pairing) ranOut(until time.Time) {
	p.mu.Lock()
	same := p.until.Equal(until)
	if same {
		p.until, p.timer = time.Time{}, nil
	}
	cb := p.OnChange
	p.mu.Unlock()
	if same && cb != nil {
		cb(time.Time{}, false)
	}
}

// Open reports whether the window is open, and until when.
func (p *Pairing) Open() (until time.Time, open bool) {
	if p == nil {
		return time.Time{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.until.IsZero() || !p.clock().Before(p.until) {
		return time.Time{}, false
	}
	return p.until, true
}

// PairingState is what the UIs show: whether the window is open, until
// when, and the seconds left (rounded up).
type PairingState struct {
	Open    bool      `json:"open"`
	Until   time.Time `json:"until,omitzero"`
	Seconds int       `json:"seconds"`
}

// State reports the window for a UI.
func (p *Pairing) State() PairingState {
	until, open := p.Open()
	if !open {
		return PairingState{}
	}
	left := time.Until(until)
	if p.now != nil {
		left = until.Sub(p.now())
	}
	return PairingState{Open: true, Until: until, Seconds: int((left + time.Second - 1) / time.Second)}
}
