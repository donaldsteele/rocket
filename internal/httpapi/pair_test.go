package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
)

// TestPairingTrustsNewSenders: while the pairing window is open, a new
// sender's offer is accepted without asking and its key is trusted; once
// it closes, a new sender is asked again, and the paired one is not.
func TestPairingTrustsNewSenders(t *testing.T) {
	a, b, c := newTestHost(t, "alice"), newTestHost(t, "bob"), newTestHost(t, "carol")

	code, body := b.call("POST", "/api/pair", map[string]any{"seconds": 300}, true)
	var st PairingState
	if code != 200 || json.Unmarshal(body, &st) != nil || !st.Open || st.Seconds < 299 {
		t.Fatalf("open pairing: %d %s", code, body)
	}
	if code, body := b.call("GET", "/api/pair", nil, false); code != 200 || json.Unmarshal(body, &st) != nil || !st.Open {
		t.Fatalf("get pairing: %d %s", code, body)
	}
	_, _, reply := postOffer(t, b, fileOffer(t, a, b, "x.txt", 1, time.Now()))
	if !reply.Accepted {
		t.Fatalf("offer while pairing: %+v", reply)
	}
	if !b.srv.trust.IsTrusted(a.id.Fingerprint(), a.id.PublicKeyB64()) {
		t.Fatal("paired sender not trusted")
	}

	if code, body := b.call("POST", "/api/pair", map[string]any{"seconds": 0}, true); code != 200 || json.Unmarshal(body, &st) != nil || st.Open {
		t.Fatalf("stop pairing: %d %s", code, body)
	}
	// Alice stays trusted.
	if _, _, r := postOffer(t, b, fileOffer(t, a, b, "y.txt", 1, time.Now())); !r.Accepted {
		t.Fatalf("paired sender after the window: %+v", r)
	}
	// Carol is asked.
	done := make(chan protocol.OfferReply, 1)
	go func() {
		_, _, r := postOffer(t, b, fileOffer(t, c, b, "z.txt", 1, time.Now()))
		done <- r
	}()
	offers := b.waitIncoming(1)
	b.call("POST", "/api/accept", map[string]any{"id": offers[0].ID, "accept": false}, true)
	if r := <-done; r.Accepted {
		t.Fatalf("new sender after the window: %+v", r)
	}
}

func TestPairRequestValidation(t *testing.T) {
	b := newTestHost(t, "bob")
	for _, body := range []any{map[string]any{}, map[string]any{"seconds": -1}, "x"} {
		if code, _ := b.call("POST", "/api/pair", body, true); code != 400 {
			t.Errorf("POST /api/pair %v: %d", body, code)
		}
	}
	// Without the CSRF header it is refused like every other change.
	if code, _ := b.call("POST", "/api/pair", map[string]any{"seconds": 60}, false); code != 403 {
		t.Errorf("POST /api/pair without CSRF: %d", code)
	}
	if _, open := b.srv.Pairing().Open(); open {
		t.Fatal("a refused request opened pairing")
	}
}
