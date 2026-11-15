package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/protocol/securestream"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// acceptedOffer posts a signed v2 offer for m from a to b (which trusts a)
// and returns it with the sender's ephemeral key, the verified reply and
// the body key.
func acceptedOffer(t *testing.T, a, b *testHost, m protocol.Manifest) (*protocol.Offer, protocol.OfferReply, []byte) {
	t.Helper()
	o, eph, err := protocol.NewOffer(a.id, "alice", b.id.Fingerprint(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, txt, reply := postOffer(t, b, o)
	if code != 200 || !reply.Accepted {
		t.Fatalf("offer: %d %s", code, txt)
	}
	sb, _ := protocol.SigningBytes(o)
	if err := protocol.VerifyReply(b.id.Pub, sb, &reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	key, err := protocol.BodyKey(eph, reply.Eph, o, reply.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	return o, reply, key
}

func postBody(t *testing.T, h *testHost, token string, body []byte) (int, string) {
	t.Helper()
	resp, err := http.Post(h.ts.URL+"/api/recv?token="+token, transfer.BodyType, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(msg)
}

func plainBody(s transfer.Start, content string) []byte {
	h := s.Header()
	return append(h[:], content...)
}

func seal(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	b, err := securestream.Seal(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRecvV2Body sends hand-made bodies for an accepted manifest straight to
// /api/recv: the good one is stored, every bad one gets 400 and leaves
// nothing in or beside the download folder.
func TestRecvV2Body(t *testing.T) {
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d", Mode: protocol.ModeDir | 0o755}, {Path: "d/a.txt", Size: 5, Mode: 0o644}, {Path: "d/b.txt", Size: 3, Mode: 0o644},
	}, Total: 8}
	a, b := trustedPair(t)
	good := plainBody(transfer.Start{}, "hellobee")

	type mk func(key []byte) []byte
	frameHdr := func(n uint32) []byte { var h [4]byte; binary.BigEndian.PutUint32(h[:], n); return h[:] }
	cases := map[string]mk{
		"wrong key": func(key []byte) []byte {
			k := bytes.Clone(key)
			k[0] ^= 1
			return seal(t, k, good)
		},
		"tampered": func(key []byte) []byte {
			s := seal(t, key, good)
			s[10] ^= 0x40
			return s
		},
		"truncated": func(key []byte) []byte { s := seal(t, key, good); return s[:len(s)-1] },
		"trailing":  func(key []byte) []byte { return append(seal(t, key, good), 0) },
		"two streams": func(key []byte) []byte {
			return append(seal(t, key, plainBody(transfer.Start{}, "hello")), seal(t, key, []byte("bee"))...)
		},
		"oversized frame":   func([]byte) []byte { return append(frameHdr(securestream.MaxCipher+1), make([]byte, 64)...) },
		"start not offered": func(key []byte) []byte { return seal(t, key, plainBody(transfer.Start{Entry: 2, Offset: 1}, "ee")) },
		"short":             func(key []byte) []byte { return seal(t, key, plainBody(transfer.Start{}, "hellobe")) },
		"long":              func(key []byte) []byte { return seal(t, key, plainBody(transfer.Start{}, "hellobee!")) },
		"plaintext":         func([]byte) []byte { return good },
	}
	for name, body := range cases {
		_, reply, key := acceptedOffer(t, a, b, m)
		if code, msg := postBody(t, b, reply.Token, body(key)); code != 400 {
			t.Errorf("%s: %d %s", name, code, msg)
		}
		if d := b.downloads(); len(d) != 0 {
			t.Fatalf("%s: left in downloads: %v", name, d)
		}
	}
	parent, _ := os.ReadDir(filepath.Dir(b.cfg.DownloadDir))
	for _, e := range parent {
		if e.Name() != "data" && e.Name() != "downloads" {
			t.Fatalf("written beside downloads: %s", e.Name())
		}
	}

	// The good body is stored; replaying it, even with the token, is refused.
	_, reply, key := acceptedOffer(t, a, b, m)
	body := seal(t, key, good)
	if code, msg := postBody(t, b, reply.Token, body); code != 200 {
		t.Fatalf("good body: %d %s", code, msg)
	}
	sameTree(t, map[string]string{"d/": "", "d/a.txt": "hello", "d/b.txt": "bee"}, snapshot(t, b.cfg.DownloadDir))
	if code, _ := postBody(t, b, reply.Token, body); code != 403 {
		t.Fatalf("replayed body: %d", code)
	}
	// A single file that is complete before the body goes wrong (more data
	// than offered) is removed too.
	fm := protocol.FileManifest("one.txt", 3, 0o644)
	_, freply, fkey := acceptedOffer(t, a, b, fm)
	if code, msg := postBody(t, b, freply.Token, seal(t, fkey, plainBody(transfer.Start{}, "abc+"))); code != 400 {
		t.Fatalf("long single file: %d %s", code, msg)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.DownloadDir, "one.txt")); !os.IsNotExist(err) {
		t.Fatalf("single file kept after a bad body: %v", err)
	}
	// A body captured from one transfer does not fit another (other keys).
	_, reply2, _ := acceptedOffer(t, a, b, m)
	if code, _ := postBody(t, b, reply2.Token, body); code != 400 {
		t.Fatalf("body of another transfer: %d", code)
	}
}

// TestV2BothDirections sends a folder each way between two hosts and checks
// the bytes, and that both histories record the transfers as encrypted.
func TestV2BothDirections(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	_ = a.srv.trust.Approve(b.id.Fingerprint(), "bob", b.id.PublicKeyB64())
	_ = b.srv.trust.Approve(a.id.Fingerprint(), "alice", a.id.PublicKeyB64())
	big := make([]byte, 200<<10+17) // several frames, the last one partial
	_, _ = rand.Read(big)
	for _, dir := range []struct {
		from, to *testHost
	}{{a, b}, {b, a}} {
		src := filepath.Join(t.TempDir(), "set")
		_ = os.MkdirAll(filepath.Join(src, "inner"), 0o755)
		_ = os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644)
		_ = os.WriteFile(filepath.Join(src, "inner", "small.txt"), []byte("small"), 0o644)
		_ = os.WriteFile(filepath.Join(src, "zero"), nil, 0o644)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := transfer.Send(ctx, dir.from.id, transfer.Target{Addr: dir.to.addr(), FP: dir.to.id.Fingerprint()}, src, nil)
		cancel()
		if err != nil {
			t.Fatalf("%s -> %s: %v", dir.from.id.Name, dir.to.id.Name, err)
		}
		sameTree(t, snapshot(t, src), snapshot(t, filepath.Join(dir.to.cfg.DownloadDir, "set")))
		// A single file, too.
		one := writeTemp(t, "one.bin", big[:1000])
		if err := transfer.Send(context.Background(), dir.from.id, transfer.Target{Addr: dir.to.addr()}, one, nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir.to.cfg.DownloadDir, "one.bin")); !bytes.Equal(got, big[:1000]) {
			t.Fatal("single file differs")
		}
		list, err := dir.to.srv.hist.List(0)
		if err != nil || len(list) != 2 {
			t.Fatalf("history %+v %v", list, err)
		}
		for _, e := range list {
			if e.Dir != history.DirIn || e.Result != history.ResultOK || !e.Encrypted {
				t.Fatalf("history entry %+v", e)
			}
		}
	}
}

// TestSendThroughUIRecordsEncrypted: a send started from the UI is recorded
// as encrypted in the sender's history.
func TestSendThroughUIRecordsEncrypted(t *testing.T) {
	a, b := trustedPair(t)
	events := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(events)
	src := writeTemp(t, "ui.txt", []byte("from the UI"))
	if code, body := a.call("POST", "/api/send", map[string]any{"peer": b.addr(), "paths": []string{src}}, true); code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	waitEventData(t, events, "sent", "")
	code, body := a.call("GET", "/api/history", nil, false)
	if code != 200 || !strings.Contains(string(body), `"encrypted":true`) {
		t.Fatalf("history: %d %s", code, body)
	}
}

// TestWireIsEncrypted relays a transfer through a recording proxy: the file
// content never appears in what crosses the network.
func TestWireIsEncrypted(t *testing.T) {
	a, b := trustedPair(t)
	var mu sync.Mutex
	var wire bytes.Buffer
	target := mustURL(b.ts.URL)
	rp := httputil.NewSingleHostReverseProxy(target)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		wire.Write(body)
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		rp.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	secret := bytes.Repeat([]byte("TOP-SECRET-CONTENT "), 5000)
	src := writeTemp(t, "secret.txt", secret)
	err := transfer.Send(context.Background(), a.id, transfer.Target{Addr: strings.TrimPrefix(proxy.URL, "http://"), FP: b.id.Fingerprint()}, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(b.cfg.DownloadDir, "secret.txt")); !bytes.Equal(got, secret) {
		t.Fatal("received bytes differ")
	}
	mu.Lock()
	defer mu.Unlock()
	if bytes.Contains(wire.Bytes(), []byte("TOP-SECRET")) {
		t.Fatal("file content crossed the network in clear")
	}
	if !bytes.Contains(wire.Bytes(), []byte("secret.txt")) {
		t.Fatal("the proxy did not see the offer (test broken)")
	}
}

// fakePeer serves a hello as id with the given proto and answers offers
// with what answer returns. It counts /api/recv calls.
type fakePeer struct {
	ts    *httptest.Server
	recvs int
	mu    sync.Mutex
}

func newFakePeer(t *testing.T, id *node.Identity, proto int, answer func(o *protocol.Offer, sb []byte) protocol.OfferReply) *fakePeer {
	fp := &fakePeer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, protocol.Hello{Proto: proto, Name: "old desk", FP: id.Fingerprint(), Pub: id.PublicKeyB64(), Version: "1.0.0"})
	})
	mux.HandleFunc("POST /api/offer", func(w http.ResponseWriter, r *http.Request) {
		var o protocol.Offer
		_ = json.NewDecoder(r.Body).Decode(&o)
		sb, _ := protocol.SigningBytes(&o)
		writeJSON(w, answer(&o, sb))
	})
	mux.HandleFunc("POST /api/recv", func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		fp.recvs++
		fp.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	})
	fp.ts = httptest.NewServer(mux)
	t.Cleanup(fp.ts.Close)
	return fp
}

