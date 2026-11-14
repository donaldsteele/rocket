package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRocket serves the three reads the tray polls plus /api/quit, checking
// the quit call the way rocket's guard does.
func fakeRocket(t *testing.T, peers, incoming int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var quits atomic.Int32
	list := func(n int) string {
		items := make([]string, n)
		for i := range items {
			items[i] = fmt.Sprintf(`{"id":"x%d"}`, i)
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/self", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"name":"desk","fingerprint":"0123456789abcdef","version":"1.0.0","port":29005}`)
	})
	mux.HandleFunc("GET /api/peers", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, list(peers)) })
	mux.HandleFunc("GET /api/incoming", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, list(incoming)) })
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Rocket-CSRF") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		quits.Add(1)
		fmt.Fprint(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &quits
}

func testClient(srv *httptest.Server) *Client {
	return &Client{Base: srv.URL, HTTP: srv.Client()}
}

func TestNewClientTargetsLoopback(t *testing.T) {
	c := NewClient(29123)
	if c.Base != "http://127.0.0.1:29123" {
		t.Fatalf("Base = %q", c.Base)
	}
	if c.UIURL() != "http://127.0.0.1:29123/" {
		t.Fatalf("UIURL = %q", c.UIURL())
	}
}

func TestStatusRunning(t *testing.T) {
	srv, _ := fakeRocket(t, 3, 2)
	st := testClient(srv).Status(context.Background())
	if st.Err != nil || !st.Running {
		t.Fatalf("status = %+v", st)
	}
	if st.Peers != 3 || st.Incoming != 2 || st.Name != "desk" || st.Version != "1.0.0" {
		t.Fatalf("status = %+v", st)
	}
}

func TestStatusNotRunning(t *testing.T) {
	srv, _ := fakeRocket(t, 0, 0)
	c := testClient(srv)
	srv.Close() // nothing listens any more
	if st := c.Status(context.Background()); st.Running || st.Err == nil {
		t.Fatalf("status = %+v, want not running with an error", st)
	}
}

