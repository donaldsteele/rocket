// Package httpapi serves the rocket web UI, the peer transfer API and the
// browser drop page on a single HTTP port, split into two surfaces: the LAN
// surface (peer protocol and drop page, any host) and the loopback-only
// control surface (full UI, control and read APIs, SSE). Every route is
// classified in one table, see routes.
package httpapi

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/receiver"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// Server holds the shared state for all handlers. The peer protocol (hello,
// offer, recv) and the pending offers and upload tokens it shares with the
// drop flow live in rx (internal/receiver); the transfers in flight, their
// history and the events that report them in ledger.
type Server struct {
	cfg      *config.Config
	id       *node.Identity
	trust    *peers.Store
	drops    *peers.DropTokens
	reg      *discovery.Registry
	hub      *eventHub
	static   fs.FS
	xfers    *transfer.Manager
	hist     *history.Store
	settings *config.SettingsStore
	offers   *rateLimiter // offers per remote IP (/api/offer, /drop/offer)
	ledger   *receiver.Ledger
	rx       *receiver.Receiver
	// Version is reported by /api/hello and /api/self.
	Version string
	// OS is reported by /api/hello.
	OS string

	mu       sync.Mutex
	dlDir    string // live download folder (flag/env/settings at start, PUT /api/settings after)
	quitOnce sync.Once
	quit     chan struct{}
}

// IncomingOffer is what the UI sees of an offer waiting for a decision (the
// "offer" event and GET /api/incoming).
type IncomingOffer = receiver.IncomingOffer

// decision is the person's answer to a pending offer.
type decision = receiver.Decision

// pendingOffer is an offer waiting for a decision (receiver.Pending).
type pendingOffer struct {
	view  IncomingOffer
	ip    string
	reply chan decision
}

func (po *pendingOffer) pending() *receiver.Pending {
	return &receiver.Pending{View: po.view, IP: po.ip, Reply: po.reply}
}

// upload is an accepted offer waiting for its bytes (receiver.Upload).
type upload = receiver.Upload

var errTooMany = receiver.ErrTooMany

// New builds a server. static is the embedded web asset tree.
func New(cfg *config.Config, id *node.Identity, trust *peers.Store, reg *discovery.Registry, static fs.FS) (*Server, error) {
	drops, err := peers.OpenDropTokens(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	hist, err := history.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	// A damaged settings.json falls back to the defaults; the next save
	// replaces it.
	settings, _ := config.OpenSettings(cfg.DataDir)
	s := &Server{
		cfg: cfg, id: id, trust: trust, drops: drops, reg: reg,
		hub:      newEventHub(),
		static:   static,
		xfers:    transfer.NewManager(),
		hist:     hist,
		settings: settings,
		offers:   newRateLimiter(offerBurst, offerRefill),
		dlDir:    cfg.DownloadDir,
		Version:  "dev",
		quit:     make(chan struct{}),
	}
	s.ledger = &receiver.Ledger{Transfers: s.xfers, History: hist, Events: s.hub.publish}
	s.rx = receiver.New(receiver.Config{
		Identity: id,
		Trust:    trust,
		Ledger:   s.ledger,
		// The UI learns of an offer from the "offer" event and answers with
		// POST /api/accept.
		Decider:          receiver.DeciderFunc(func(o IncomingOffer) { s.hub.publish(Event{Type: "offer", Data: o}) }),
		Sink:             receiver.FSSink{Dir: s.downloadDir},
		Hello:            func() (string, string) { return s.Version, s.OS },
		AutoAccept:       s.autoAccept,
		AllowOffer:       func(ip string) bool { return s.offers.Allow(ip) },
		OfferReadTimeout: offerReadTimeout,
	})
	return s, nil
}

// Quit returns a channel closed when the UI requests shutdown.
func (s *Server) Quit() <-chan struct{} { return s.quit }

// routes is the single routing table. Every route is LAN (any host) or Local
// (loopback + Host allowlist + CSRF on non-GET); the static tree classifies
// per path.
func (s *Server) routes() []route {
	return []route{
		// LAN surface: peer protocol v2.
		{"GET /api/hello", surfaceLAN, s.rx.HandleHello},
		{"POST /api/offer", surfaceLAN, s.rx.HandleOffer},
		{"POST /api/recv", surfaceLAN, s.rx.HandleRecv},
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
		{"GET /api/transfers", surfaceLocal, s.handleTransfers},
		{"POST /api/cancel", surfaceLocal, s.handleCancel},
		{"GET /api/history", surfaceLocal, s.handleHistory},
		{"POST /api/history/clear", surfaceLocal, s.handleHistoryClear},
		{"GET /api/settings", surfaceLocal, s.handleGetSettings},
		{"PUT /api/settings", surfaceLocal, s.handlePutSettings},
		{"GET /api/trusted", surfaceLocal, s.handleTrusted},
		{"POST /api/trusted/revoke", surfaceLocal, s.handleRevoke},
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

// --- pending offers and upload tokens (shared with the peer protocol) ---

// addPending registers po unless the global or per-IP limit is reached.
func (s *Server) addPending(po *pendingOffer) error { return s.rx.AddPending(po.pending()) }

func (s *Server) dropPending(id string) { s.rx.DropPending(id) }

// waitDecision publishes po as an "offer" event and blocks until the person
// decides, the requester goes away (the offer is withdrawn), or
// protocol.OfferWait passes (see receiver.Receiver.Wait).
func (s *Server) waitDecision(r *http.Request, po *pendingOffer) (d decision, ok, timedOut bool) {
	return s.rx.Wait(r.Context(), po.pending())
}

// downloadDir is the live download folder.
func (s *Server) downloadDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dlDir
}

// autoAccept reports whether trusted senders and remembered browsers skip
// the prompt (settings: auto_accept_trusted).
func (s *Server) autoAccept() bool { return s.settings.Get().AutoAcceptTrusted }

// endTransfer finishes transfer id (see receiver.Ledger.End).
func (s *Server) endTransfer(id string, err error) (history.Result, error) {
	return s.ledger.End(id, err)
}

// --- helpers ---

// viewOf is the prompt view of manifest m (see receiver.View).
func viewOf(id string, m protocol.Manifest) IncomingOffer { return receiver.View(id, m) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func randToken() string { return receiver.NewToken() }

// progressFn returns a transfer.ProgressFn that publishes "progress" events
// (see receiver.Ledger.Progress).
func (s *Server) progressFn(transferID, name, dir string) transfer.ProgressFn {
	return s.ledger.Progress(transferID, name, dir)
}
