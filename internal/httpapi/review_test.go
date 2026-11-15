package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// switchProxy forwards everything to a target the test can change: the
// address a sender was given now leads to another host, as after an ARP
// spoof.
type switchProxy struct {
	ts *httptest.Server
	mu sync.Mutex
	to string
}

func newSwitchProxy(t *testing.T, to string) *switchProxy {
	p := &switchProxy{to: to}
	p.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		to := p.to
		p.mu.Unlock()
		rp := httputil.NewSingleHostReverseProxy(mustURL(to))
		rp.ErrorHandler = func(http.ResponseWriter, *http.Request, error) { panic(http.ErrAbortHandler) }
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.ts.Close)
	return p
}

func (p *switchProxy) addr() string { return strings.TrimPrefix(p.ts.URL, "http://") }

func (p *switchProxy) set(to string) {
	p.mu.Lock()
	p.to = to
	p.mu.Unlock()
}

// sendTo starts a send of paths from a to peer on a's control API and
// returns its transfer ID.
func sendTo(t *testing.T, a *testHost, peer string, paths ...string) string {
	t.Helper()
	code, body := a.call("POST", "/api/send", map[string]any{"peer": peer, "paths": paths}, true)
	if code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	var r struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.Unmarshal(body, &r)
	return r.TransferID
}

// resumeSend calls POST /api/resume for id and returns the new transfer ID.
func resumeSend(t *testing.T, a *testHost, id string) string {
	t.Helper()
	code, body := a.call("POST", "/api/resume", map[string]any{"transfer_id": id}, true)
	if code != 200 {
		t.Fatalf("resume: %d %s", code, body)
	}
	var r struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.Unmarshal(body, &r)
	return r.TransferID
}

// assertUntouched fails when h received or was offered anything.
func assertUntouched(t *testing.T, h *testHost) {
	t.Helper()
	if d := h.downloads(); len(d) != 0 {
		t.Fatalf("%s received %v", h.id.Name, d)
	}
	if l := h.history(""); len(l) != 0 {
		t.Fatalf("%s saw transfers %+v", h.id.Name, l)
	}
}

