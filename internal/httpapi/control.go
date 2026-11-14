package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/donaldsteele/rocket/internal/transfer"
)

// --- read endpoints (loopback only) ---

func (s *Server) handleSelf(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"name":        s.id.Name,
		"fingerprint": s.id.Fingerprint(),
		"pub":         s.id.PublicKeyB64(),
		"downloadDir": s.cfg.DownloadDir,
		"port":        s.cfg.HTTPPort,
		"version":     s.Version,
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
	out := make([]IncomingOffer, 0, len(s.pending))
	for _, p := range s.pending {
		out = append(out, p.view)
	}
	s.mu.Unlock()
	writeJSON(w, out)
}

// --- control endpoints (loopback + CSRF header) ---

// handleAccept delivers the person's decision to a pending offer.
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
	po.reply <- decision{accept: body.Accept, trust: body.Trust}
	writeJSON(w, map[string]bool{"ok": true})
}

// resolveTarget turns a peer reference into a send target: host:port is used
// directly; otherwise it is a discovered peer's name or fingerprint prefix,
// whose fingerprint the receiver must then answer with.
func (s *Server) resolveTarget(peer string) (transfer.Target, error) {
	if transfer.IsHostPort(peer) {
		return transfer.Target{Addr: peer}, nil
	}
	if p, ok := s.reg.Find(peer); ok {
		return transfer.Target{Addr: p.Addr, FP: p.Fingerprint}, nil
	}
	return transfer.Target{}, fmt.Errorf("peer %q not found on the network", peer)
}

// handleSend sends a local file (path on this computer) to a peer, in the
// background. Body: {peer, paths[]}; this version sends exactly one file.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Peer  string   `json:"peer"`
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.Peer == "" || len(body.Paths) == 0 {
		http.Error(w, "need peer and paths", http.StatusBadRequest)
		return
	}
	if len(body.Paths) != 1 {
		http.Error(w, "this version sends one file at a time", http.StatusBadRequest)
		return
	}
	path := body.Paths[0]
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		http.Error(w, "not a readable file: "+path, http.StatusBadRequest)
		return
	}
	target, err := s.resolveTarget(body.Peer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tid := randToken()
	name := filepath.Base(path)
	go func() {
		err := transfer.Send(context.Background(), s.id, target, path, s.progressFn(tid, name, "out"))
		if err != nil {
			s.publishError(tid, name, err)
			return
		}
		s.hub.publish(Event{Type: "sent", Data: map[string]any{"transfer_id": tid, "name": name, "to": target.Addr}})
	}()
	writeJSON(w, map[string]any{"ok": true, "transfer_id": tid})
}

// handleRelay takes a file uploaded from this host's own browser UI and
// streams it straight to a peer, so a browser (which has no local file path)
// can drive a peer-to-peer send. ?peer= names the peer, ?size= the file size.
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveTarget(r.URL.Query().Get("peer"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	size, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
	if err != nil || size < 0 {
		http.Error(w, "missing or bad size", http.StatusBadRequest)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FileName() == "" {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	name := transfer.BaseName(part.FileName())
	tid := randToken()
	// The send outlives neither this request nor its body: cancel it if the
	// browser goes away.
	err = transfer.SendReader(r.Context(), s.id, target, name, size, part, s.progressFn(tid, name, "out"))
	if err != nil {
		s.publishError(tid, name, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.hub.publish(Event{Type: "sent", Data: map[string]any{"transfer_id": tid, "name": name, "to": target.Addr}})
	writeJSON(w, map[string]any{"ok": true, "transfer_id": tid})
}

func (s *Server) handleQuit(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
	s.quitOnce.Do(func() { close(s.quit) })
}
