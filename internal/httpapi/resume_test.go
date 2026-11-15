package httpapi

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/resume"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// cutProxy relays HTTP to a receiver and plays the network: it can drop the
// connection of the k-th POST /api/recv after a given number of body bytes,
// slow bodies down, and records what crossed it: the body bytes and
// Content-Length of every upload and the resume point of every reply.
type cutProxy struct {
	ts *httptest.Server

	mu      sync.Mutex
	cuts    []int64 // per upload: drop after this many body bytes (-1: never)
	slow    time.Duration
	sent    []int64 // body bytes forwarded per upload
	lengths []int64 // Content-Length per upload
	resumes []*protocol.ResumePoint
	accepts int
}

var errCut = errors.New("cut by the test proxy")

func newCutProxy(t *testing.T, target string, cuts ...int64) *cutProxy {
	p := &cutProxy{cuts: cuts}
	rp := httputil.NewSingleHostReverseProxy(mustURL(target))
	// A failed upstream request drops the client's connection instead of
	// answering 502, as a broken network would.
	rp.ErrorHandler = func(http.ResponseWriter, *http.Request, error) { panic(http.ErrAbortHandler) }
	rp.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.URL.Path != "/api/offer" || resp.StatusCode != 200 {
			return nil
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(b))
		var r protocol.OfferReply
		if json.Unmarshal(b, &r) == nil && r.Accepted {
			p.mu.Lock()
			p.accepts++
			p.resumes = append(p.resumes, r.Resume)
			p.mu.Unlock()
		}
		return nil
	}
	p.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/recv" {
			p.mu.Lock()
			k := len(p.sent)
			p.sent = append(p.sent, 0)
			p.lengths = append(p.lengths, r.ContentLength)
			cut := int64(-1)
			if k < len(p.cuts) {
				cut = p.cuts[k]
			}
			slow := p.slow
			p.mu.Unlock()
			r.Body = &cutBody{rc: r.Body, p: p, k: k, cut: cut, slow: slow}
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.ts.Close)
	return p
}

func (p *cutProxy) addr() string { return strings.TrimPrefix(p.ts.URL, "http://") }

func (p *cutProxy) setSlow(d time.Duration) {
	p.mu.Lock()
	p.slow = d
	p.mu.Unlock()
}

// uploads returns the body bytes and Content-Length of every upload, and
// the resume point of every accepted reply.
func (p *cutProxy) uploads() (sent, lengths []int64, resumes []*protocol.ResumePoint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.sent...), append([]int64(nil), p.lengths...), append([]*protocol.ResumePoint(nil), p.resumes...)
}

type cutBody struct {
	rc   io.ReadCloser
	p    *cutProxy
	k    int
	n    int64
	cut  int64
	slow time.Duration
}

func (b *cutBody) Read(buf []byte) (int, error) {
	if b.cut >= 0 && b.n >= b.cut {
		return 0, errCut
	}
	if b.slow > 0 {
		time.Sleep(b.slow)
		buf = buf[:min(len(buf), 16<<10)]
	}
	if b.cut >= 0 && int64(len(buf)) > b.cut-b.n {
		buf = buf[:b.cut-b.n]
	}
	n, err := b.rc.Read(buf)
	b.n += int64(n)
	b.p.mu.Lock()
	b.p.sent[b.k] += int64(n)
	b.p.mu.Unlock()
	return n, err
}

func (b *cutBody) Close() error { return b.rc.Close() }

// fastRetries makes a's sends retry at once.
func fastRetries(a *testHost, n int) {
	d := make([]time.Duration, n)
	for i := range d {
		d[i] = 20 * time.Millisecond
	}
	a.srv.retryDelays = d
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func sum(b []byte) [32]byte { return sha256.Sum256(b) }

func sumFile(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sum(b)
}

// sendVia starts a send of paths from a to b through the proxy (with b's
// fingerprint) on a's control API and returns its transfer ID.
func sendVia(t *testing.T, a, b *testHost, p *cutProxy, paths ...string) string {
	t.Helper()
	code, body := a.call("POST", "/api/send", map[string]any{"peer": p.addr(), "paths": paths}, true)
	if code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	var r struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.Unmarshal(body, &r)
	return r.TransferID
}

// waitEnd waits for the end of transfer id on h's events: "sent" or
// "done" returns nil data; "error" and "cancelled" return their data.
func waitEnd(t *testing.T, ch chan Event, id string) (string, map[string]any) {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case ev := <-ch:
			m, _ := ev.Data.(map[string]any)
			if id != "" && m["transfer_id"] != id {
				continue
			}
			switch ev.Type {
			case "sent", "done", "error", "cancelled":
				return ev.Type, m
			}
		case <-timeout:
			t.Fatalf("transfer %s did not end", id)
		}
	}
}

