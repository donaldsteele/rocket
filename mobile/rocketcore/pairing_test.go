package rocketcore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/donaldsteele/rocket/internal/transfer"
)

// TestPhonePairing: while the phone's pairing window is open, a desktop it
// has never seen sends without a prompt and becomes trusted.
func TestPhonePairing(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	if _, err := phone.StartPairing(60); err == nil || !strings.Contains(err.Error(), "Receive files") {
		t.Fatalf("pairing without receiving: %v", err)
	}
	d := &decider{n: phone, silent: true} // a prompt would never be answered
	sink, ev := newTempSink(t, false), newEvents(t)
	addr := startReceiving(t, phone, d, sink, ev)

	js, err := phone.StartPairing(DefaultPairingSeconds)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Open    bool `json:"open"`
		Seconds int  `json:"seconds"`
	}
	if json.Unmarshal([]byte(js), &st) != nil || !st.Open || st.Seconds < DefaultPairingSeconds-1 {
		t.Fatalf("StartPairing = %s", js)
	}
	if e := ev.wait("pairing"); e.Data["open"] != true {
		t.Fatalf("pairing event %v", e.Data)
	}

	src := writeTree(t, map[string]string{"hi.txt": "hello"})
	target := transfer.Target{Addr: addr, FP: phone.Fingerprint()}
	if err := transfer.Send(context.Background(), desk.id, target, filepath.Join(src, "hi.txt"), nil); err != nil {
		t.Fatal(err)
	}
	if e := ev.wait("paired"); e.Data["fingerprint"] != desk.id.Fingerprint() || e.Data["name"] != "desk" {
		t.Fatalf("paired event %v", e.Data)
	}
	ev.wait("done")
	if d.count() != 0 {
		t.Fatal("asked while pairing")
	}
	trusted, _ := phone.TrustedJSON()
	if !strings.Contains(trusted, desk.id.Fingerprint()) {
		t.Fatalf("not trusted: %s", trusted)
	}

	phone.StopReceiver() // closes the window too
	if strings.Contains(phone.PairingJSON(), `"open":true`) {
		t.Fatalf("open after StopReceiver: %s", phone.PairingJSON())
	}
}
