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
// connection retries; the phone keeps nothing of the broken attempt (its
// sink removes it) and receives the whole file on the retry.
func TestDesktopRetriesToPhone(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	if err := phone.trust.Approve(desk.id.Fingerprint(), "desk", desk.id.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	sink := newTempSink(t, true)
	port, err := phone.StartReceiver(0, &decider{n: phone, accept: true}, sink, newEvents(t))
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
	if begun, aborted := sink.counts(); begun != 2 || aborted != 1 {
		t.Fatalf("sink begun %d, aborted %d", begun, aborted)
	}
	if _, resumes := p.seen(); len(resumes) != 2 || resumes[1] != nil {
		t.Fatalf("the phone offered a resume point: %v", resumes)
	}
}
