package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/web"
)

// testHost is one rocket host under test: its own temp data and download
// dirs, identity, trust store and an httptest server.
type testHost struct {
	t   *testing.T
	cfg *config.Config
	id  *node.Identity
	srv *Server
	ts  *httptest.Server
}

func newTestHost(t *testing.T, name string) *testHost {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		DataDir:     filepath.Join(tmp, "data"),
		DownloadDir: filepath.Join(tmp, "downloads"),
		HTTPPort:    29100,
		BeaconPort:  29101,
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
	srv, err := New(cfg, id, trust, reg, web.FS())
	if err != nil {
		t.Fatal(err)
	}
	h := &testHost{t: t, cfg: cfg, id: id, srv: srv}
	h.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(h.ts.Close)
	return h
}

// addr is host:port of the test server.
func (h *testHost) addr() string { return strings.TrimPrefix(h.ts.URL, "http://") }

// call makes a request from this computer (loopback, real Host header). csrf
// adds the X-Rocket-CSRF header.
func (h *testHost) call(method, path string, body any, csrf bool) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(CSRFHeader, "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// waitIncoming polls GET /api/incoming until n offers are pending.
func (h *testHost) waitIncoming(n int) []IncomingOffer {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, b := h.call("GET", "/api/incoming", nil, false)
		if code != 200 {
			h.t.Fatalf("incoming: %d %s", code, b)
		}
		var list []IncomingOffer
		if err := json.Unmarshal(b, &list); err != nil {
			h.t.Fatal(err)
		}
		if len(list) >= n {
			return list
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %d incoming offers", n)
	return nil
}

// downloads lists file names in the download dir.
func (h *testHost) downloads() []string {
	entries, _ := os.ReadDir(h.cfg.DownloadDir)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
