package rocketcore

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/httpapi"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/web"
)

// desktop is a desktop rocket host (internal/httpapi) on an httptest server,
// with its own temp data and download folders.
type desktop struct {
	t     *testing.T
	cfg   *config.Config
	id    *node.Identity
	trust *peers.Store
	ts    *httptest.Server
}

func newDesktop(t *testing.T, name string) *desktop {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		DataDir:     filepath.Join(tmp, "data"),
		DownloadDir: filepath.Join(tmp, "downloads"),
		HTTPPort:    29400,
		BeaconPort:  29401,
	}
	for _, d := range []string{cfg.DataDir, cfg.DownloadDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	id, err := node.Load(cfg.DataDir, name)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := peers.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	reg := discovery.New(discovery.Self{Name: name, Fingerprint: id.Fingerprint()}, cfg.BeaconPort)
	srv, err := httpapi.New(cfg, id, trust, reg, web.FS())
	if err != nil {
		t.Fatal(err)
	}
	d := &desktop{t: t, cfg: cfg, id: id, trust: trust, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(d.ts.Close)
	return d
}

func (d *desktop) addr() string { return strings.TrimPrefix(d.ts.URL, "http://") }

// call makes a loopback control-API request (with the CSRF header).
func (d *desktop) call(method, path string, body any) (int, []byte) {
	d.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, d.ts.URL+path, rd)
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpapi.CSRFHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// waitIncoming waits for an offer to wait for a decision on the desktop.
func (d *desktop) waitIncoming() httpapi.IncomingOffer {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, b := d.call("GET", "/api/incoming", nil)
		var list []httpapi.IncomingOffer
		_ = json.Unmarshal(b, &list)
		if len(list) > 0 {
			return list[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.t.Fatal("no offer reached the desktop")
	return httpapi.IncomingOffer{}
}

// newPhone makes a Node in a temp folder with the beacon off.
func newPhone(t *testing.T, name string) *Node {
	t.Helper()
	n, err := NewNode(filepath.Join(t.TempDir(), "files"), name, "test")
	if err != nil {
		t.Fatal(err)
	}
	n.SetBeaconPort(0)
	t.Cleanup(n.StopReceiver)
	return n
}

// event is a decoded event JSON.
type event struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// events collects JSON events (ProgressListener, EventListener).
type events struct {
	t  *testing.T
	ch chan event
}

func newEvents(t *testing.T) *events { return &events{t: t, ch: make(chan event, 4096)} }

func (e *events) OnEvent(s string) {
	var ev event
	if err := json.Unmarshal([]byte(s), &ev); err != nil {
		e.t.Errorf("bad event JSON %q: %v", s, err)
		return
	}
	select {
	case e.ch <- ev:
	default:
	}
}

// wait returns the next event of type typ, failing on an unexpected end
// ("error" or "cancelled" when typ is neither).
func (e *events) wait(typ string) event {
	e.t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev := <-e.ch:
			if ev.Type == typ {
				return ev
			}
			if (ev.Type == "error" || ev.Type == "cancelled") && typ != "error" && typ != "cancelled" {
				e.t.Fatalf("waiting for %q, got %s %v", typ, ev.Type, ev.Data)
			}
		case <-timeout:
			e.t.Fatalf("no %q event", typ)
			return event{}
		}
	}
}

// item is one entry of a testSource.
type item struct {
	path    string
	data    []byte
	dir     bool
	fd      bool // hand out a file descriptor (else chunks)
	noSize  bool // report the size as unknown (needs fd)
	slow    bool // sleep between chunks
	failGet bool // OpenFd fails
}

// testSource is a Source as the Android app would supply it.
type testSource struct {
	t     *testing.T
	items []item
}

func (s *testSource) Count() int        { return len(s.items) }
func (s *testSource) Path(i int) string { return s.items[i].path }
func (s *testSource) IsDir(i int) bool  { return s.items[i].dir }
func (s *testSource) Size(i int) int64 {
	if s.items[i].noSize {
		return -1
	}
	return int64(len(s.items[i].data))
}

func (s *testSource) OpenFd(i int) (int, error) {
	it := s.items[i]
	if it.failGet {
		return -1, os.ErrPermission
	}
	if !it.fd {
		return -1, nil
	}
	p := filepath.Join(s.t.TempDir(), "src")
	if err := os.WriteFile(p, it.data, 0o644); err != nil {
		return -1, err
	}
	f, err := os.Open(p)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	return dupFd(s.t, f), nil
}

func (s *testSource) ReadChunk(i int, off int64, max int) ([]byte, error) {
	it := s.items[i]
	if it.slow {
		time.Sleep(5 * time.Millisecond)
	}
	if off >= int64(len(it.data)) {
		return nil, nil
	}
	end := min(off+int64(max), int64(len(it.data)))
	return append([]byte(nil), it.data[off:end]...), nil
}

// tempSink is a Sink over a temp folder, as the Android app's MediaStore
// sink behaves: fresh top-level names, never overwriting, abort removes all.
type tempSink struct {
	t       *testing.T
	root    string
	useFd   bool
	mu      sync.Mutex
	begun   int
	aborted int
	kept    int
	resumed int
}

func newTempSink(t *testing.T, useFd bool) *tempSink {
	return &tempSink{t: t, root: t.TempDir(), useFd: useFd}
}

func (s *tempSink) Begin(id, manifestJSON string) (SinkTransfer, error) {
	var m struct {
		Kind    string `json:"kind"`
		Entries []struct {
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil || len(m.Entries) == 0 {
		s.t.Errorf("bad manifest JSON %q: %v", manifestJSON, err)
	}
	s.mu.Lock()
	s.begun++
	s.mu.Unlock()
	return &tempTransfer{s: s}, nil
}

// tempState is what a tempTransfer's Keep returns.
type tempState struct {
	Tops []string `json:"tops"`
}

func (s *tempSink) Resume(id, manifestJSON, state string) (SinkTransfer, error) {
	var st tempState
	if err := json.Unmarshal([]byte(state), &st); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.resumed++
	s.mu.Unlock()
	return &tempTransfer{s: s, tops: st.Tops}, nil
}

func (s *tempSink) Check(state string) error {
	var st tempState
	if err := json.Unmarshal([]byte(state), &st); err != nil {
		return err
	}
	for _, p := range st.Tops {
		_, err := os.Lstat(p)
		if _, perr := os.Lstat(p + ".part"); err != nil && perr != nil {
			return err
		}
	}
	return nil
}

func (s *tempSink) Discard(state string) {
	var st tempState
	if json.Unmarshal([]byte(state), &st) == nil {
		removeTops(st.Tops)
	}
}

func removeTops(tops []string) {
	for _, p := range tops {
		os.RemoveAll(p)
		os.Remove(p + ".part")
	}
}

func (s *tempSink) counts() (begun, aborted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.begun, s.aborted
}

// tree lists the files (with content) and folders under the sink root.
func (s *tempSink) tree() map[string]string {
	return snapshot(s.t, s.root)
}

type tempTransfer struct {
	s    *tempSink
	tops []string // top-level items made, on disk
	open []*tempFile
}

// fresh returns p, or p with " (n)" before the extension, that does not
// exist yet.
func fresh(p string, dir bool) string {
	ext := ""
	if !dir {
		ext = filepath.Ext(p)
	}
	stem := p[:len(p)-len(ext)]
	for n := 0; ; n++ {
		c := p
		if n > 0 {
			c = stem + " (" + strconv.Itoa(n) + ")" + ext
		}
		if _, err := os.Lstat(c); os.IsNotExist(err) {
			if _, err := os.Lstat(c + ".part"); os.IsNotExist(err) {
				return c
			}
		}
	}
}

func (x *tempTransfer) Mkdir(rel string) (string, error) {
	if strings.Contains(rel, "\\") || strings.Contains(rel, "..") {
		x.s.t.Errorf("unsafe folder %q", rel)
	}
	p := filepath.Join(x.s.root, filepath.FromSlash(rel))
	if !strings.Contains(rel, "/") {
		p = fresh(p, true)
		x.tops = append(x.tops, p)
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	r, _ := filepath.Rel(x.s.root, p)
	return filepath.ToSlash(r), nil
}

func (x *tempTransfer) Create(rel string, size int64) (SinkFile, error) {
	if strings.Contains(rel, "\\") || strings.Contains(rel, "..") {
		x.s.t.Errorf("unsafe file %q", rel)
	}
	final := fresh(filepath.Join(x.s.root, filepath.FromSlash(rel)), false)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(final+".part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(rel, "/") {
		x.tops = append(x.tops, final)
	}
	tf := &tempFile{x: x, f: f, final: final, size: size}
	x.open = append(x.open, tf)
	return tf, nil
}

func (x *tempTransfer) Abort() {
	removeTops(x.tops)
	x.s.mu.Lock()
	x.s.aborted++
	x.s.mu.Unlock()
}

func (x *tempTransfer) Keep() (string, error) {
	for _, f := range x.open {
		if f.f != nil {
			f.f.Close()
			f.f = nil
		}
	}
	x.s.mu.Lock()
	x.s.kept++
	x.s.mu.Unlock()
	b, err := json.Marshal(tempState{Tops: x.tops})
	return string(b), err
}

func (x *tempTransfer) Reopen(ref string) (SinkFile, error) {
	f, err := os.OpenFile(ref+".part", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	tf := &tempFile{x: x, f: f, final: ref, size: -1}
	x.open = append(x.open, tf)
	return tf, nil
}

func (x *tempTransfer) Location() string { return x.s.root }

type tempFile struct {
	x     *tempTransfer
	f     *os.File
	final string
	size  int64
}

func (f *tempFile) Ref() string { return f.final }

func (f *tempFile) Fd() int {
	if !f.x.s.useFd && f.size >= 0 {
		return -1
	}
	fd := dupFd(f.x.s.t, f.f)
	f.f.Close()
	f.f = nil
	return fd
}

func (f *tempFile) Write(b []byte) error {
	_, err := f.f.Write(b)
	return err
}

func (f *tempFile) Commit() (string, error) {
	if f.f != nil {
		if err := f.f.Close(); err != nil {
			return "", err
		}
	}
	if st, err := os.Stat(f.final + ".part"); err != nil || (f.size >= 0 && st.Size() != f.size) {
		f.x.s.t.Errorf("commit of %s: size %v, want %d (%v)", f.final, st, f.size, err)
	}
	return f.final, os.Rename(f.final+".part", f.final)
}

func (f *tempFile) Abort() {
	if f.f != nil {
		f.f.Close()
	}
	os.Remove(f.final + ".part")
}

// decider answers every offer as told, recording what it was asked.
type decider struct {
	n             *Node
	accept, trust bool
	silent        bool // never answer
	mu            sync.Mutex
	asked         []map[string]any
}

func (d *decider) Ask(s string) {
	var o map[string]any
	_ = json.Unmarshal([]byte(s), &o)
	d.mu.Lock()
	d.asked = append(d.asked, o)
	accept, trust, silent := d.accept, d.trust, d.silent
	d.mu.Unlock()
	if silent {
		return
	}
	id, _ := o["id"].(string)
	go d.n.Answer(id, accept, trust)
}

func (d *decider) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.asked)
}

// snapshot maps every path under root ("/"-separated) to its content, or
// "<dir>" for a folder.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == root {
			return err
		}
		r, _ := filepath.Rel(root, p)
		r = filepath.ToSlash(r)
		if info.IsDir() {
			out[r] = "<dir>"
			return nil
		}
		b, err := os.ReadFile(p)
		out[r] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tree = %v, want %v", keys(got), keys(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %.40q, want %.40q (tree %v)", k, got[k], v, keys(got))
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// waitEmpty waits until root holds nothing. It looks at names only: reading
// a part file the receiver is about to remove would race with the removal
// (and on Windows, holding it open makes the removal fail).
func waitEmpty(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(names(t, root)) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("left behind in %s: %v", root, names(t, root))
}

// names lists the paths under root, skipping entries that vanish while it
// walks.
func names(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p != root {
			return nil
		}
		if err != nil || p == root {
			return err
		}
		r, _ := filepath.Rel(root, p)
		out = append(out, filepath.ToSlash(r))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
