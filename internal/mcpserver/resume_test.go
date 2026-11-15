package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/httpapi"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/internal/resume"
	"github.com/donaldsteele/rocket/internal/transfer"
	"github.com/donaldsteele/rocket/web"
)

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %+v", name, err, res)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v in %s", name, err, b)
	}
}

// TestStatusReportsProtocolAndInterrupted: get_status and list_peers say
// which peers need an update, and get_status lists interrupted sends; a
// send to a peer that needs an update is refused before anything is sent.
func TestStatusReportsProtocolAndInterrupted(t *testing.T) {
	svc := testService(t)
	ob, err := resume.OpenOutbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.Outbox = ob
	if err := ob.Put(resume.Outgoing{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "photos", PeerName: "desk", Files: 3, Bytes: 10, Error: "transfer interrupted"}); err != nil {
		t.Fatal(err)
	}
	svc.Reg.Observe(discovery.ViaBeacon, discovery.Peer{Name: "new", Fingerprint: "1111111111111111", Addr: "127.0.0.1:1", Proto: 2})
	svc.Reg.Observe(discovery.ViaBeacon, discovery.Peer{Name: "old", Fingerprint: "2222222222222222", Addr: "127.0.0.1:2", Proto: 1})
	cs := connect(t, svc)

	var st statusOut
	call(t, cs, "get_status", nil, &st)
	if st.Protocol != 2 || !st.Encrypted || st.PeerCount != 2 || st.PeersNeedingUpdate != 1 {
		t.Fatalf("status %+v", st)
	}
	if len(st.Interrupted) != 1 || st.Interrupted[0].ID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || st.Interrupted[0].Name != "photos" {
		t.Fatalf("interrupted %+v", st.Interrupted)
	}

	var lp listPeersOut
	call(t, cs, "list_peers", nil, &lp)
	for _, p := range lp.Peers {
		if p.NeedsUpdate != (p.Name == "old") || (p.Name == "old" && p.Proto != 1) {
			t.Errorf("peer %+v", p)
		}
	}

	f := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sf sendFileOut
	call(t, cs, "send_file", map[string]any{"peer": "old", "paths": []string{f}}, &sf)
	if sf.Sent || !strings.Contains(sf.Note, "older rocket (protocol 1)") {
		t.Fatalf("send to an old peer: %+v", sf)
	}
}

// TestListHistoryReportsEncryptedAndInterrupted: the history entries carry
// encrypted and the interrupted result.
func TestListHistoryReportsEncryptedAndInterrupted(t *testing.T) {
	svc := testService(t)
	if err := svc.History.Add(history.Entry{ID: "a", Dir: history.DirOut, Name: "f", PeerName: "bob", Result: history.ResultInterrupted, Encrypted: true}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, svc)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_history", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if !bytes.Contains(b, []byte(`"result":"interrupted"`)) || !bytes.Contains(b, []byte(`"encrypted":true`)) {
		t.Fatalf("list_history: %s", b)
	}
}

// cutProxy forwards TCP to target and, once, closes a connection after it
// has carried more than cutAfter bytes from the client.
type cutProxy struct {
	ln       net.Listener
	cutAfter int64
	cut      atomic.Bool
	wg       sync.WaitGroup
	mu       sync.Mutex
	conns    map[net.Conn]bool
}

func newCutProxy(t *testing.T, target string, cutAfter int64) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, cutAfter: cutAfter, conns: map[net.Conn]bool{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns[c] = true
			p.mu.Unlock()
			p.wg.Add(1)
			go p.serve(c, target)
		}
	}()
	// Idle keep-alive connections would otherwise stay open until the
	// client gives up on them.
	t.Cleanup(func() {
		ln.Close()
		p.mu.Lock()
		for c := range p.conns {
			c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}

func (p *cutProxy) serve(c net.Conn, target string) {
	defer p.wg.Done()
	s, err := net.Dial("tcp", target)
	if err != nil {
		c.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(c, s); done <- struct{}{} }()
	go func() {
		var n int64
		buf := make([]byte, 32<<10)
		for {
			k, err := c.Read(buf)
			if k > 0 {
				if _, werr := s.Write(buf[:k]); werr != nil {
					break
				}
				n += int64(k)
				if n > p.cutAfter && p.cut.CompareAndSwap(false, true) {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
	c.Close()
	s.Close()
	<-done
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// TestSendFileReconnects: send_file survives a cut connection, reports the
// retry, and the file arrives whole (the second attempt resumes).
func TestSendFileReconnects(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a retry delay")
	}
	svc := testService(t)
	ob, err := resume.OpenOutbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.Outbox = ob

	// The receiver: a real rocket host that trusts the sender's key.
	tmp := t.TempDir()
	cfg := &config.Config{DataDir: filepath.Join(tmp, "data"), DownloadDir: filepath.Join(tmp, "dl"), HTTPPort: 29190, BeaconPort: 29191}
	for _, d := range []string{cfg.DataDir, cfg.DownloadDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rid, err := node.Load(cfg.DataDir, "desk")
	if err != nil {
		t.Fatal(err)
	}
	trust, err := peers.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.Approve(svc.ID.Fingerprint(), svc.ID.Name, svc.ID.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	srv, err := httpapi.New(cfg, rid, trust, discovery.New(discovery.Self{Name: "desk", Fingerprint: rid.Fingerprint()}, cfg.BeaconPort), web.FS())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	proxy := newCutProxy(t, strings.TrimPrefix(ts.URL, "http://"), 1<<20)
	svc.Reg.Observe(discovery.ViaBeacon, discovery.Peer{Name: "desk", Fingerprint: rid.Fingerprint(), Addr: proxy.ln.Addr().String(), Proto: 2})

	data := bytes.Repeat([]byte("rocket resume "), 300_000) // about 4 MB
	src := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// An earlier interrupted send of the same file to the same peer: the
	// completed send leaves nothing to resume.
	plan, err := transfer.PlanPaths([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	mh, _ := plan.Manifest.Hash()
	if err := ob.Put(resume.Outgoing{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Name: "big.bin", ManifestHash: mh, PeerFP: rid.Fingerprint(), Paths: []string{src}}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, svc)
	var sf sendFileOut
	call(t, cs, "send_file", map[string]any{"peer": "desk", "paths": []string{src}}, &sf)
	if !sf.Sent || !sf.Encrypted || sf.Retries < 1 || sf.Interrupted {
		t.Fatalf("send_file %+v", sf)
	}
	if !proxy.cut.Load() {
		t.Fatal("the proxy never cut the connection")
	}
	got, err := os.ReadFile(filepath.Join(cfg.DownloadDir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("received %d bytes that differ from the %d sent", len(got), len(data))
	}
	list, err := svc.History.List(5)
	if err != nil || len(list) != 1 || list[0].Result != history.ResultOK || !list[0].Encrypted {
		t.Fatalf("history %+v %v", list, err)
	}
	if n := len(ob.List()); n != 0 {
		t.Fatalf("%d sends left in the outbox", n)
	}
	var st statusOut
	call(t, cs, "get_status", nil, &st)
	if len(st.Transfers) != 0 {
		t.Fatalf("transfers still listed: %+v", st.Transfers)
	}
}