// TestRetryGoesOnlyToTheReceiverThatAccepted: a send to a bare host:port
// breaks after bob accepted it, and the address then leads to mallory (who
// would take files from alice without asking). The retry must not send her
// anything: it requires bob's fingerprint, learned from his accepted reply.
// Bob keeps his part.
func TestRetryGoesOnlyToTheReceiverThatAccepted(t *testing.T) {
	a, b := trustedPair(t)
	mallory := newTestHost(t, "mallory")
	if err := mallory.srv.trust.Approve(a.id.Fingerprint(), "alice", a.id.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	a.srv.retryDelays = []time.Duration{300 * time.Millisecond}
	src := writeTemp(t, "big.iso", randomBytes(600<<10))
	cut := newCutProxy(t, b.ts.URL, 200<<10)
	front := newSwitchProxy(t, cut.ts.URL)
	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)

	id := sendTo(t, a, front.addr(), src)
	waitEventData(t, aEv, "retrying", id)
	front.set(mallory.ts.URL)
	typ, m := waitEnd(t, aEv, id)
	if typ != "error" || m["result"] == history.ResultOK {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	assertUntouched(t, mallory)
	b.waitKept(1)
}

// TestResumeIsPinnedAndKeptUntilAccepted: an interrupted send to a bare
// host:port is resumed while the address leads to someone else: the resume
// fails without sending her anything, and the interrupted send is still
// listed (it was not accepted). Resumed again once the address leads back
// to bob, it continues with only the missing bytes, and is gone.
func TestResumeIsPinnedAndKeptUntilAccepted(t *testing.T) {
	a, b := trustedPair(t)
	mallory := newTestHost(t, "mallory")
	if err := mallory.srv.trust.Approve(a.id.Fingerprint(), "alice", a.id.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	a.srv.retryDelays = []time.Duration{}
	data := randomBytes(500 << 10)
	src := writeTemp(t, "doc.bin", data)
	cut := newCutProxy(t, b.ts.URL, 200<<10)
	front := newSwitchProxy(t, cut.ts.URL)
	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)

	id := sendTo(t, a, front.addr(), src)
	if typ, m := waitEnd(t, aEv, id); typ != "error" || m["result"] != history.ResultInterrupted {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	b.waitKept(1)
	out, _ := a.resumable()
	if len(out) != 1 || out[0].PeerFP != "" || out[0].ReceiverFP != b.id.Fingerprint() {
		t.Fatalf("outbox %+v", out)
	}

	front.set(mallory.ts.URL)
	tid := resumeSend(t, a, id)
	if typ, m := waitEnd(t, aEv, tid); typ != "error" {
		t.Fatalf("resume to the wrong host ended with %s %v", typ, m)
	}
	assertUntouched(t, mallory)
	if out, _ := a.resumable(); len(out) != 1 || out[0].ID != id {
		t.Fatalf("after a resume that was never accepted, outbox %+v", out)
	}

	// The peer is away: same.
	front.set("http://127.0.0.1:1")
	tid = resumeSend(t, a, id)
	if typ, m := waitEnd(t, aEv, tid); typ != "error" {
		t.Fatalf("resume to nobody ended with %s %v", typ, m)
	}
	if out, _ := a.resumable(); len(out) != 1 || out[0].ID != id {
		t.Fatalf("after a resume to nobody, outbox %+v", out)
	}

	front.set(cut.ts.URL)
	tid = resumeSend(t, a, id)
	if typ, m := waitEnd(t, aEv, tid); typ != "sent" {
		t.Fatalf("resume ended with %s %v", typ, m)
	}
	if got := sumFile(t, filepath.Join(b.cfg.DownloadDir, "doc.bin")); got != sum(data) {
		t.Fatal("received file differs")
	}
	checkOnlyMissingSent(t, cut, protocol.FileManifest("doc.bin", int64(len(data)), 0o644), 200<<10, 0)
	if out, _ := a.resumable(); len(out) != 0 {
		t.Fatalf("still resumable: %+v", out)
	}
	b.waitNoKept()
}

// TestSlowResumeCheckRenewsTheOffer: when the prefix check takes longer
// than the sender trusts the upload token to live (shortened here to
// nothing), the sender offers again before uploading. The receiver, which
// does not trust the sender, accepts that offer without a prompt and names
// the same resume point; only the missing bytes are sent.
func TestSlowResumeCheckRenewsTheOffer(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	a.srv.retryDelays = []time.Duration{}
	a.srv.renewAfter = -1 // always renew
	data := randomBytes(600 << 10)
	src := writeTemp(t, "film.bin", data)
	p := newCutProxy(t, b.ts.URL, 200<<10)
	aEv, bEv := a.srv.hub.subscribe(), b.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	defer b.srv.hub.unsubscribe(bEv)

	id := sendVia(t, a, b, p, src)
	b.call("POST", "/api/accept", map[string]any{"id": b.waitIncoming(1)[0].ID, "accept": true}, true)
	if typ, m := waitEnd(t, aEv, id); typ != "error" || m["result"] != history.ResultInterrupted {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	b.waitKept(1)
	drain(bEv)

	tid := resumeSend(t, a, id)
	if typ, m := waitEnd(t, aEv, tid); typ != "sent" {
		t.Fatalf("resume ended with %s %v", typ, m)
	}
	if got := sumFile(t, filepath.Join(b.cfg.DownloadDir, "film.bin")); got != sum(data) {
		t.Fatal("received file differs")
	}
	sent, lengths, resumes := p.uploads()
	if len(resumes) != 3 || resumes[1] == nil || resumes[2] == nil || *resumes[1] != *resumes[2] {
		t.Fatalf("replies %v: want the first, the resume and its renewal with the same point", resumes)
	}
	m := protocol.FileManifest("film.bin", int64(len(data)), 0o644)
	want := transfer.SealedBodySize(m, transfer.Start{Entry: resumes[2].Entry, Offset: resumes[2].Offset})
	if len(sent) != 2 || sent[1] != want || lengths[1] != want {
		t.Fatalf("uploads %v/%v, want the second to carry %d bytes", sent, lengths, want)
	}
	for _, ev := range drain(bEv) {
		if ev.Type == "offer" {
			t.Fatalf("the receiver asked again: %v", ev.Data)
		}
	}
	b.waitNoKept()
}

// TestRetryOfAcceptedOfferWithoutBodyNeedsNoPrompt: the connection breaks
// right after the receiver (which does not trust the sender) accepted,
// before any of the body arrived, so nothing was kept. The retry continues
// what the person accepted without asking again.
func TestRetryOfAcceptedOfferWithoutBodyNeedsNoPrompt(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	fastRetries(a, 5)
	data := randomBytes(300 << 10)
	src := writeTemp(t, "pic.bin", data)
	p := newCutProxy(t, b.ts.URL, 0)
	aEv, bEv := a.srv.hub.subscribe(), b.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	defer b.srv.hub.unsubscribe(bEv)

	id := sendVia(t, a, b, p, src)
	b.call("POST", "/api/accept", map[string]any{"id": b.waitIncoming(1)[0].ID, "accept": true}, true)
	if typ, m := waitEnd(t, aEv, id); typ != "sent" {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	if got := sumFile(t, filepath.Join(b.cfg.DownloadDir, "pic.bin")); got != sum(data) {
		t.Fatal("received file differs")
	}
	offers := 0
	for _, ev := range drain(bEv) {
		if ev.Type == "offer" {
			offers++
		}
	}
	if offers != 1 {
		t.Fatalf("the receiver asked %d times", offers)
	}
	if _, _, resumes := p.uploads(); len(resumes) < 2 {
		t.Fatalf("replies %v", resumes)
	}
}

// drain returns the events waiting in ch.
func drain(ch chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestRetryOfAcceptedOfferWhoseUploadNeverArrived: the same when the
// upload request never reaches the receiver (its token stays unused). The
// retry is accepted without a prompt.
func TestRetryOfAcceptedOfferWhoseUploadNeverArrived(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	fastRetries(a, 5)
	data := randomBytes(200 << 10)
	src := writeTemp(t, "pic.bin", data)
	rp := httputil.NewSingleHostReverseProxy(mustURL(b.ts.URL))
	var mu sync.Mutex
	uploads := 0
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/recv" {
			mu.Lock()
			uploads++
			first := uploads == 1
			mu.Unlock()
			if first {
				panic(http.ErrAbortHandler) // the connection drops before it reaches bob
			}
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	aEv, bEv := a.srv.hub.subscribe(), b.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	defer b.srv.hub.unsubscribe(bEv)

	id := sendTo(t, a, strings.TrimPrefix(front.URL, "http://"), src)
	b.call("POST", "/api/accept", map[string]any{"id": b.waitIncoming(1)[0].ID, "accept": true}, true)
	if typ, m := waitEnd(t, aEv, id); typ != "sent" {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	if got := sumFile(t, filepath.Join(b.cfg.DownloadDir, "pic.bin")); got != sum(data) {
		t.Fatal("received file differs")
	}
	offers := 0
	for _, ev := range drain(bEv) {
		if ev.Type == "offer" {
			offers++
		}
	}
	if offers != 1 {
		t.Fatalf("the receiver asked %d times", offers)
	}
}

// TestRelayResumePinnedRenewedAndKeptUntilAccepted: the same three rules
// for files picked in the desktop page. An interrupted page send to a bare
// host:port resumed while the address leads elsewhere fails without
// sending anything there and stays in the outbox; resumed once the address
// leads back, with a slow prefix check (the renewal threshold shortened to
// nothing), it renews the offer, which the receiver accepts without asking
// at the same point, and completes.
func TestRelayResumePinnedRenewedAndKeptUntilAccepted(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	mallory := newTestHost(t, "mallory")
	if err := mallory.srv.trust.Approve(a.id.Fingerprint(), "alice", a.id.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	a.srv.retryDelays = []time.Duration{}
	data := randomBytes(600 << 10)
	pg := newPage(t, a, []pageFile{{"film.bin", data}})
	cut := newCutProxy(t, b.ts.URL, 200<<10)
	front := newSwitchProxy(t, cut.ts.URL)
	bEv := b.srv.hub.subscribe()
	defer b.srv.hub.unsubscribe(bEv)
	go func() {
		b.call("POST", "/api/accept", map[string]any{"id": b.waitIncoming(1)[0].ID, "accept": true}, true)
	}()
	if end := pg.run(pg.start(front.addr())); end.Result != string(history.ResultInterrupted) || end.Retryable {
		t.Fatalf("send ended with %+v", end)
	}
	b.waitKept(1)
	old := pg.id
	if out, _ := a.resumable(); len(out) != 1 || out[0].ID != old || out[0].ReceiverFP != b.id.Fingerprint() {
		t.Fatalf("outbox %+v", out)
	}

	front.set(mallory.ts.URL)
	pg.answers, pg.froms, pg.upBytes = nil, nil, nil
	if end := pg.run(map[string]any{"resume": old, "files": pg.list()}); end.Result == "sent" {
		t.Fatalf("resume to the wrong host ended with %+v", end)
	}
	assertUntouched(t, mallory)
	if out, _ := a.resumable(); len(out) != 1 || out[0].ID != old {
		t.Fatalf("after a resume that was never accepted, outbox %+v", out)
	}

	front.set(cut.ts.URL)
	a.srv.renewAfter = -1 // always renew
	drain(bEv)
	pg.answers, pg.froms, pg.upBytes = nil, nil, nil
	if end := pg.run(map[string]any{"resume": old, "files": pg.list()}); end.Result != "sent" {
		t.Fatalf("resume ended with %+v (answers %+v)", end, pg.answers)
	}
	if got := sumFile(t, filepath.Join(b.cfg.DownloadDir, "film.bin")); got != sum(data) {
		t.Fatal("received file differs")
	}
	sent, _, resumes := cut.uploads()
	if len(resumes) != 3 || resumes[1] == nil || resumes[2] == nil || *resumes[1] != *resumes[2] {
		t.Fatalf("replies %v: want the first, the resume and its renewal with the same point", resumes)
	}
	want := transfer.SealedBodySize(pg.manifest(), transfer.Start{Entry: resumes[2].Entry, Offset: resumes[2].Offset})
	if len(sent) != 2 || sent[1] != want {
		t.Fatalf("uploads %v, want the second to carry %d bytes", sent, want)
	}
	for _, ev := range drain(bEv) {
		if ev.Type == "offer" {
			t.Fatalf("the receiver asked again: %v", ev.Data)
		}
	}
	if out, _ := a.resumable(); len(out) != 0 {
		t.Fatalf("still resumable: %+v", out)
	}
	b.waitNoKept()
}