func (f *fakePeer) addr() string { return strings.TrimPrefix(f.ts.URL, "http://") }

// TestSenderRefusesBadReplies: a receiver whose reply is unsigned, signed by
// another key, or altered never gets the body.
func TestSenderRefusesBadReplies(t *testing.T) {
	a := newTestHost(t, "alice")
	rcv, other := newTestHost(t, "bob"), newTestHost(t, "mallory")
	src := writeTemp(t, "x.txt", []byte("x"))
	accept := func(o *protocol.Offer) protocol.OfferReply {
		_, eph, _ := protocol.NewEphemeral()
		return protocol.OfferReply{Accepted: true, TransferID: strings.Repeat("a", 32), Token: strings.Repeat("b", 32), Eph: eph}
	}
	cases := map[string]func(o *protocol.Offer, sb []byte) protocol.OfferReply{
		"unsigned": func(o *protocol.Offer, sb []byte) protocol.OfferReply { return accept(o) },
		"signed by another key": func(o *protocol.Offer, sb []byte) protocol.OfferReply {
			r := accept(o)
			protocol.SignReply(other.id, sb, &r)
			return r
		},
		"altered after signing": func(o *protocol.Offer, sb []byte) protocol.OfferReply {
			r := accept(o)
			protocol.SignReply(rcv.id, sb, &r)
			r.Token = strings.Repeat("c", 32)
			return r
		},
		"for another offer": func(o *protocol.Offer, sb []byte) protocol.OfferReply {
			r := accept(o)
			protocol.SignReply(rcv.id, append(sb, 'x'), &r)
			return r
		},
		"accepted without a key": func(o *protocol.Offer, sb []byte) protocol.OfferReply {
			r := accept(o)
			r.Eph = ""
			protocol.SignReply(rcv.id, sb, &r)
			return r
		},
		"unsigned decline": func(o *protocol.Offer, sb []byte) protocol.OfferReply {
			return protocol.OfferReply{TransferID: strings.Repeat("a", 32), Reason: "declined"}
		},
	}
	for name, answer := range cases {
		peer := newFakePeer(t, rcv.id, protocol.Version, answer)
		err := transfer.Send(context.Background(), a.id, transfer.Target{Addr: peer.addr(), FP: rcv.id.Fingerprint()}, src, nil)
		if !errors.Is(err, protocol.ErrReply) {
			t.Errorf("%s: %v, want ErrReply", name, err)
		}
		if peer.recvs != 0 {
			t.Errorf("%s: body sent after a bad reply", name)
		}
	}
	// A properly signed reply from the fake peer is followed (sanity check of
	// the harness): the body goes out.
	good := newFakePeer(t, rcv.id, protocol.Version, func(o *protocol.Offer, sb []byte) protocol.OfferReply {
		r := accept(o)
		protocol.SignReply(rcv.id, sb, &r)
		return r
	})
	if err := transfer.Send(context.Background(), a.id, transfer.Target{Addr: good.addr()}, src, nil); err != nil || good.recvs != 1 {
		t.Fatalf("signed reply: %v (%d bodies)", err, good.recvs)
	}
}

