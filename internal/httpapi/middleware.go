package httpapi

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/donaldsteele/rocket/internal/receiver"
)

// surface says who may call a route.
type surface int

const (
	// surfaceLAN routes are callable from any host: the peer protocol and
	// the browser drop flow.
	surfaceLAN surface = iota
	// surfaceLocal routes are callable only from this computer: loopback
	// RemoteAddr AND an allowlisted Host header (DNS-rebinding defence), plus
	// the CSRF header on every non-GET call.
	surfaceLocal
	// surfaceMixed is the static file tree, which classifies per path (see
	// handleStatic).
	surfaceMixed
)

func (s surface) String() string {
	switch s {
	case surfaceLAN:
		return "lan"
	case surfaceLocal:
		return "local"
	default:
		return "mixed"
	}
}

// CSRFHeader must be "1" on every non-GET control call. Browsers cannot add a
// custom header to a cross-origin request without a CORS preflight, which
// this server never approves.
const CSRFHeader = "X-Rocket-CSRF"

// route is one entry of the single routing table every request goes through.
type route struct {
	pattern string // net/http ServeMux pattern, "METHOD /path"
	surface surface
	handler http.HandlerFunc
}

// guard wraps a route's handler with its surface's access checks and the
// common security headers.
func (s *Server) guard(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if rt.surface == surfaceLocal {
			if !s.isLocal(r) {
				http.Error(w, "forbidden: this endpoint is only available on this computer", http.StatusForbidden)
				return
			}
			if !safeMethod(r.Method) && !hasCSRF(r) {
				http.Error(w, "forbidden: missing "+CSRFHeader+" header", http.StatusForbidden)
				return
			}
		}
		rt.handler(w, r)
	})
}

func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

func hasCSRF(r *http.Request) bool { return r.Header.Get(CSRFHeader) == "1" }

// isLocal reports whether r comes from this computer through an expected Host
// name: the peer address is loopback and Host is 127.0.0.1:<port>,
// localhost:<port> or [::1]:<port>, where <port> is the port the request
// arrived on.
func (s *Server) isLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	return hostAllowed(r.Host, s.localPort(r))
}

// localPort is the port the request arrived on, falling back to the
// configured HTTP port.
func (s *Server) localPort(r *http.Request) int {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if ta, ok := a.(*net.TCPAddr); ok && ta.Port != 0 {
			return ta.Port
		}
	}
	return s.cfg.HTTPPort
}

func hostAllowed(host string, port int) bool {
	p := strconv.Itoa(port)
	switch strings.ToLower(host) {
	case "127.0.0.1:" + p, "localhost:" + p, "[::1]:" + p:
		return true
	}
	return false
}

// remoteIP is the peer IP of r, used for per-IP limits and token binding.
func remoteIP(r *http.Request) string { return receiver.RemoteIP(r) }
