package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/donaldsteele/rocket/internal/protocol"
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

// handleSend sends local files and folders (paths on this computer) to a
// peer in one transfer, in the background. Body: {peer, paths[]}. The
// manifest is built before answering, so a missing path or a send over the
// limits fails here with 400.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Peer  string   `json:"peer"`
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.Peer == "" || len(body.Paths) == 0 {
		http.Error(w, "need peer and paths", http.StatusBadRequest)
		return
	}
	plan, err := transfer.PlanPaths(body.Paths)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := s.resolveTarget(body.Peer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tid := randToken()
	name := transfer.Label(plan.Manifest)
	go func() {
		err := transfer.SendPlan(context.Background(), s.id, target, plan, s.progressFn(tid, name, "out"))
		s.publishSent(tid, name, target, plan.Manifest, err)
	}()
	writeJSON(w, map[string]any{"ok": true, "transfer_id": tid, "files": transfer.Files(plan.Manifest), "total": plan.Manifest.Total})
}

// publishSent emits "sent" for a finished outgoing transfer, or "error".
func (s *Server) publishSent(tid, name string, target transfer.Target, m protocol.Manifest, err error) {
	if err != nil {
		s.publishError(tid, name, err)
		return
	}
	s.hub.publish(Event{Type: "sent", Data: map[string]any{
		"transfer_id": tid, "name": name, "to": target.Addr, "files": transfer.Files(m), "bytes": m.Total,
	}})
}

// relayManifestField is the multipart form field that must come first in a
// POST /api/relay body: JSON {"files":[{"name","size"}]}, where name may be a
// relative path from a folder picker ("photos/2024/a.jpg").
const relayManifestField = "manifest"

// handleRelay takes files uploaded from this host's own browser UI and
// streams them straight to a peer, so a browser (which has no local file
// paths) can drive a peer-to-peer send. ?peer= names the peer. The body is
// multipart: first the "manifest" field, then one file part per listed file,
// in order. One bare file goes out as kind:file, anything else as a tar
// (kind:multi) built on the fly from the parts; nothing is staged.
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveTarget(r.URL.Query().Get("peer"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	first, err := mr.NextPart()
	if err != nil || first.FormName() != relayManifestField || first.FileName() != "" {
		http.Error(w, "the first part must be the manifest field", http.StatusBadRequest)
		return
	}
	var list struct {
		Files []transfer.BrowserFile `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(first, protocol.MaxOfferBytes)).Decode(&list); err != nil {
		http.Error(w, "bad manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	m, err := transfer.BrowserManifest(list.Files, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tid := randToken()
	name := transfer.Label(m)
	k := 0 // index into list.Files of the next file part
	open := func(int, protocol.Entry) (io.ReadCloser, error) {
		part, err := mr.NextPart()
		if err != nil {
			return nil, fmt.Errorf("expected %d files, got %d", len(list.Files), k)
		}
		want := transfer.BaseName(list.Files[k].Name)
		if part.FileName() == "" || transfer.BaseName(part.FileName()) != want {
			return nil, fmt.Errorf("file %d is not %q", k+1, want)
		}
		k++
		return part, nil
	}
	// The send outlives neither this request nor its body: cancel it if the
	// browser goes away.
	err = transfer.SendManifest(r.Context(), s.id, target, m, open, s.progressFn(tid, name, "out"))
	s.publishSent(tid, name, target, m, err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "transfer_id": tid, "files": transfer.Files(m)})
}

func (s *Server) handleQuit(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
	s.quitOnce.Do(func() { close(s.quit) })
}