// TestProtocol1Refused: a 1.1 sender refuses a 1.0 receiver with the
// update message, and a 1.1 receiver answers a protocol-1 offer with 426.
func TestProtocol1Refused(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	old := newFakePeer(t, b.id, 1, func(*protocol.Offer, []byte) protocol.OfferReply {
		t.Error("offer sent to a protocol-1 peer")
		return protocol.OfferReply{}
	})
	src := writeTemp(t, "x.txt", []byte("x"))
	err := transfer.Send(context.Background(), a.id, transfer.Target{Addr: old.addr(), FP: b.id.Fingerprint()}, src, nil)
	if err == nil || err.Error() != "old desk runs an older rocket (protocol 1); update it to 1.1 or later" || !errors.Is(err, protocol.ErrProto) {
		t.Fatalf("send to a 1.0 peer: %v", err)
	}
	// The same through the UI: the error event carries the message.
	events := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(events)
	if code, body := a.call("POST", "/api/send", map[string]any{"peer": old.addr(), "paths": []string{src}}, true); code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	timeout := time.After(10 * time.Second)
	for {
		var ev Event
		select {
		case ev = <-events:
		case <-timeout:
			t.Fatal("no error event")
		}
		if ev.Type != "error" {
			continue
		}
		if m, _ := ev.Data.(map[string]any); m["error"] != "old desk runs an older rocket (protocol 1); update it to 1.1 or later" {
			t.Fatalf("error event %v", m)
		}
		break
	}

	// A 1.0 sender's offer (protocol 1, signed the 1.0 way, no eph).
	o, _, err := protocol.NewOffer(a.id, "alice", b.id.Fingerprint(), protocol.FileManifest("x.txt", 1, 0o644), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	o.Proto, o.Eph = 1, ""
	_ = o.SignWith(a.id)
	code, body, _ := postOffer(t, b, o)
	if code != http.StatusUpgradeRequired || strings.TrimSpace(body) != "this device runs rocket 1.1 or later; update the sender" {
		t.Fatalf("protocol-1 offer: %d %q", code, body)
	}
	if len(b.waitIncomingNow()) != 0 {
		t.Fatal("protocol-1 offer prompted")
	}
	// The hello says protocol 2.
	resp, err := http.Get(b.ts.URL + "/api/hello")
	if err != nil {
		t.Fatal(err)
	}
	var h protocol.Hello
	_ = json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if h.Proto != 2 {
		t.Fatalf("hello proto %d", h.Proto)
	}
}

// TestReplySignedAndDecline: every reply, accepted or not, is signed by the
// receiver; a decline carries no token or key.
func TestReplySignedAndDecline(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	o, _, _ := protocol.NewOffer(a.id, "alice", b.id.Fingerprint(), protocol.FileManifest("x.txt", 1, 0o644), time.Now())
	done := make(chan protocol.OfferReply, 1)
	go func() { _, _, r := postOffer(t, b, o); done <- r }()
	b.call("POST", "/api/accept", map[string]any{"id": b.waitIncoming(1)[0].ID, "accept": false}, true)
	r := <-done
	sb, _ := protocol.SigningBytes(o)
	if err := protocol.VerifyReply(b.id.Pub, sb, &r); err != nil || r.Accepted || r.Token != "" || r.Eph != "" || r.Reason != "declined" {
		t.Fatalf("decline %+v (%v)", r, err)
	}
	// Accepted replies from one receiver use a fresh key every time.
	_ = b.srv.trust.Approve(a.id.Fingerprint(), "alice", a.id.PublicKeyB64())
	_, r1, _ := acceptedOffer(t, a, b, protocol.FileManifest("x.txt", 1, 0o644))
	_, r2, _ := acceptedOffer(t, a, b, protocol.FileManifest("x.txt", 1, 0o644))
	if r1.Eph == r2.Eph || r1.Resume != nil {
		t.Fatalf("replies %+v %+v", r1, r2)
	}
}

// TestOfferLowOrderEphRefused: an offer whose ephemeral key gives no shared
// secret is refused before anyone is asked.
func TestOfferLowOrderEphRefused(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	o, _, _ := protocol.NewOffer(a.id, "alice", b.id.Fingerprint(), protocol.FileManifest("x.txt", 1, 0o644), time.Now())
	o.Eph = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // u = 1, a small-order point
	_ = o.SignWith(a.id)
	code, body, _ := postOffer(t, b, o)
	if code != 400 || !strings.Contains(body, protocol.ErrEph.Error()) {
		t.Fatalf("low-order eph: %d %s", code, body)
	}
	if len(b.waitIncomingNow()) != 0 {
		t.Fatal("offer with an unusable key prompted")
	}
}

// TestDropKeeps10Shape: the browser drop page still gets the 1.0 reply (no
// signature, no key) and its history entry is not marked encrypted.
func TestDropKeeps10Shape(t *testing.T) {
	b := newTestHost(t, "bob")
	code, body := b.call("POST", "/drop/offer", map[string]any{"files": []dropFile{{Name: "d.txt", Size: 2}}}, true)
	if code != 200 || strings.Contains(string(body), `"sig"`) || strings.Contains(string(body), `"eph"`) {
		t.Fatalf("drop reply: %d %s", code, body)
	}
	var reply protocol.OfferReply
	_ = json.Unmarshal(body, &reply)
	if code, msg := dropUpload(t, http.DefaultClient, b, reply.Token, []part{{"d.txt", "hi"}}); code != 200 {
		t.Fatalf("drop upload: %d %s", code, msg)
	}
	list, _ := b.srv.hist.List(0)
	if len(list) != 1 || list[0].Encrypted || list[0].Result != history.ResultOK {
		t.Fatalf("drop history %+v", list)
	}
}
