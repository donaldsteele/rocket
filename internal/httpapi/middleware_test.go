package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const forbiddenLocal = "only available on this computer"

// request runs one request through the full handler with a chosen remote
// address and Host header.
func request(t *testing.T, s *Server, method, path, remote, host string, csrf bool) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(method, path, strings.NewReader("{}")).WithContext(ctx)
	req.RemoteAddr = remote
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(CSRFHeader, "1")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

func splitPattern(t *testing.T, p string) (string, string) {
	method, path, ok := strings.Cut(p, " ")
	if !ok {
		t.Fatalf("route pattern %q has no method", p)
	}
	return method, path
}

// TestLANToLoopbackRoutesForbidden calls every loopback-only route in the
// routing table from a non-loopback address and asserts 403, whatever Host
// header and CSRF header the caller sends.
func TestLANToLoopbackRoutesForbidden(t *testing.T) {
	h := newTestHost(t, "bob")
	s := h.srv
	locals := 0
	for _, rt := range s.routes() {
		if rt.surface != surfaceLocal {
			continue
		}
		locals++
		method, path := splitPattern(t, rt.pattern)
		for _, host := range []string{"192.168.1.10:29100", "127.0.0.1:29100", "localhost:29100"} {
			for _, remote := range []string{"192.168.1.50:40000", "[fe80::1]:40000", "10.0.0.7:1"} {
				code, body := request(t, s, method, path, remote, host, true)
				if code != http.StatusForbidden || !strings.Contains(body, forbiddenLocal) {
					t.Errorf("%s from %s (Host %s): got %d %q, want 403", rt.pattern, remote, host, code, body)
				}
			}
		}
	}
	if locals < 8 {
		t.Fatalf("only %d local routes in the table", locals)
	}
}

// TestLoopbackHostAllowlist: a loopback caller with a foreign Host header (a
// DNS-rebinding page) is refused; the three allowlisted names pass.
func TestLoopbackHostAllowlist(t *testing.T) {
	h := newTestHost(t, "bob")
	for _, host := range []string{"evil.example:29100", "127.0.0.1:29101", "127.0.0.1", "rocket.local:29100", "localhost.evil.example:29100"} {
		code, _ := request(t, h.srv, "GET", "/api/self", "127.0.0.1:5555", host, false)
		if code != http.StatusForbidden {
			t.Errorf("Host %q: got %d, want 403", host, code)
		}
	}
	for _, host := range []string{"127.0.0.1:29100", "localhost:29100", "LOCALHOST:29100", "[::1]:29100"} {
		for _, remote := range []string{"127.0.0.1:5555", "[::1]:5555"} {
			code, body := request(t, h.srv, "GET", "/api/self", remote, host, false)
			if code != http.StatusOK {
				t.Errorf("Host %q from %s: got %d %s, want 200", host, remote, code, body)
			}
		}
	}
}

// TestCSRFHeaderRequired: every non-GET loopback route needs X-Rocket-CSRF.
func TestCSRFHeaderRequired(t *testing.T) {
	h := newTestHost(t, "bob")
	for _, rt := range h.srv.routes() {
		method, path := splitPattern(t, rt.pattern)
		if rt.surface != surfaceLocal || method == "GET" {
			continue
		}
		code, body := request(t, h.srv, method, path, "127.0.0.1:5555", "127.0.0.1:29100", false)
		if code != http.StatusForbidden || !strings.Contains(body, CSRFHeader) {
			t.Errorf("%s without CSRF: got %d %q, want 403", rt.pattern, code, body)
		}
		if path == "/api/quit" {
			continue // would close the quit channel; covered above
		}
		code, body = request(t, h.srv, method, path, "127.0.0.1:5555", "127.0.0.1:29100", true)
		if code == http.StatusForbidden {
			t.Errorf("%s with CSRF from loopback: got 403 %q", rt.pattern, body)
		}
	}
}

// TestLANRoutesReachable: LAN routes are not blocked by the loopback guard.
func TestLANRoutesReachable(t *testing.T) {
	h := newTestHost(t, "bob")
	for _, rt := range h.srv.routes() {
		if rt.surface != surfaceLAN {
			continue
		}
		method, path := splitPattern(t, rt.pattern)
		_, body := request(t, h.srv, method, path, "192.168.1.50:40000", "192.168.1.10:29100", false)
		if strings.Contains(body, forbiddenLocal) {
			t.Errorf("LAN route %s refused from the LAN", rt.pattern)
		}
	}
	code, body := request(t, h.srv, "GET", "/api/hello", "192.168.1.50:40000", "192.168.1.10:29100", false)
	if code != 200 || !strings.Contains(body, h.id.Fingerprint()) || !strings.Contains(body, `"proto":1`) {
		t.Fatalf("hello: %d %s", code, body)
	}
}

// TestStaticSplit: GET / is the full UI on loopback and the drop page from the
// LAN; the full UI's files are loopback only.
func TestStaticSplit(t *testing.T) {
	h := newTestHost(t, "bob")
	lan, lanHost := "192.168.1.50:40000", "192.168.1.10:29100"
	code, body := request(t, h.srv, "GET", "/", lan, lanHost, false)
	if code != 200 || !strings.Contains(body, "drop.js") || strings.Contains(body, "app.js") {
		t.Fatalf("LAN GET /: %d, want the drop page", code)
	}
	code, body = request(t, h.srv, "GET", "/", "127.0.0.1:5555", "127.0.0.1:29100", false)
	if code != 200 || !strings.Contains(body, "app.js") {
		t.Fatalf("loopback GET /: %d, want the full UI", code)
	}
	// Loopback with a foreign Host gets the drop page, not the UI.
	_, body = request(t, h.srv, "GET", "/", "127.0.0.1:5555", "evil.example:29100", false)
	if strings.Contains(body, "app.js") {
		t.Fatal("rebinding Host got the full UI")
	}
	for _, p := range []string{"/index.html", "/app.js", "/vendor/alpine.min.js"} {
		if code, _ := request(t, h.srv, "GET", p, lan, lanHost, false); code != http.StatusForbidden {
			t.Errorf("LAN GET %s: %d, want 403", p, code)
		}
	}
	for _, p := range []string{"/drop.html", "/drop.js", "/styles.css", "/theme.js", "/vendor/fonts/Inter-Variable-latin.woff2"} {
		if code, _ := request(t, h.srv, "GET", p, lan, lanHost, false); code != http.StatusOK {
			t.Errorf("LAN GET %s: %d, want 200", p, code)
		}
	}
	code, _ = request(t, h.srv, "GET", "/api/self", lan, lanHost, false)
	if code != http.StatusForbidden {
		t.Fatalf("LAN GET /api/self: %d", code)
	}
}

func TestPendingLimits(t *testing.T) {
	h := newTestHost(t, "bob")
	s := h.srv
	add := func(id, ip string) error {
		return s.addPending(&pendingOffer{view: IncomingOffer{TransferID: id}, ip: ip, reply: make(chan decision, 1)})
	}
	for i := range 4 {
		if err := add("a"+strconv.Itoa(i), "10.0.0.1"); err != nil {
			t.Fatalf("offer %d from one IP: %v", i, err)
		}
	}
	if err := add("a4", "10.0.0.1"); err != errTooMany {
		t.Fatalf("5th offer from one IP: %v, want errTooMany", err)
	}
	for i := range 12 {
		if err := add("b"+strconv.Itoa(i), "10.0.1."+strconv.Itoa(i)); err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
	}
	if err := add("c", "10.0.2.1"); err != errTooMany {
		t.Fatalf("17th offer: %v, want errTooMany", err)
	}
}
