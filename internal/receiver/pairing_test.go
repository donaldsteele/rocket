package receiver

import (
	"testing"
	"time"
)

func TestPairingWindow(t *testing.T) {
	var nilP *Pairing
	if _, open := nilP.Open(); open {
		t.Fatal("nil pairing is open")
	}
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var changes []bool
	p := &Pairing{now: func() time.Time { return now }, OnChange: func(_ time.Time, open bool) { changes = append(changes, open) }}
	if _, open := p.Open(); open {
		t.Fatal("zero pairing is open")
	}
	until := p.Start(5 * time.Minute)
	if !until.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("until %v", until)
	}
	if st := p.State(); !st.Open || st.Seconds != 300 {
		t.Fatalf("state %+v", st)
	}
	now = now.Add(299*time.Second + time.Millisecond)
	if st := p.State(); !st.Open || st.Seconds != 1 {
		t.Fatalf("state near the end %+v", st)
	}
	now = now.Add(time.Second)
	if _, open := p.Open(); open {
		t.Fatal("open after it ran out")
	}
	if p.Start(5 * time.Hour); !p.State().Open || p.State().Seconds != int(MaxPairing/time.Second) {
		t.Fatalf("not capped: %+v", p.State())
	}
	p.Stop()
	if _, open := p.Open(); open {
		t.Fatal("open after Stop")
	}
	p.Stop() // closing a closed window reports nothing
	if p.Start(0); p.State().Open {
		t.Fatal("Start(0) opened it")
	}
	if len(changes) != 3 || !changes[0] || !changes[1] || changes[2] {
		t.Fatalf("changes %v", changes)
	}
}

func TestPairingReportsRunningOut(t *testing.T) {
	closed := make(chan struct{}, 1)
	p := &Pairing{OnChange: func(_ time.Time, open bool) {
		if !open {
			closed <- struct{}{}
		}
	}}
	p.Start(50 * time.Millisecond)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("running out was not reported")
	}
	if _, open := p.Open(); open {
		t.Fatal("still open")
	}
}
