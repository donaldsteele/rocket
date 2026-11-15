package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// --- read endpoints (loopback only) ---

func (s *Server) handleSelf(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"name":        s.id.Name,
		"fingerprint": s.id.Fingerprint(),
		"pub":         s.id.PublicKeyB64(),
		"downloadDir": s.downloadDir(),
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
			"os": p.OS, "version": p.Version, "via": p.Via,
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleIncoming(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.rx.PendingOffers())
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
	if !s.rx.Answer(body.ID, body.Accept, body.Trust) {
		http.Error(w, "unknown offer", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// resolveTarget turns a peer reference into a send target and a display
// name: host:port is used directly (and named by its address); otherwise it
// is a discovered peer's full fingerprint, or a name or fingerprint prefix
// that matches exactly one peer (discovery.Registry.Resolve). The receiver
// must then answer with that peer's fingerprint.
func (s *Server) resolveTarget(peer string) (transfer.Target, string, error) {
	if transfer.IsHostPort(peer) {
		return transfer.Target{Addr: peer}, peer, nil
	}
	p, err := s.reg.Resolve(peer)
	if err != nil {
		return transfer.Target{}, "", err
	}
	return transfer.Target{Addr: p.Addr, FP: p.Fingerprint}, p.Name, nil
}

// startOutgoing registers an outgoing transfer (waiting for the receiver's
// decision) and returns its context: POST /api/cancel cancels it, which
// withdraws a pending offer or aborts the upload. abort, if not nil, also
// runs on cancel (see transfer.Manager.Start).
func (s *Server) startOutgoing(parent context.Context, tid, peerName string, t transfer.Target, m protocol.Manifest, abort func()) context.Context {
	return s.xfers.Start(parent, transfer.Info{
		ID: tid, Dir: history.DirOut, Name: transfer.Label(m), PeerName: peerName, PeerFP: t.FP,
		Files: transfer.Files(m), Total: m.Total, State: transfer.StateWaiting,
	}, abort)
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
	target, peerName, err := s.resolveTarget(body.Peer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tid := randToken()
	name := transfer.Label(plan.Manifest)
	ctx := s.startOutgoing(context.Background(), tid, peerName, target, plan.Manifest, nil)
	go func() {
		err := transfer.SendPlan(ctx, s.id, target, plan, s.progressFn(tid, name, "out"))
		s.publishSent(tid, name, target, plan.Manifest, err)
	}()
	writeJSON(w, map[string]any{"ok": true, "transfer_id": tid, "files": transfer.Files(plan.Manifest), "total": plan.Manifest.Total})
}

// publishSent ends an outgoing transfer: history, then "sent" on success
// (endTransfer publishes "cancelled" or "error" otherwise). It returns the
// error as endTransfer reports it.
func (s *Server) publishSent(tid, name string, target transfer.Target, m protocol.Manifest, err error) error {
	if _, err = s.endTransfer(tid, err); err != nil {
		return err
	}
	s.hub.publish(Event{Type: "sent", Data: map[string]any{
		"transfer_id": tid, "name": name, "to": target.Addr, "files": transfer.Files(m), "bytes": m.Total,
	}})
	return nil
}

// relayManifestField is the multipart form field that must come first in a
// POST /api/relay body: JSON {"files":[{"name","size"}]}, where name may be a
// relative path from a folder picker ("photos/2024/a.jpg").
const relayManifestField = "manifest"

// handleRelay takes files uploaded from this host's own browser UI and
// streams them straight to a peer, so a browser (which has no local file
// paths) can drive a peer-to-peer send. ?peer= is the peer's full
// fingerprint (as the UI's peer list carries it) or a host:port; names and
// prefixes are refused, since names are unauthenticated. The body is
// multipart: first the "manifest" field, then one file part per listed file,
// in order. One bare file goes out as kind:file, anything else as a tar
// (kind:multi) built on the fly from the parts; nothing is staged.
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	peer := r.URL.Query().Get("peer")
	if !discovery.IsFingerprint(peer) && !transfer.IsHostPort(peer) {
		http.Error(w, "peer must be a full fingerprint or host:port", http.StatusBadRequest)
		return
	}
	target, peerName, err := s.resolveTarget(peer)
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
		// Closing a multipart part drains the rest of it, which after a
		// cancel could take as long as the whole upload; the next NextPart
		// skips what is left anyway.
		return io.NopCloser(part), nil
	}
	// The send outlives neither this request nor its body: it stops if the
	// browser goes away, or on POST /api/cancel, which also stops the read of
	// the browser upload (the tar writer may be blocked in it).
	rc := http.NewResponseController(w)
	ctx := s.startOutgoing(r.Context(), tid, peerName, target, m, func() { _ = rc.SetReadDeadline(time.Now()) })
	err = transfer.SendManifest(ctx, s.id, target, m, open, s.progressFn(tid, name, "out"))
	if err = s.publishSent(tid, name, target, m, err); err != nil {
		code := http.StatusBadGateway
		if transfer.Outcome(err) == history.ResultCancelled {
			code = http.StatusGone
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "transfer_id": tid, "files": transfer.Files(m)})
}

func (s *Server) handleQuit(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
	s.quitOnce.Do(func() { close(s.quit) })
}