// waitNoState waits until h keeps nothing for a resume and has nothing in
// its download folder.
func (h *testHost) waitNoState() {
	h.t.Helper()
	h.assertNoPartials()
	h.waitNoKept()
}

// waitKept waits until h keeps n transfers for a resume.
func (h *testHost) waitKept(n int) []map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, in := h.resumable()
		if len(in) == n {
			return in
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("kept %v, want %d", in, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitNoKept waits until h keeps nothing for a resume (no records, no part
// files).
func (h *testHost) waitNoKept() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for hasPart(h) {
		if time.Now().After(deadline) {
			h.t.Fatalf("part files left: %v", h.downloads())
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		ids, _ := os.ReadDir(filepath.Join(h.cfg.DataDir, "resume"))
		if len(ids) == 0 {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("resume records left: %v", ids)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// resumable reads GET /api/resumable.
func (h *testHost) resumable() (out []resume.Outgoing, in []map[string]any) {
	h.t.Helper()
	code, b := h.call("GET", "/api/resumable", nil, false)
	if code != 200 {
		h.t.Fatalf("resumable: %d %s", code, b)
	}
	var r struct {
		Outgoing []resume.Outgoing `json:"outgoing"`
		Incoming []map[string]any  `json:"incoming"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		h.t.Fatal(err)
	}
	return r.Outgoing, r.Incoming
}

// checkOnlyMissingSent checks the wire: the first upload was cut, and the
// second carried exactly the body from the resume point the receiver
// offered, which lies inside entry wantEntry with some bytes before it.
func checkOnlyMissingSent(t *testing.T, p *cutProxy, m protocol.Manifest, cut int64, wantEntry uint32) {
	t.Helper()
	sent, lengths, resumes := p.uploads()
	if len(sent) != 2 || len(resumes) != 2 {
		t.Fatalf("uploads %v, replies %v", sent, resumes)
	}
	full := transfer.SealedBodySize(m, transfer.Start{})
	if resumes[0] != nil || lengths[0] != full || sent[0] != cut {
		t.Fatalf("first upload: resume %v, length %d (full %d), sent %d (cut %d)", resumes[0], lengths[0], full, sent[0], cut)
	}
	rp := resumes[1]
	if rp == nil || rp.Entry != wantEntry || rp.Offset == 0 || rp.Offset >= uint64(m.Entries[rp.Entry].Size) {
		t.Fatalf("resume point %+v, want inside entry %d", rp, wantEntry)
	}
	start := transfer.Start{Entry: rp.Entry, Offset: rp.Offset}
	want := transfer.SealedBodySize(m, start)
	if lengths[1] != want || sent[1] != want {
		t.Fatalf("second upload: length %d, sent %d, want %d (the body from %+v)", lengths[1], sent[1], want, start)
	}
	before, _ := transfer.Before(m, start)
	if before <= 0 || transfer.BodySize(m, start)-transfer.StartHeaderSize != m.Total-before {
		t.Fatalf("resume sent %d of %d bytes", transfer.BodySize(m, start)-transfer.StartHeaderSize, m.Total)
	}
	if sent[0]+sent[1] >= full+full/2 {
		t.Fatalf("%d bytes on the wire for a %d-byte body", sent[0]+sent[1], full)
	}
}

// TestResumeMidFile drops the connection in the middle of a single file:
// the sender retries on its own, the receiver auto-accepts the retry with
// its resume point, and only the missing bytes cross the wire. The result
// has the source's SHA-256.
func TestResumeMidFile(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	fastRetries(a, 5)
	data := randomBytes(1<<20 + 12345)
	src := writeTemp(t, "film.bin", data)
	const cut = 300 << 10
	p := newCutProxy(t, b.ts.URL, cut)
	aEv, bEv := a.srv.hub.subscribe(), b.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	defer b.srv.hub.unsubscribe(bEv)

	// b does not trust a: the first offer needs the person, the retry not.
	id := sendVia(t, a, b, p, src)
	b.call("POST", "/api/accept", map[string]any{"id": b.waitIncoming(1)[0].ID, "accept": true}, true)
	retry := waitEventData(t, aEv, "retrying", id)
	if retry["attempt"] != 1 || retry["attempts"] != 5 {
		t.Fatalf("retrying %v", retry)
	}
	typ, m := waitEnd(t, aEv, id)
	if typ != "sent" {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	if got := sumFile(t, filepath.Join(b.cfg.DownloadDir, "film.bin")); got != sum(data) {
		t.Fatal("received file differs from the source")
	}
	checkOnlyMissingSent(t, p, protocol.FileManifest("film.bin", int64(len(data)), 0o644), cut, 0)
	sameTree(t, map[string]string{"film.bin": string(data)}, snapshot(t, b.cfg.DownloadDir))
	b.waitNoKept()

	// The receiver's history: the first attempt interrupted, the retry ok;
	// the sender's: one ok entry. A "kept" event came first.
	list := b.history("")
	if len(list) != 2 || list[0].Result != history.ResultOK || list[1].Result != history.ResultInterrupted {
		t.Fatalf("receiver history %+v", list)
	}
	if out := a.history(""); len(out) != 1 || out[0].Result != history.ResultOK || out[0].ID != id {
		t.Fatalf("sender history %+v", out)
	}
	sawKept := false
	for done := false; !done; {
		select {
		case ev := <-bEv:
			sawKept = sawKept || ev.Type == "kept"
		default:
			done = true
		}
	}
	if !sawKept {
		t.Error("no kept event on the receiver")
	}
}

// TestResumeMidFolder: the same for a folder, cut in its second file. The
// first file is not sent again, and the tree arrives identical.
func TestResumeMidFolder(t *testing.T) {
	a, b := trustedPair(t)
	fastRetries(a, 5)
	dir := filepath.Join(t.TempDir(), "album")
	files := map[string][]byte{
		"a.bin":         randomBytes(100 << 10),
		"inner/b.bin":   randomBytes(700 << 10),
		"inner/c.bin":   randomBytes(50 << 10),
		"empty/":        nil,
		"inner/zero.tx": {},
	}
	for rel, d := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if strings.HasSuffix(rel, "/") {
			_ = os.MkdirAll(p, 0o755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, d, 0o644)
	}
	plan, err := transfer.PlanPaths([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	bIdx := -1
	for i, e := range plan.Manifest.Entries {
		if e.Path == "album/inner/b.bin" {
			bIdx = i
		}
	}
	const cut = 400 << 10
	p := newCutProxy(t, b.ts.URL, cut)
	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	id := sendVia(t, a, b, p, dir)
	if typ, m := waitEnd(t, aEv, id); typ != "sent" {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	sameTree(t, snapshot(t, dir), snapshot(t, filepath.Join(b.cfg.DownloadDir, "album")))
	if len(b.downloads()) != 1 {
		t.Fatalf("downloads %v", b.downloads())
	}
	checkOnlyMissingSent(t, p, plan.Manifest, cut, uint32(bIdx))
	b.waitNoKept()
}

// interrupt sends src from a to b through a proxy that drops the first
// upload after cut bytes, with no retries: the send ends interrupted, and
// both sides keep what they need for a resume. It returns the proxy and
// the send's transfer ID.
func interrupt(t *testing.T, a, b *testHost, cut int64, src string) (*cutProxy, string) {
	t.Helper()
	a.srv.retryDelays = []time.Duration{}
	p := newCutProxy(t, b.ts.URL, cut)
	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	id := sendVia(t, a, b, p, src)
	typ, m := waitEnd(t, aEv, id)
	if typ != "error" || m["result"] != history.ResultInterrupted {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	b.waitKept(1)
	return p, id
}

// TestResumeAPI: an interrupted send is listed on both sides; POST
// /api/resume (CSRF header required) continues it with only the missing
// bytes; afterwards nothing is left to resume.
func TestResumeAPI(t *testing.T) {
	a, b := trustedPair(t)
	data := randomBytes(600 << 10)
	src := writeTemp(t, "doc.bin", data)
	p, id := interrupt(t, a, b, 200<<10, src)

	out, _ := a.resumable()
	if len(out) != 1 || out[0].ID != id || out[0].Name != "doc.bin" || out[0].Bytes != int64(len(data)) || out[0].RemoteID == "" {
		t.Fatalf("sender resumable %+v", out)
	}
	if e := a.waitHistory(id); e.Result != history.ResultInterrupted {
		t.Fatalf("sender history %+v", e)
	}
	_, in := b.resumable()
	if len(in) != 1 || in[0]["name"] != "doc.bin" || in[0]["sender_fp"] != a.id.Fingerprint() || in[0]["done"].(float64) <= 0 {
		t.Fatalf("receiver resumable %+v", in)
	}
	if !hasPart(b) {
		t.Fatal("the receiver kept no part file")
	}
	// Control routes need the CSRF header.
	if code, _ := a.call("POST", "/api/resume", map[string]any{"transfer_id": id}, false); code != 403 {
		t.Fatalf("resume without CSRF: %d", code)
	}
	if code, _ := a.call("POST", "/api/discard", map[string]any{"transfer_id": id}, false); code != 403 {
		t.Fatalf("discard without CSRF: %d", code)
	}
	if code, _ := a.call("POST", "/api/resume", map[string]any{"transfer_id": strings.Repeat("0", 32)}, true); code != 404 {
		t.Fatalf("resume of nothing: %d", code)
	}

	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	code, body := a.call("POST", "/api/resume", map[string]any{"transfer_id": id}, true)
	if code != 200 {
		t.Fatalf("resume: %d %s", code, body)
	}
	var r struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.Unmarshal(body, &r)
	if typ, m := waitEnd(t, aEv, r.TransferID); typ != "sent" {
		t.Fatalf("resumed send ended with %s %v", typ, m)
	}
	if got := sumFile(t, filepath.Join(b.cfg.DownloadDir, "doc.bin")); got != sum(data) {
		t.Fatal("received file differs")
	}
	checkOnlyMissingSent(t, p, protocol.FileManifest("doc.bin", int64(len(data)), 0o644), 200<<10, 0)
	if out, _ := a.resumable(); len(out) != 0 {
		t.Fatalf("still resumable on the sender: %+v", out)
	}
	if _, in := b.resumable(); len(in) != 0 {
		t.Fatalf("still kept on the receiver: %+v", in)
	}
	b.waitNoKept()
}

// TestResumeChangedSourceStartsOver: when the source changed (same size,
// different bytes in the part the receiver holds), the sender's prefix
// check fails and the body starts at (0,0); the receiver drops what it kept
// and the result is the new content, in one copy.
func TestResumeChangedSourceStartsOver(t *testing.T) {
	a, b := trustedPair(t)
	data := randomBytes(500 << 10)
	src := writeTemp(t, "notes.bin", data)
	p, id := interrupt(t, a, b, 200<<10, src)
	data[1000] ^= 0xff // inside the prefix the receiver kept
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	code, body := a.call("POST", "/api/resume", map[string]any{"transfer_id": id}, true)
	if code != 200 {
		t.Fatalf("resume: %d %s", code, body)
	}
	var r struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.Unmarshal(body, &r)
	if typ, m := waitEnd(t, aEv, r.TransferID); typ != "sent" {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	m := protocol.FileManifest("notes.bin", int64(len(data)), 0o644)
	sent, lengths, resumes := p.uploads()
	if len(resumes) != 2 || resumes[1] == nil {
		t.Fatalf("the receiver offered no resume point: %v", resumes)
	}
	if full := transfer.SealedBodySize(m, transfer.Start{}); lengths[1] != full || sent[1] != full {
		t.Fatalf("second upload %d/%d bytes, want the whole body (%d)", sent[1], lengths[1], full)
	}
	sameTree(t, map[string]string{"notes.bin": string(data)}, snapshot(t, b.cfg.DownloadDir))
	b.waitNoKept()
}

// TestKeptExpires: a kept transfer whose 24 hours are up is swept with its
// files; the next offer of the same files is a fresh transfer.
func TestKeptExpires(t *testing.T) {
	a, b := trustedPair(t)
	data := randomBytes(400 << 10)
	src := writeTemp(t, "old.bin", data)
	p, id := interrupt(t, a, b, 150<<10, src)
	if !hasPart(b) {
		t.Fatal("nothing kept")
	}
	bEv := b.srv.hub.subscribe()
	defer b.srv.hub.unsubscribe(bEv)
	// Not yet expired: the sweep keeps it.
	b.srv.sweep()
	if _, in := b.resumable(); len(in) != 1 {
		t.Fatalf("kept %v", in)
	}
	later := func() time.Time { return time.Now().Add(resume.KeepFor + time.Minute) }
	b.srv.kept.Now, a.srv.outbox.Now = later, later
	b.srv.sweep()
	a.srv.sweep()
	b.srv.kept.Now, a.srv.outbox.Now = time.Now, time.Now
	b.waitNoState()
	if gone := waitEventData(t, bEv, "kept_gone", ""); gone["reason"] != "expired" {
		t.Fatalf("kept_gone %v", gone)
	}
	if out, _ := a.resumable(); len(out) != 0 {
		t.Fatalf("expired send still listed: %v", out)
	}
	if code, _ := a.call("POST", "/api/resume", map[string]any{"transfer_id": id}, true); code != 404 {
		t.Fatalf("resume after expiry: %d", code)
	}
	// Sending the same files again starts from the beginning.
	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	nid := sendVia(t, a, b, p, src)
	if typ, m := waitEnd(t, aEv, nid); typ != "sent" {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	if _, _, resumes := p.uploads(); len(resumes) != 2 || resumes[1] != nil {
		t.Fatalf("resume offered after expiry: %v", resumes)
	}
	sameTree(t, map[string]string{"old.bin": string(data)}, snapshot(t, b.cfg.DownloadDir))
}

// TestResumeOnlySameSenderAndManifest: a kept transfer is continued without
// a prompt only by an offer with the same sender key and the same manifest
// hash. Anyone else, or other files, gets the normal prompt.
func TestResumeOnlySameSenderAndManifest(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	mallory := newTestHost(t, "mallory")
	data := randomBytes(300 << 10)
	src := writeTemp(t, "plan.bin", data)
	a.srv.retryDelays = []time.Duration{}
	p := newCutProxy(t, b.ts.URL, 100<<10)
	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	id := sendVia(t, a, b, p, src)
	b.call("POST", "/api/accept", map[string]any{"id": b.waitIncoming(1)[0].ID, "accept": true}, true)
	if typ, m := waitEnd(t, aEv, id); typ != "error" || m["result"] != history.ResultInterrupted {
		t.Fatalf("send ended with %s %v", typ, m)
	}
	b.waitKept(1)
	plan, err := transfer.PlanPaths([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	m := plan.Manifest // the manifest that was sent (file modes differ by OS)

	// prompted posts an offer and reports whether it waited for the person
	// (then declines it) or was answered at once.
	prompted := func(from *testHost, man protocol.Manifest) (bool, protocol.OfferReply) {
		o, _, err := protocol.NewOffer(from.id, from.id.Name, b.id.Fingerprint(), man, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan protocol.OfferReply, 1)
		go func() { _, _, r := postOffer(t, b, o); done <- r }()
		select {
		case r := <-done:
			return false, r
		case <-time.After(300 * time.Millisecond):
		}
		inc := b.waitIncoming(1)
		b.call("POST", "/api/accept", map[string]any{"id": inc[0].ID, "accept": false}, true)
		return true, <-done
	}
	if ok, r := prompted(mallory, m); !ok || r.Accepted || r.Resume != nil {
		t.Fatalf("another sender, same files: prompted %v, reply %+v", ok, r)
	}
	other := protocol.FileManifest("plan.bin", int64(len(data))+1, m.Entries[0].Mode)
	if ok, r := prompted(a, other); !ok || r.Accepted || r.Resume != nil {
		t.Fatalf("same sender, other files: prompted %v, reply %+v", ok, r)
	}
	if ok, r := prompted(a, m); ok || !r.Accepted || r.Resume == nil || r.Resume.Offset == 0 {
		t.Fatalf("same sender, same files: prompted %v, reply %+v", ok, r)
	}
	// The declines left the kept transfer alone; the real resume finishes it.
	code, body := a.call("POST", "/api/resume", map[string]any{"transfer_id": id}, true)
	if code != 200 {
		t.Fatalf("resume: %d %s", code, body)
	}
	var r struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.Unmarshal(body, &r)
	if typ, m := waitEnd(t, aEv, r.TransferID); typ != "sent" {
		t.Fatalf("resumed send ended with %s %v", typ, m)
	}
	if got := sumFile(t, filepath.Join(b.cfg.DownloadDir, "plan.bin")); got != sum(data) {
		t.Fatal("received file differs")
	}
	b.waitNoKept()
}

// TestCancelLeavesNothing: cancelling on either side, during the body or
// while the sender waits to reconnect, and declining, leave no kept state
// and no partial files.
func TestCancelLeavesNothing(t *testing.T) {
	data := randomBytes(2 << 20)
	t.Run("receiver cancels", func(t *testing.T) {
		a, b := trustedPair(t)
		fastRetries(a, 5)
		src := writeTemp(t, "big.bin", data)
		p := newCutProxy(t, b.ts.URL)
		p.setSlow(5 * time.Millisecond)
		aEv := a.srv.hub.subscribe()
		defer a.srv.hub.unsubscribe(aEv)
		id := sendVia(t, a, b, p, src)
		b.cancel(b.waitMoving("in").ID)
		if typ, m := waitEnd(t, aEv, id); typ != "cancelled" {
			t.Fatalf("sender saw %s %v", typ, m)
		}
		b.waitNoState()
		if out, _ := a.resumable(); len(out) != 0 {
			t.Fatalf("sender kept %v", out)
		}
		if sent, _, _ := p.uploads(); len(sent) != 1 {
			t.Fatalf("the sender retried after a cancel: %v", sent)
		}
	})
	t.Run("sender cancels", func(t *testing.T) {
		a, b := trustedPair(t)
		fastRetries(a, 5)
		src := writeTemp(t, "big.bin", data)
		p := newCutProxy(t, b.ts.URL)
		p.setSlow(5 * time.Millisecond)
		bEv := b.srv.hub.subscribe()
		defer b.srv.hub.unsubscribe(bEv)
		id := sendVia(t, a, b, p, src)
		in := b.waitMoving("in")
		a.cancel(id)
		typ, m := waitEnd(t, bEv, in.ID)
		if typ != "cancelled" || m["by"] != "peer" {
			t.Fatalf("receiver saw %s %v", typ, m)
		}
		if e := b.waitHistory(in.ID); e.Result != history.ResultCancelled || !strings.Contains(e.Error, "sender") {
			t.Fatalf("receiver history %+v", e)
		}
		b.waitNoState()
		if e := a.waitHistory(id); e.Result != history.ResultCancelled {
			t.Fatalf("sender history %+v", e)
		}
		if out, _ := a.resumable(); len(out) != 0 {
			t.Fatalf("sender kept %v", out)
		}
	})
	t.Run("sender cancels while reconnecting", func(t *testing.T) {
		a, b := trustedPair(t)
		a.srv.retryDelays = []time.Duration{time.Minute}
		src := writeTemp(t, "big.bin", data)
		p := newCutProxy(t, b.ts.URL, 300<<10)
		aEv := a.srv.hub.subscribe()
		defer a.srv.hub.unsubscribe(aEv)
		id := sendVia(t, a, b, p, src)
		retry := waitEventData(t, aEv, "retrying", id)
		if retry["attempt"] != 1 || retry["attempts"] != 1 {
			t.Fatalf("retrying %v", retry)
		}
		if list := a.transfers(); len(list) != 1 || list[0].State != transfer.StateReconnecting || list[0].Attempt != 1 || list[0].Attempts != 1 {
			t.Fatalf("transfers while reconnecting %+v", list)
		}
		b.waitKept(1)
		a.cancel(id)
		if typ, m := waitEnd(t, aEv, id); typ != "cancelled" {
			t.Fatalf("sender saw %s %v", typ, m)
		}
		b.waitNoState()
		if out, _ := a.resumable(); len(out) != 0 {
			t.Fatalf("sender kept %v", out)
		}
	})
	t.Run("sender discards", func(t *testing.T) {
		a, b := trustedPair(t)
		src := writeTemp(t, "big.bin", data)
		_, id := interrupt(t, a, b, 300<<10, src)
		if code, body := a.call("POST", "/api/discard", map[string]any{"transfer_id": id}, true); code != 200 {
			t.Fatalf("discard: %d %s", code, body)
		}
		b.waitNoState()
		if out, _ := a.resumable(); len(out) != 0 {
			t.Fatalf("sender kept %v", out)
		}
	})
	t.Run("receiver discards", func(t *testing.T) {
		a, b := trustedPair(t)
		src := writeTemp(t, "big.bin", data)
		interrupt(t, a, b, 300<<10, src)
		_, in := b.resumable()
		if len(in) != 1 {
			t.Fatalf("kept %v", in)
		}
		if code, body := b.call("POST", "/api/discard", map[string]any{"transfer_id": in[0]["id"]}, true); code != 200 {
			t.Fatalf("discard: %d %s", code, body)
		}
		b.waitNoState()
		if code, _ := b.call("POST", "/api/discard", map[string]any{"transfer_id": in[0]["id"]}, true); code != 404 {
			t.Fatalf("second discard: %d", code)
		}
	})
	t.Run("decline", func(t *testing.T) {
		a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
		src := writeTemp(t, "big.bin", data)
		p := newCutProxy(t, b.ts.URL)
		aEv := a.srv.hub.subscribe()
		defer a.srv.hub.unsubscribe(aEv)
		id := sendVia(t, a, b, p, src)
		b.call("POST", "/api/accept", map[string]any{"id": b.waitIncoming(1)[0].ID, "accept": false}, true)
		if typ, m := waitEnd(t, aEv, id); typ != "error" || m["result"] != history.ResultDeclined {
			t.Fatalf("sender saw %s %v", typ, m)
		}
		b.waitNoState()
		if out, _ := a.resumable(); len(out) != 0 {
			t.Fatalf("sender kept %v", out)
		}
	})
}

// TestStalledReceiveSettles: the old connection went silent (no reset), so
// the receiver still waits on it when the sender offers the same files
// again. The new offer stops the stalled receive, which keeps what it
// stored, and is answered with the resume point.
func TestStalledReceiveSettles(t *testing.T) {
	a, b := trustedPair(t)
	data := randomBytes(300 << 10)
	m := protocol.FileManifest("slow.bin", int64(len(data)), 0o644)
	_, reply, key := acceptedOffer(t, a, b, m)
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		req, _ := http.NewRequest("POST", b.ts.URL+"/api/recv?token="+reply.Token, pr)
		req.ContentLength = transfer.SealedBodySize(m, transfer.Start{})
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	// Two full frames, then silence.
	sealed := seal(t, key, append(plainBody(transfer.Start{}, ""), data...))
	if _, err := pw.Write(sealed[:2*(4+64<<10+16)]); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); len(b.transfers()) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the receive did not start")
		}
	}
	time.Sleep(200 * time.Millisecond) // let the two frames land
	o, _, err := protocol.NewOffer(a.id, "alice", b.id.Fingerprint(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, txt, r := postOffer(t, b, o)
	if code != 200 || !r.Accepted || r.Resume == nil {
		t.Fatalf("offer while stalled: %d %s %+v", code, txt, r)
	}
	if want := uint64(2*(64<<10) - transfer.StartHeaderSize); r.Resume.Offset != want || r.Resume.Entry != 0 {
		t.Fatalf("resume point %+v, want offset %d", r.Resume, want)
	}
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("answered after %s, before the old receive could settle", d)
	}
	if e := b.history(""); len(e) != 1 || e[0].Result != history.ResultInterrupted {
		t.Fatalf("history %+v", e)
	}
}

// TestCancelNoticeChecks: the cancel notice must be signed by the sender of
// that transfer; an unknown transfer is 404.
func TestCancelNoticeChecks(t *testing.T) {
	a, b := trustedPair(t)
	mallory := newTestHost(t, "mallory")
	src := writeTemp(t, "x.bin", randomBytes(300<<10))
	interrupt(t, a, b, 100<<10, src)
	_, in := b.resumable()
	if len(in) != 1 {
		t.Fatalf("kept %v", in)
	}
	kid := in[0]["id"].(string)
	post := func(c protocol.CancelNotice) int {
		body, _ := json.Marshal(c)
		resp, err := http.Post(b.ts.URL+"/api/recv/cancel", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(protocol.SignCancel(mallory.id, b.id.Fingerprint(), kid)); code != 403 {
		t.Fatalf("notice by another key: %d", code)
	}
	if code := post(protocol.SignCancel(a.id, mallory.id.Fingerprint(), kid)); code != 403 {
		t.Fatalf("notice for another receiver: %d", code)
	}
	if code := post(protocol.SignCancel(a.id, b.id.Fingerprint(), strings.Repeat("1", 32))); code != 404 {
		t.Fatalf("notice for an unknown transfer: %d", code)
	}
	if code := post(protocol.CancelNotice{TransferID: "x"}); code != 400 {
		t.Fatalf("malformed notice: %d", code)
	}
	if _, in := b.resumable(); len(in) != 1 {
		t.Fatal("a bad notice discarded the kept transfer")
	}
	if code := post(protocol.SignCancel(a.id, b.id.Fingerprint(), kid)); code != 200 {
		t.Fatalf("good notice: %d", code)
	}
	b.waitNoState()
}
