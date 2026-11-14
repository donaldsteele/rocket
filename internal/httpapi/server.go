// Package httpapi serves the rocket web UI, the peer transfer API and the
// browser drop page on a single HTTP port.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// Server holds the shared state for all handlers.
type Server struct {
	cfg    *config.Config
	id     *node.Identity
	trust  *peers.Store
	reg    *discovery.Registry
	hub    *eventHub
	static fs.FS

	mu       sync.Mutex
	pending  map[string]*pendingOffer // keyed by offer ID
	tokens   map[string]*pendingUpload
	quitOnce sync.Once
	quit     chan struct{}
}

type pendingOffer struct {
	offer transfer.Offer
	reply chan transfer.OfferReply
}

type pendingUpload struct {
	name string
	size int64
	from string
}

// New builds a server. static is the embedded web asset tree.
func New(cfg *config.Config, id *node.Identity, trust *peers.Store, reg *discovery.Registry, static fs.FS) *Server {
	return &Server{
		cfg: cfg, id: id, trust: trust, reg: reg,
		hub:     newEventHub(),
		static:  static,
		pending: map[string]*pendingOffer{},
		tokens:  map[string]*pendingUpload{},
		quit:    make(chan struct{}),
	}
}

// Quit returns a channel closed when the UI requests shutdown.
func (s *Server) Quit() <-chan struct{} { return s.quit }

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/peers", s.handlePeers)
	mux.HandleFunc("GET /api/incoming", s.handleIncoming)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/self", s.handleSelf)
	mux.HandleFunc("POST /api/offer", s.handleOffer)   // peer -> this host
	mux.HandleFunc("POST /api/recv", s.handleRecv)     // peer -> this host (bytes)
	mux.HandleFunc("POST /api/accept", s.handleAccept) // UI decision
	mux.HandleFunc("POST /api/send", s.handleSend)     // this host -> peer (local path)
	mux.HandleFunc("POST /api/relay", s.handleRelay)   // browser upload -> peer, streamed
	mux.HandleFunc("POST /api/quit", s.handleQuit)
	mux.HandleFunc("POST /drop", s.handleDrop) // browser guest upload

	// Static UI, last so API routes win.
	mux.Handle("/", http.FileServer(http.FS(s.static)))
	return mux
}

// --- read endpoints ---

func (s *Server) handleSelf(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"name":        s.id.Name,
		"fingerprint": s.id.Fingerprint(),
		"downloadDir": s.cfg.DownloadDir,
	})
}

