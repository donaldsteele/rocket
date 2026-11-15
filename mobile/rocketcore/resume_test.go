package rocketcore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// cutProxy relays HTTP to target and drops the connection of the first
// POST /api/recv after cut body bytes, recording the Content-Length of
// every upload and the resume point of every accepted reply.
type cutProxy struct {
	ts      *httptest.Server
	mu      sync.Mutex
	cut     int64
	lengths []int64
	resumes []*protocol.ResumePoint
}

var errCut = errors.New("cut by the test proxy")

func newCutProxy(t *testing.T, target string, cut int64) *cutProxy {
	p := &cutProxy{cut: cut}
	u, _ := url.Parse(target)
	rp := httputil.NewSingleHostReverseProxy(u)
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
			p.resumes = append(p.resumes, r.Resume)
			p.mu.Unlock()
		}
		return nil
	}
	p.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/recv" {
			p.mu.Lock()
			first := len(p.lengths) == 0
			p.lengths = append(p.lengths, r.ContentLength)
			p.mu.Unlock()
			if first {
				r.Body = &cutBody{rc: r.Body, left: cut}
			}
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.ts.Close)
	return p
}

func (p *cutProxy) addr() string { return strings.TrimPrefix(p.ts.URL, "http://") }

func (p *cutProxy) seen() ([]int64, []*protocol.ResumePoint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.lengths...), append([]*protocol.ResumePoint(nil), p.resumes...)
}

type cutBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *cutBody) Read(buf []byte) (int, error) {
	if b.left <= 0 {
		return 0, errCut
	}
	if int64(len(buf)) > b.left {
		buf = buf[:b.left]
	}
	n, err := b.rc.Read(buf)
	b.left -= int64(n)
	return n, err
}

func (b *cutBody) Close() error { return b.rc.Close() }

