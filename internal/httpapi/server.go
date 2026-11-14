// Package httpapi serves the rocket web UI, the peer transfer API and the
// browser drop page on a single HTTP port, split into two surfaces: the LAN
// surface (peer protocol and drop page, any host) and the loopback-only
// control surface (full UI, control and read APIs, SSE). Every route is
// classified in one table, see routes.
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/internal/protocol"
)

// Server holds the shared state for all handlers.
type Server struct {
	cfg      *config.Config
	id       *node.Identity
	trust    *peers.Store
	drops    *peers.DropTokens
	reg      *discovery.Registry
	hub      *eventHub
	static   fs.FS
	verifier *protocol.Verifier
	// Version is reported by /api/hello and /api/self.
	Version string
	// OS is reported by /api/hello.
	OS string

	mu       sync.Mutex
	pending  map[string]*pendingOffer // keyed by transfer ID
	uploads  map[string]*upload       // keyed by one-shot upload token
	quitOnce sync.Once
	quit     chan struct{}
}

// IncomingOffer is what the UI sees of an offer waiting for a decision (the
// "offer" event and GET /api/incoming).
type IncomingOffer struct {
	ID          string   `json:"id"` // same as TransferID; answer with POST /api/accept {id}
	TransferID  string   `json:"transfer_id"`
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`  // first entry
	Size        int64    `json:"size"`  // total bytes
	Files       int      `json:"files"` // entry count
	Names       []string `json:"names"` // first few entry paths
	SenderName  string   `json:"sender_name"`
	SenderFP    string   `json:"sender_fp,omitempty"`
	FromBrowser bool     `json:"from_browser"`
	Addr        string   `json:"addr"` // remote IP
}

// decision is the person's answer to a pending offer.
type decision struct {
	accept bool
	trust  bool
}

type pendingOffer struct {
	view  IncomingOffer
	ip    string
	reply chan decision
}

// upload is an accepted offer waiting for its bytes, keyed by a one-shot
// token that is valid for protocol.TokenTTL and only from the offering IP.
type upload struct {
	transferID string
	manifest   protocol.Manifest
	senderName string
	senderFP   string
	ip         string
	drop       bool // browser drop (POST /drop/upload) vs peer (POST /api/recv)
	expires    time.Time
}

var errTooMany = errors.New("too many pending offers")

// New builds a server. static is the embedded web asset tree.
func New(cfg *config.Config, id *node.Identity, trust *peers.Store, reg *discovery.Registry, static fs.FS) (*Server, error) {
	drops, err := peers.OpenDropTokens(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: cfg, id: id, trust: trust, drops: drops, reg: reg,
		hub:      newEventHub(),
		static:   static,
		verifier: protocol.NewVerifier(id.Fingerprint()),
		Version:  "dev",
		pending:  map[string]*pendingOffer{},
		uploads:  map[string]*upload{},
		quit:     make(chan struct{}),
	}, nil
}

// Quit returns a channel closed when the UI requests shutdown.
func (s *Server) Quit() <-chan struct{} { return s.quit }

// routes is the single routing table. Every route is LAN (any host) or Local
// (loopback + Host allowlist + CSRF on non-GET); the static tree classifies
// per path.
func (s *Server) routes() []route {
	return []route{
		// LAN surface: peer protocol v1.
		{"GET /api/hello", surfaceLAN, s.handleHello},
		{"POST /api/offer", surfaceLAN, s.handleOffer},
		{"POST /api/recv", surfaceLAN, s.handleRecv},
		// LAN surface: browser drop flow.
		{"POST /drop/offer", surfaceLAN, s.handleDropOffer},
		{"POST /drop/upload", surfaceLAN, s.handleDropUpload},

		// Loopback-only control and read API.
		{"GET /api/self", surfaceLocal, s.handleSelf},
		{"GET /api/peers", surfaceLocal, s.handlePeers},
		{"GET /api/incoming", surfaceLocal, s.handleIncoming},
		{"GET /api/events", surfaceLocal, s.handleEvents},
		{"POST /api/accept", surfaceLocal, s.handleAccept},
		{"POST /api/send", surfaceLocal, s.handleSend},
		{"POST /api/relay", surfaceLocal, s.handleRelay},
		{"POST /api/quit", surfaceLocal, s.handleQuit},

		// Static files: full UI on loopback, drop page elsewhere.
		{"GET /", surfaceMixed, s.handleStatic},
	}
}

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.pattern, s.guard(rt))
	}
	return mux
}

// lanStatic lists the static files any host may fetch: the drop page and the
// assets it uses. Everything else (index.html, app.js, Alpine) is loopback
// only.
func lanStatic(name string) bool {
	switch name {
	case "drop.html", "drop.js", "styles.css", "theme.js", "icons.svg", "favicon.png":
		return true
	}
	return strings.HasPrefix(name, "vendor/fonts/") && !strings.Contains(name, "..")
}

// handleStatic serves the embedded web tree. GET / is the full UI for a local
// request and the drop page for anyone else.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	local := s.isLocal(r)
	p := r.URL.Path
	if p == "/" {
		if local {
			http.ServeFileFS(w, r, s.static, "index.html")
		} else {
			http.ServeFileFS(w, r, s.static, "drop.html")
		}
		return
	}
	if !local && !lanStatic(strings.TrimPrefix(p, "/")) {
		http.Error(w, "forbidden: this page is only available on this computer", http.StatusForbidden)
		return
	}
	http.FileServerFS(s.static).ServeHTTP(w, r)
}

// --- pending offers and upload tokens ---

// addPending registers po unless the global or per-IP limit is reached.
func (s *Server) addPending(po *pendingOffer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) >= protocol.MaxPending {
		return errTooMany
	}
	n := 0
	for _, p := range s.pending {
		if p.ip == po.ip {
			n++
		}
	}
	if n >= protocol.MaxPendingIP {
		return errTooMany
	}
	s.pending[po.view.TransferID] = po
	return nil
}

func (s *Server) dropPending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// waitDecision publishes po as an "offer" event and blocks until the person
// decides, the requester goes away, or protocol.OfferWait passes. ok is false
// when no decision arrived (timedOut tells which).
func (s *Server) waitDecision(r *http.Request, po *pendingOffer) (d decision, ok, timedOut bool) {
	s.hub.publish(Event{Type: "offer", Data: po.view})
	t := time.NewTimer(protocol.OfferWait)
	defer t.Stop()
	select {
	case d = <-po.reply:
		return d, true, false
	case <-r.Context().Done():
	case <-t.C:
		timedOut = true
	}
	s.dropPending(po.view.TransferID)
	s.hub.publish(Event{Type: "offer_gone", Data: map[string]any{"transfer_id": po.view.TransferID}})
	return decision{}, false, timedOut
}

// mintUpload stores up under a fresh one-shot token and returns the token.
func (s *Server) mintUpload(up *upload) string {
	token := randToken()
	now := time.Now()
	up.expires = now.Add(protocol.TokenTTL)
	s.mu.Lock()
	for k, u := range s.uploads {
		if now.After(u.expires) {
			delete(s.uploads, k)
		}
	}
	s.uploads[token] = up
	s.mu.Unlock()
	return token
}

// takeUpload consumes a token. It returns nil if the token is unknown, used,
// expired, of the wrong flow (drop vs peer), or presented from another IP.
func (s *Server) takeUpload(token string, drop bool, ip string) *upload {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	up := s.uploads[token]
	delete(s.uploads, token) // one-shot, even when the checks below fail
	s.mu.Unlock()
	if up == nil || up.drop != drop || up.ip != ip || time.Now().After(up.expires) {
		return nil
	}
	return up
}

// --- helpers ---

func viewOf(id string, m protocol.Manifest) IncomingOffer {
	v := IncomingOffer{ID: id, TransferID: id, Kind: m.Kind, Size: m.Total, Files: len(m.Entries)}
	for i, e := range m.Entries {
		if i == 0 {
			v.Name = e.Path
		}
		if i == 5 {
			break
		}
		v.Names = append(v.Names, e.Path)
	}
	return v
}

// cleanLabel makes an untrusted display name safe to show: control and
// format characters removed, whitespace collapsed, at most 64 runes.
func cleanLabel(s, fallback string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.Join(strings.Fields(s), " ") {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n == 64 {
			break
		}
		b.WriteRune(r)
		n++
	}
	if b.Len() == 0 {
		return fallback
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func randToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// progressFn returns a transfer.ProgressFn that publishes "progress" events.
func (s *Server) progressFn(transferID, name, dir string) func(sent, total int64) {
	return func(sent, total int64) {
		s.hub.publish(Event{Type: "progress", Data: map[string]any{
			"transfer_id": transferID, "name": name, "sent": sent, "total": total, "dir": dir,
		}})
	}
}

func (s *Server) publishError(transferID, name string, err error) {
	s.hub.publish(Event{Type: "error", Data: map[string]any{"transfer_id": transferID, "name": name, "error": err.Error()}})
}