func (s *Server) handlePeers(w http.ResponseWriter, _ *http.Request) {
	peers := s.reg.Peers()
	out := make([]map[string]any, 0, len(peers))
	for _, p := range peers {
		out = append(out, map[string]any{
			"name": p.Name, "fp": p.Fingerprint, "addr": p.Addr,
			"os": p.OS, "version": p.Version,
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleIncoming(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := make([]transfer.Offer, 0, len(s.pending))
	for _, p := range s.pending {
		out = append(out, p.offer)
	}
	s.mu.Unlock()
	writeJSON(w, out)
}

// --- receive side: offer -> accept -> recv ---

func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	var offer transfer.Offer
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&offer); err != nil {
		http.Error(w, "bad offer", http.StatusBadRequest)
		return
	}
	offer.ID = randToken()

	// Trust-on-first-use: a trusted sender skips the prompt. Browser drops are
	// handled by handleDrop (cookie), not here.
	if !offer.FromBrowser && s.trust.IsTrusted(offer.SenderFP) {
		token := s.mintToken(offer)
		writeJSON(w, transfer.OfferReply{Accepted: true, Token: token})
		return
	}

	po := &pendingOffer{offer: offer, reply: make(chan transfer.OfferReply, 1)}
	s.mu.Lock()
	s.pending[offer.ID] = po
	s.mu.Unlock()
	s.hub.publish(Event{Type: "offer", Data: offer})

	// Wait for the UI's decision, the request being cancelled, or a timeout.
	select {
	case reply := <-po.reply:
		writeJSON(w, reply)
	case <-r.Context().Done():
		s.dropPending(offer.ID)
	case <-time.After(2 * time.Minute):
		s.dropPending(offer.ID)
		writeJSON(w, transfer.OfferReply{Accepted: false, Reason: "timed out"})
	}
}

// handleAccept records the UI's decision on a pending offer.
func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Accept bool   `json:"accept"`
		Trust  bool   `json:"trust"` // remember this sender (TOFU)
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	po := s.pending[body.ID]
	delete(s.pending, body.ID)
	s.mu.Unlock()
	if po == nil {
		http.Error(w, "unknown offer", http.StatusNotFound)
		return
	}
	reply := transfer.OfferReply{Accepted: body.Accept}
	if body.Accept {
		if body.Trust && po.offer.SenderFP != "" {
			_ = s.trust.Approve(po.offer.SenderFP, po.offer.SenderName)
		}
		reply.Token = s.mintToken(po.offer)
	} else {
		reply.Reason = "declined"
	}
	po.reply <- reply
	writeJSON(w, map[string]bool{"ok": true})
}

// handleRecv streams an accepted upload to disk.
func (s *Server) handleRecv(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	s.mu.Lock()
	up := s.tokens[token]
	delete(s.tokens, token) // one-shot
	s.mu.Unlock()
	if up == nil {
		http.Error(w, "invalid or used token", http.StatusForbidden)
		return
	}

	dest := uniquePath(filepath.Join(s.cfg.DownloadDir, filepath.Base(up.name)))
	f, err := os.Create(dest)
	if err != nil {
		http.Error(w, "cannot create file", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	pr := &progressWriter{total: up.size, name: filepath.Base(dest), hub: s.hub}
	if _, err := io.Copy(io.MultiWriter(f, pr), r.Body); err != nil {
		s.hub.publish(Event{Type: "error", Data: map[string]string{"name": up.name, "error": err.Error()}})
		http.Error(w, "transfer interrupted", http.StatusInternalServerError)
		return
	}
	s.hub.publish(Event{Type: "done", Data: map[string]any{"name": filepath.Base(dest), "path": dest, "from": up.from}})
	writeJSON(w, map[string]any{"ok": true, "path": dest})
}

// --- send side (this host -> peer), used by the UI and the CLI ---

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PeerAddr string `json:"peer_addr"`
		Path     string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.PeerAddr == "" || body.Path == "" {
		http.Error(w, "need peer_addr and path", http.StatusBadRequest)
		return
	}
	offer := transfer.Offer{SenderName: s.id.Name, SenderFP: s.id.Fingerprint()}
	go func() {
		err := transfer.Send(context.Background(), body.PeerAddr, offer, body.Path, func(sent, total int64) {
			s.hub.publish(Event{Type: "progress", Data: map[string]any{
				"name": filepath.Base(body.Path), "sent": sent, "total": total, "dir": "out",
			}})
		})
		if err != nil {
			s.hub.publish(Event{Type: "error", Data: map[string]string{"name": filepath.Base(body.Path), "error": err.Error()}})
			return
		}
		s.hub.publish(Event{Type: "sent", Data: map[string]string{"name": filepath.Base(body.Path), "to": body.PeerAddr}})
	}()
	writeJSON(w, map[string]bool{"ok": true})
}

// handleRelay takes a file uploaded from this host's own browser UI and streams
// it straight to a peer, so a browser (which has no local file path) can drive a
// peer-to-peer send. Progress is reported on this host's event stream.
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	peerAddr := r.URL.Query().Get("peer")
	if peerAddr == "" {
		http.Error(w, "missing peer", http.StatusBadRequest)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	part, err := mr.NextPart()
	if err != nil {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	name := part.FileName()
	size := parseInt(r.URL.Query().Get("size"))
	offer := transfer.Offer{
		Name: name, Size: size,
		SenderName: s.id.Name, SenderFP: s.id.Fingerprint(),
	}
	// Relay runs past this request, so give it its own context.
	done := make(chan error, 1)
	go func() {
		done <- transfer.SendReader(context.Background(), peerAddr, offer, part, func(sent, total int64) {
			s.hub.publish(Event{Type: "progress", Data: map[string]any{"name": name, "sent": sent, "total": total, "dir": "out"}})
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			s.hub.publish(Event{Type: "error", Data: map[string]string{"name": name, "error": err.Error()}})
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.hub.publish(Event{Type: "sent", Data: map[string]string{"name": name, "to": peerAddr}})
		writeJSON(w, map[string]bool{"ok": true})
	case <-r.Context().Done():
		// Client went away; the relay goroutine will error out on its own.
	}
}

// --- browser drop page ---

func (s *Server) handleDrop(w http.ResponseWriter, r *http.Request) {
	// Cookie-remembered browsers skip the prompt; others get one accept prompt.
	trusted := false
	if c, err := r.Cookie("rocket_drop"); err == nil && c.Value != "" {
		trusted = true
	}
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	offer := transfer.Offer{
		ID: randToken(), Name: hdr.Filename, Size: hdr.Size,
		SenderName: "browser guest", FromBrowser: true,
	}
	if !trusted {
		po := &pendingOffer{offer: offer, reply: make(chan transfer.OfferReply, 1)}
		s.mu.Lock()
		s.pending[offer.ID] = po
		s.mu.Unlock()
		s.hub.publish(Event{Type: "offer", Data: offer})
		select {
		case reply := <-po.reply:
			if !reply.Accepted {
				http.Error(w, "declined", http.StatusForbidden)
				return
			}
		case <-r.Context().Done():
			s.dropPending(offer.ID)
			return
		case <-time.After(2 * time.Minute):
			s.dropPending(offer.ID)
			http.Error(w, "timed out", http.StatusRequestTimeout)
			return
		}
		// Remember this browser for the session.
		http.SetCookie(w, &http.Cookie{Name: "rocket_drop", Value: randToken(), Path: "/", HttpOnly: true})
	}

	dest := uniquePath(filepath.Join(s.cfg.DownloadDir, filepath.Base(hdr.Filename)))
	out, err := os.Create(dest)
	if err != nil {
		http.Error(w, "cannot save", http.StatusInternalServerError)
		return
	}
	defer out.Close()
	pr := &progressWriter{total: hdr.Size, name: filepath.Base(dest), hub: s.hub}
	if _, err := io.Copy(io.MultiWriter(out, pr), file); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	s.hub.publish(Event{Type: "done", Data: map[string]any{"name": filepath.Base(dest), "path": dest, "from": "browser guest"}})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleQuit(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
	s.quitOnce.Do(func() { close(s.quit) })
}

// --- helpers ---

func (s *Server) mintToken(offer transfer.Offer) string {
	token := randToken()
	s.mu.Lock()
	s.tokens[token] = &pendingUpload{name: offer.Name, size: offer.Size, from: offer.SenderName}
	s.mu.Unlock()
	return token
}

func (s *Server) dropPending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func parseInt(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func randToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// uniquePath appends " (n)" before the extension if path already exists.
func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	stem := path[:len(path)-len(ext)]
	for n := 1; ; n++ {
		cand := stem + " (" + itoa(n) + ")" + ext
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// progressWriter publishes receive progress as bytes arrive.
type progressWriter struct {
	total int64
	got   int64
	name  string
	hub   *eventHub
	last  time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.got += int64(n)
	if time.Since(p.last) > 50*time.Millisecond || p.got >= p.total {
		p.last = time.Now()
		p.hub.publish(Event{Type: "progress", Data: map[string]any{
			"name": p.name, "sent": p.got, "total": p.total, "dir": "in",
		}})
	}
	return n, nil
}