func randomData(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// checkResumed checks that the second upload carried only the body from
// the resume point offered for entry 0 of a one-file manifest of size.
func checkResumed(t *testing.T, p *cutProxy, name string, size int64) {
	t.Helper()
	lengths, resumes := p.seen()
	if len(lengths) != 2 || len(resumes) != 2 || resumes[0] != nil || resumes[1] == nil {
		t.Fatalf("uploads %v, resume points %v", lengths, resumes)
	}
	m := protocol.FileManifest(name, size, 0o644)
	rp := resumes[1]
	if rp.Entry != 0 || rp.Offset == 0 || rp.Offset >= uint64(size) {
		t.Fatalf("resume point %+v", rp)
	}
	if want := transfer.SealedBodySize(m, transfer.Start{Offset: rp.Offset}); lengths[1] != want {
		t.Fatalf("second upload %d bytes, want %d", lengths[1], want)
	}
}

// TestPhoneSendResumesAfterCut: a phone's send through a connection that
// drops mid-file retries on its own and the desktop continues where it
// stopped, without asking its person again.
func TestPhoneSendResumesAfterCut(t *testing.T) {
	for _, fd := range []bool{true, false} {
		phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
		phone.retryDelays = []time.Duration{20 * time.Millisecond, 20 * time.Millisecond}
		data := randomData(700 << 10)
		p := newCutProxy(t, desk.ts.URL, 250<<10)
		ev := newEvents(t)
		if _, err := phone.Send(p.addr(), desk.id.Fingerprint(), &testSource{t: t, items: []item{
			{path: "clip.mp4", data: data, fd: fd},
		}}, ev); err != nil {
			t.Fatal(err)
		}
		desk.call("POST", "/api/accept", map[string]any{"id": desk.waitIncoming().ID, "accept": true})
		if r := ev.wait("retrying"); r.Data["attempt"] != float64(1) || r.Data["attempts"] != float64(2) {
			t.Fatalf("retrying %v", r.Data)
		}
		ev.wait("sent")
		sameTree(t, snapshot(t, desk.cfg.DownloadDir), map[string]string{"clip.mp4": string(data)})
		checkResumed(t, p, "clip.mp4", int64(len(data)))
		if j := phone.ResumableJSON(); j != `{"incoming":[],"outgoing":[]}` {
			t.Fatalf("resumable %s", j)
		}
	}
}

// resumable decodes ResumableJSON.
func resumable(t *testing.T, n *Node) []map[string]any {
	t.Helper()
	var r struct {
		Outgoing []map[string]any `json:"outgoing"`
		Incoming []map[string]any `json:"incoming"`
	}
	if err := json.Unmarshal([]byte(n.ResumableJSON()), &r); err != nil {
		t.Fatal(err)
	}
	return r.Outgoing
}

// deskKept waits until the desktop keeps n transfers for a resume.
func (d *desktop) waitKept(n int) {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, b := d.call("GET", "/api/resumable", nil)
		var r struct {
			Incoming []map[string]any `json:"incoming"`
		}
		_ = json.Unmarshal(b, &r)
		if len(r.Incoming) == n {
			return
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("desktop keeps %s, want %d", b, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPhoneResumeAndDiscard: with no retries left the send ends
// interrupted and is listed by ResumableJSON; Resume continues it (with a
// Source made again, as after an app restart); Discard of another forgets
// it and makes the desktop drop what it kept.
func TestPhoneResumeAndDiscard(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	if err := desk.trust.Approve(phone.Fingerprint(), "pixel", phone.PublicKey()); err != nil {
		t.Fatal(err)
	}
	phone.retryDelays = []time.Duration{}
	data := randomData(500 << 10)
	src := func() Source { return &testSource{t: t, items: []item{{path: "report.pdf", data: data, fd: true}}} }
	p := newCutProxy(t, desk.ts.URL, 200<<10)
	ev := newEvents(t)
	tid, err := phone.Send(p.addr(), desk.id.Fingerprint(), src(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if e := ev.wait("error"); e.Data["result"] != "interrupted" || e.Data["transfer_id"] != tid {
		t.Fatalf("end %v", e.Data)
	}
	out := resumable(t, phone)
	if len(out) != 1 || out[0]["transfer_id"] != tid || out[0]["name"] != "report.pdf" || out[0]["peer_fp"] != desk.id.Fingerprint() {
		t.Fatalf("resumable %v", out)
	}
	desk.waitKept(1)
	if _, err := phone.Resume(strings.Repeat("0", 32), src(), ev); err == nil {
		t.Fatal("resumed an unknown send")
	}
	ev = newEvents(t)
	nid, err := phone.Resume(tid, src(), ev)
	if err != nil || nid == tid {
		t.Fatalf("resume: %q %v", nid, err)
	}
	if s := ev.wait("sent"); s.Data["transfer_id"] != nid {
		t.Fatalf("sent %v", s.Data)
	}
	sameTree(t, snapshot(t, desk.cfg.DownloadDir), map[string]string{"report.pdf": string(data)})
	checkResumed(t, p, "report.pdf", int64(len(data)))
	if out := resumable(t, phone); len(out) != 0 {
		t.Fatalf("still resumable %v", out)
	}
	desk.waitKept(0)

	// Discard: another interrupted send; the desktop drops its part file.
	p2 := newCutProxy(t, desk.ts.URL, 100<<10)
	ev = newEvents(t)
	tid, err = phone.Send(p2.addr(), desk.id.Fingerprint(), &testSource{t: t, items: []item{{path: "other.bin", data: data}}}, ev)
	if err != nil {
		t.Fatal(err)
	}
	ev.wait("error")
	desk.waitKept(1)
	if err := phone.Discard(tid); err != nil {
		t.Fatal(err)
	}
	if err := phone.Discard(tid); err == nil {
		t.Fatal("discarded twice")
	}
	desk.waitKept(0)
	waitNames(t, desk.cfg.DownloadDir, []string{"report.pdf"})
}

// waitNames waits until root holds exactly want (top-level names).
func waitNames(t *testing.T, root string, want []string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := names(t, root)
		if strings.Join(got, "|") == strings.Join(want, "|") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("names %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDesktopRetriesToPhone: a desktop send to a phone through a dropping
// connection retries; the phone keeps what arrived of the broken attempt
// (its sink's Keep) and continues the same file from the resume point it
// offers, so only the rest of the body is sent again. Both ways of
// writing (a descriptor, or Write) are kept and continued.
func TestDesktopRetriesToPhone(t *testing.T) {
	for _, useFd := range []bool{true, false} {
		phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
		if err := phone.trust.Approve(desk.id.Fingerprint(), "desk", desk.id.PublicKeyB64()); err != nil {
			t.Fatal(err)
		}
		sink := newTempSink(t, useFd)
		ev := newEvents(t)
		port, err := phone.StartReceiver(0, &decider{n: phone, accept: true}, sink, ev)
		if err != nil {
			t.Fatal(err)
		}
		data := randomData(600 << 10)
		p := newCutProxy(t, "http://127.0.0.1:"+strconv.Itoa(port), 200<<10)
		m := protocol.FileManifest("song.flac", int64(len(data)), 0o644)
		open := func(int, protocol.Entry) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
		err = transfer.SendManifestOpts(context.Background(), desk.id, transfer.Target{Addr: p.addr(), FP: phone.Fingerprint()}, m, open, nil,
			transfer.SendOptions{Retry: true, Delays: []time.Duration{20 * time.Millisecond}})
		if err != nil {
			t.Fatal(err)
		}
		if got := sink.tree(); len(got) != 1 || got["song.flac"] != string(data) {
			t.Fatalf("phone stored %v", keys(got))
		}
		sink.mu.Lock()
		begun, aborted, kept, resumed := sink.begun, sink.aborted, sink.kept, sink.resumed
		sink.mu.Unlock()
		if begun != 1 || aborted != 0 || kept != 1 || resumed != 1 {
			t.Fatalf("sink begun %d, aborted %d, kept %d, resumed %d", begun, aborted, kept, resumed)
		}
		checkResumed(t, p, "song.flac", int64(len(data)))
		ev.skipTo("kept")
		if g := ev.skipTo("kept_gone"); g.Data["reason"] != "completed" {
			t.Fatalf("kept_gone %v", g.Data)
		}
		if j := phone.ResumableJSON(); j != `{"incoming":[],"outgoing":[]}` {
			t.Fatalf("resumable %s", j)
		}
	}
}

// skipTo returns the next event of type typ, skipping every other.
func (e *events) skipTo(typ string) event {
	e.t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev := <-e.ch:
			if ev.Type == typ {
				return ev
			}
		case <-timeout:
			e.t.Fatalf("no %q event", typ)
			return event{}
		}
	}
}

// incoming decodes ResumableJSON's kept receives.
func incoming(t *testing.T, n *Node) []map[string]any {
	t.Helper()
	var r struct {
		Incoming []map[string]any `json:"incoming"`
	}
	if err := json.Unmarshal([]byte(n.ResumableJSON()), &r); err != nil {
		t.Fatal(err)
	}
	return r.Incoming
}

// waitIncoming waits until n keeps k receives and returns them.
func waitIncoming(t *testing.T, n *Node, k int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		in := incoming(t, n)
		if len(in) == k {
			return in
		}
		if time.Now().After(deadline) {
			t.Fatalf("kept %v, want %d", in, k)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPhoneKeepsAcrossRestartAndDiscards: a receive broken mid-folder is
// kept through a receiver restart and continued by the sender's next
// offer without a prompt (the sender is not trusted); another kept
// receive is removed with its files by Discard.
func TestPhoneKeepsAcrossRestartAndDiscards(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	sink := newTempSink(t, true)
	dec := &decider{n: phone, accept: true}
	port, err := phone.StartReceiver(0, dec, sink, newEvents(t))
	if err != nil {
		t.Fatal(err)
	}
	a, b := randomData(150<<10), randomData(400<<10)
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "album", Mode: protocol.ModeDir | 0o755},
		{Path: "album/a.jpg", Size: int64(len(a)), Mode: 0o644},
		{Path: "album/b.jpg", Size: int64(len(b)), Mode: 0o644},
	}, Total: int64(len(a) + len(b))}
	open := func(i int, _ protocol.Entry) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([][]byte{nil, a, b}[i])), nil
	}
	send := func(addr string) error {
		return transfer.SendManifestOpts(context.Background(), desk.id, transfer.Target{Addr: addr, FP: phone.Fingerprint()}, m, open, nil,
			transfer.SendOptions{Retry: true, Delays: []time.Duration{}})
	}
	p := newCutProxy(t, "http://127.0.0.1:"+strconv.Itoa(port), 300<<10)
	if err := send(p.addr()); !errors.Is(err, transfer.ErrInterrupted) {
		t.Fatalf("first attempt: %v", err)
	}
	in := waitIncoming(t, phone, 1)
	if len(in) != 1 || in[0]["name"] != "album" || in[0]["sender_name"] != "desk" || in[0]["total"] != float64(m.Total) {
		t.Fatalf("incoming %v", in)
	}
	if done := in[0]["done"].(float64); done <= float64(len(a)) || done >= float64(m.Total) {
		t.Fatalf("done %v", done)
	}
	phone.StopReceiver()
	if in := incoming(t, phone); len(in) != 1 {
		t.Fatalf("kept receive not listed with the receiver off: %v", in)
	}
	port, err = phone.StartReceiver(0, dec, sink, newEvents(t))
	if err != nil {
		t.Fatal(err)
	}
	p2 := newCutProxy(t, "http://127.0.0.1:"+strconv.Itoa(port), 1<<30)
	if err := send(p2.addr()); err != nil {
		t.Fatal(err)
	}
	if dec.count() != 1 {
		t.Fatalf("asked %d times, want only the first offer", dec.count())
	}
	if _, resumes := p2.seen(); len(resumes) != 1 || resumes[0] == nil || resumes[0].Entry != 2 {
		t.Fatalf("resume point %v", resumes)
	}
	sameTree(t, sink.tree(), map[string]string{"album": "<dir>", "album/a.jpg": string(a), "album/b.jpg": string(b)})
	if len(incoming(t, phone)) != 0 {
		t.Fatal("still kept after completing")
	}

	// Discard: a kept receive goes with its files.
	p3 := newCutProxy(t, "http://127.0.0.1:"+strconv.Itoa(port), 100<<10)
	m.Entries[0].Path, m.Entries[1].Path, m.Entries[2].Path = "other", "other/a.jpg", "other/b.jpg"
	if err := send(p3.addr()); !errors.Is(err, transfer.ErrInterrupted) {
		t.Fatalf("third attempt: %v", err)
	}
	in = waitIncoming(t, phone, 1)
	id, _ := in[0]["id"].(string)
	if err := phone.Discard(id); err != nil {
		t.Fatal(err)
	}
	if err := phone.Discard(id); err == nil {
		t.Fatal("discarded twice")
	}
	waitNames(t, sink.root, []string{"album", "album/a.jpg", "album/b.jpg"})
}

// TestPhoneResumeBeforeAcceptKeepsSend: a Resume while the desktop is away
// fails before the desktop accepts it, and the interrupted send stays
// listed (it is hidden only while the Resume runs), so it can be resumed or
// discarded later.
func TestPhoneResumeBeforeAcceptKeepsSend(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	if err := desk.trust.Approve(phone.Fingerprint(), "pixel", phone.PublicKey()); err != nil {
		t.Fatal(err)
	}
	phone.retryDelays = []time.Duration{}
	data := randomData(300 << 10)
	src := func() Source { return &testSource{t: t, items: []item{{path: "clip.bin", data: data, fd: true}}} }
	p := newCutProxy(t, desk.ts.URL, 100<<10)
	ev := newEvents(t)
	tid, err := phone.Send(p.addr(), "", src(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if e := ev.wait("error"); e.Data["result"] != "interrupted" {
		t.Fatalf("end %v", e.Data)
	}
	desk.waitKept(1)
	out := resumable(t, phone)
	if len(out) != 1 || out[0]["receiver_fp"] != desk.id.Fingerprint() {
		t.Fatalf("resumable %v", out)
	}

	p.ts.Close() // the desktop is away
	ev = newEvents(t)
	nid, err := phone.Resume(tid, src(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if e := ev.wait("error"); e.Data["transfer_id"] != nid || e.Data["result"] == "interrupted" {
		t.Fatalf("end %v", e.Data)
	}
	if out := resumable(t, phone); len(out) != 1 || out[0]["transfer_id"] != tid {
		t.Fatalf("after a resume that was never accepted: %v", out)
	}
}
