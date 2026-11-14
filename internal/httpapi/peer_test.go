package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
)

// postOffer sends an offer to h's /api/offer over the network.
func postOffer(t *testing.T, h *testHost, o *protocol.Offer) (int, string, protocol.OfferReply) {
	t.Helper()
	b, _ := json.Marshal(o)
	resp, err := http.Post(h.ts.URL+"/api/offer", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var reply protocol.OfferReply
	if resp.StatusCode == 200 {
		_ = json.Unmarshal(body, &reply)
	}
	return resp.StatusCode, string(body), reply
}

// trustedPair returns a sender A already trusted by receiver B, so B's
// /api/offer answers without a prompt.
func trustedPair(t *testing.T) (*testHost, *testHost) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	if err := b.srv.trust.Approve(a.id.Fingerprint(), "alice", a.id.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func fileOffer(t *testing.T, a, b *testHost, name string, size int64, ts time.Time) *protocol.Offer {
	t.Helper()
	o, err := protocol.NewOffer(a.id, a.id.Name, b.id.Fingerprint(), protocol.FileManifest(name, size, 0o644), ts)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOfferForgedFingerprintRejected(t *testing.T) {
	a, b := trustedPair(t)
	mallory := newTestHost(t, "mallory")
	// Mallory claims Alice's (trusted) fingerprint with her own key.
	o := fileOffer(t, mallory, b, "x.txt", 1, time.Now())
	o.SenderFP = a.id.Fingerprint()
	_ = o.SignWith(mallory.id)
	code, body, _ := postOffer(t, b, o)
	if code != 400 || !strings.Contains(body, protocol.ErrFingerprint.Error()) {
		t.Fatalf("forged fp: %d %s", code, body)
	}
	// Mallory presents Alice's real key too, but cannot sign as Alice.
	o.SenderPub = a.id.PublicKeyB64()
	code, body, _ = postOffer(t, b, o)
	if code != 400 || !strings.Contains(body, protocol.ErrSignature.Error()) {
		t.Fatalf("stolen pub: %d %s", code, body)
	}
}

func TestOfferWrongReceiverRejected(t *testing.T) {
	a, b := trustedPair(t)
	c := newTestHost(t, "carol")
	o := fileOffer(t, a, c, "x.txt", 1, time.Now()) // addressed to carol
	code, body, _ := postOffer(t, b, o)
	if code != 400 || !strings.Contains(body, protocol.ErrWrongReceiver.Error()) {
		t.Fatalf("wrong receiver: %d %s", code, body)
	}
}

func TestOfferStaleRejected(t *testing.T) {
	a, b := trustedPair(t)
	for _, d := range []time.Duration{-10 * time.Minute, 10 * time.Minute} {
		code, body, _ := postOffer(t, b, fileOffer(t, a, b, "x.txt", 1, time.Now().Add(d)))
		if code != 400 || !strings.Contains(body, protocol.ErrStale.Error()) {
			t.Fatalf("stale %s: %d %s", d, code, body)
		}
	}
}

func TestOfferReplayRejected(t *testing.T) {
	a, b := trustedPair(t)
	o := fileOffer(t, a, b, "x.txt", 1, time.Now())
	code, body, reply := postOffer(t, b, o)
	if code != 200 || !reply.Accepted || reply.Token == "" || reply.TransferID == "" {
		t.Fatalf("first offer from trusted sender: %d %s", code, body)
	}
	code, body, _ = postOffer(t, b, o)
	if code != 400 || !strings.Contains(body, protocol.ErrReplay.Error()) {
		t.Fatalf("replay: %d %s", code, body)
	}
}

func TestOfferTraversalRejected(t *testing.T) {
	a, b := trustedPair(t)
	manifests := []protocol.Manifest{
		{Kind: protocol.KindFile, Entries: []protocol.Entry{{Path: "..", Size: 1}}, Total: 1},
		{Kind: protocol.KindFile, Entries: []protocol.Entry{{Path: `..\..\evil.exe`, Size: 1}}, Total: 1},
		{Kind: protocol.KindMulti, Entries: []protocol.Entry{{Path: "../../evil", Size: 1}}, Total: 1},
		{Kind: protocol.KindMulti, Entries: []protocol.Entry{{Path: "/etc/passwd", Size: 1}}, Total: 1},
		{Kind: protocol.KindMulti, Entries: []protocol.Entry{{Path: "C:/Windows/evil", Size: 1}}, Total: 1},
	}
	for _, m := range manifests {
		o, err := protocol.NewOffer(a.id, "alice", b.id.Fingerprint(), m, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		code, body, _ := postOffer(t, b, o)
		if code != 400 || !strings.Contains(body, protocol.ErrManifest.Error()) {
			t.Errorf("traversal %q: %d %s", m.Entries[0].Path, code, body)
		}
	}
	if len(b.downloads()) != 0 {
		t.Fatalf("files written: %v", b.downloads())
	}
}

func TestOfferTrustedFPWithOtherKeyPrompts(t *testing.T) {
	// The store pins Alice's fingerprint to a different key (for example a
	// tampered trusted.json): Alice's offer must not skip the prompt.
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	other := newTestHost(t, "other")
	_ = b.srv.trust.Approve(a.id.Fingerprint(), "alice", other.id.PublicKeyB64())
	done := make(chan protocol.OfferReply, 1)
	go func() {
		_, _, reply := postOffer(t, b, fileOffer(t, a, b, "x.txt", 1, time.Now()))
		done <- reply
	}()
	offers := b.waitIncoming(1)
	b.call("POST", "/api/accept", map[string]any{"id": offers[0].ID, "accept": false}, true)
	if r := <-done; r.Accepted || r.Reason != "declined" {
		t.Fatalf("reply %+v", r)
	}
}

func TestOfferBodyLimit(t *testing.T) {
	_, b := trustedPair(t)
	big := `{"sender_name":"` + strings.Repeat("a", protocol.MaxOfferBytes) + `"}`
	resp, err := http.Post(b.ts.URL+"/api/offer", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("1 MiB+ offer: %d", resp.StatusCode)
	}
}

func TestRecvContentLengthMustMatch(t *testing.T) {
	a, b := trustedPair(t)
	_, _, reply := postOffer(t, b, fileOffer(t, a, b, "x.txt", 10, time.Now()))
	if !reply.Accepted {
		t.Fatal("not accepted")
	}
	resp, err := http.Post(b.ts.URL+"/api/recv?token="+reply.Token, "application/octet-stream", strings.NewReader("short"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("short Content-Length: %d", resp.StatusCode)
	}
	// One-shot: the token is gone even though the upload failed.
	resp, err = http.Post(b.ts.URL+"/api/recv?token="+reply.Token, "application/octet-stream", strings.NewReader("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("reused token: %d", resp.StatusCode)
	}
	if len(b.downloads()) != 0 {
		t.Fatalf("partials left: %v", b.downloads())
	}
}

func TestRecvTokenBoundToIP(t *testing.T) {
	a, b := trustedPair(t)
	_, _, reply := postOffer(t, b, fileOffer(t, a, b, "x.txt", 1, time.Now()))
	req := httptest.NewRequest("POST", "/api/recv?token="+reply.Token, strings.NewReader("x"))
	req.RemoteAddr = "192.168.1.66:4000"
	rec := httptest.NewRecorder()
	b.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("token from another IP: %d", rec.Code)
	}
}

func TestMultiOfferNotYetSupported(t *testing.T) {
	a, b := trustedPair(t)
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{{Path: "d/a", Size: 1}}, Total: 1}
	o, _ := protocol.NewOffer(a.id, "alice", b.id.Fingerprint(), m, time.Now())
	if code, body, _ := postOffer(t, b, o); code != 400 {
		t.Fatalf("multi offer: %d %s", code, body)
	}
}