func TestStatusRejectsOtherPrograms(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"not rocket": func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"hello":"world"}`) },
		"html":       func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `<html></html>`) },
		"forbidden":  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusForbidden) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if st := testClient(srv).Status(context.Background()); st.Running || st.Err == nil {
				t.Fatalf("status = %+v, want not running", st)
			}
		})
	}
}

func TestQuitSendsCSRFHeader(t *testing.T) {
	srv, quits := fakeRocket(t, 0, 0)
	if err := testClient(srv).Quit(context.Background()); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	if quits.Load() != 1 {
		t.Fatalf("quit calls = %d, want 1", quits.Load())
	}
}

func TestQuitReportsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	if err := testClient(srv).Quit(context.Background()); err == nil {
		t.Fatal("Quit succeeded against a server that refused it")
	}
}

func TestViewFor(t *testing.T) {
	cases := []struct {
		name     string
		st       Status
		starting bool
		startErr string
		want     View
	}{
		{
			name: "running, no offers",
			st:   Status{Running: true, Peers: 1},
			want: View{Status: "Running, 1 peer", Tooltip: "rocket: running, 1 peer", CanOpen: true, CanQuit: true},
		},
		{
			name: "running with offers",
			st:   Status{Running: true, Peers: 0, Incoming: 2},
			want: View{
				Status: "Running, 0 peers", Offers: "2 incoming offers", Tooltip: "rocket: 2 incoming offers waiting",
				Pending: true, CanOpen: true, CanQuit: true,
			},
		},
		{
			name: "one offer",
			st:   Status{Running: true, Peers: 4, Incoming: 1},
			want: View{
				Status: "Running, 4 peers", Offers: "1 incoming offer", Tooltip: "rocket: 1 incoming offer waiting",
				Pending: true, CanOpen: true, CanQuit: true,
			},
		},
		{
			name: "not running",
			st:   Status{Err: errors.New("refused")},
			want: View{Status: "Not running", Tooltip: "rocket: not running", CanStart: true},
		},
		{
			name:     "starting",
			st:       Status{Err: errors.New("refused")},
			starting: true,
			want:     View{Status: "Starting rocket...", Tooltip: "rocket: starting"},
		},
		{
			name:     "start failed",
			st:       Status{Err: errors.New("refused")},
			startErr: "rocket program not found",
			want:     View{Status: "Not running (rocket program not found)", Tooltip: "rocket: not running", CanStart: true},
		},
		{
			name:     "running wins over starting",
			st:       Status{Running: true, Peers: 2},
			starting: true,
			want:     View{Status: "Running, 2 peers", Tooltip: "rocket: running, 2 peers", CanOpen: true, CanQuit: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := viewFor(tc.st, tc.starting, tc.startErr); got != tc.want {
				t.Fatalf("viewFor =\n %+v\nwant\n %+v", got, tc.want)
			}
		})
	}
}

func TestStartErrText(t *testing.T) {
	if got := startErrText(errNoRocket); got != "rocket program not found" {
		t.Fatalf("got %q", got)
	}
	if got := startErrText(errors.New("exec: permission denied")); got != "could not start rocket" {
		t.Fatalf("got %q", got)
	}
}

func TestFindRocket(t *testing.T) {
	dir := filepath.Join("opt", "rocket")
	exe := filepath.Join(dir, "rocket-tray.exe")
	noPath := func(string) (string, error) { return "", errors.New("not on PATH") }

	files := map[string]bool{filepath.Join(dir, "rocket.exe"): true}
	isFile := func(p string) bool { return files[p] }

	got, err := findRocket(exe, "windows", isFile, noPath)
	if err != nil || got != filepath.Join(dir, "rocket.exe") {
		t.Fatalf("sibling: got %q, %v", got, err)
	}

	// The Linux name has no extension, so the Windows sibling does not count.
	onPath := func(name string) (string, error) { return "/usr/local/bin/" + name, nil }
	got, err = findRocket(exe, "linux", isFile, onPath)
	if err != nil || got != "/usr/local/bin/rocket" {
		t.Fatalf("PATH fallback: got %q, %v", got, err)
	}

	if _, err := findRocket(exe, "linux", isFile, noPath); !errors.Is(err, errNoRocket) {
		t.Fatalf("missing: err = %v, want errNoRocket", err)
	}
}

func TestRocketArgs(t *testing.T) {
	want := []string{"ui", "--no-browser", "--port", "29110"}
	if got := rocketArgs(29110); !reflect.DeepEqual(got, want) {
		t.Fatalf("rocketArgs = %q, want %q", got, want)
	}
}

func TestIconBytes(t *testing.T) {
	ico := []byte{0, 0, 1, 0}
	png := []byte("\x89PNG\r\n\x1a\n")
	for _, pending := range []bool{false, true} {
		if b := iconBytes("windows", pending); !bytes.HasPrefix(b, ico) {
			t.Errorf("windows pending=%v: not an ICO", pending)
		}
		for _, goos := range []string{"linux", "darwin"} {
			if b := iconBytes(goos, pending); !bytes.HasPrefix(b, png) {
				t.Errorf("%s pending=%v: not a PNG", goos, pending)
			}
		}
	}
	if bytes.Equal(iconBytes("windows", false), iconBytes("windows", true)) {
		t.Error("pending icon is the same as the normal one")
	}
}

func TestPortFromEnv(t *testing.T) {
	cases := map[string]int{"": 29005, "29110": 29110, "abc": 29005, "0": 29005, "70000": 29005}
	for v, want := range cases {
		got := portFromEnv(func(string) string { return v })
		if got != want {
			t.Errorf("ROCKET_PORT=%q: got %d, want %d", v, got, want)
		}
	}
}

func TestRunStatusAndFlags(t *testing.T) {
	srv, _ := fakeRocket(t, 2, 0)
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	var out, errOut bytes.Buffer
	if code := run([]string{"--port", fmt.Sprint(port), "--status"}, &out, &errOut); code != 0 {
		t.Fatalf("--status exit = %d, stderr %q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "rocket: running, 2 peers" {
		t.Fatalf("--status printed %q", got)
	}

	srv.Close()
	out.Reset()
	if code := run([]string{"--port", fmt.Sprint(port), "--status"}, &out, &errOut); code != 1 {
		t.Fatalf("--status against nothing: exit = %d, want 1", code)
	}

	for _, args := range [][]string{{"--port", "0"}, {"--port", "99999"}, {"extra"}, {"--nope"}} {
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}

	out.Reset()
	if code := run([]string{"--version"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "rocket-tray ") {
		t.Fatalf("--version: exit %d, out %q", code, out.String())
	}
}
