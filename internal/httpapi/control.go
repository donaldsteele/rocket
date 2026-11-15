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
	"github.com/donaldsteele/rocket/internal/receiver"
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
			"os": p.OS, "version": p.Version, "via": p.Via, "proto": p.Proto,
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
		Files: transfer.Files(m), Total: m.Total, State: transfer.StateWaiting, Encrypted: true,
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
	tid := s.startSend(plan, target, peerName, body.Paths)
	writeJSON(w, map[string]any{"ok": true, "transfer_id": tid, "files": transfer.Files(plan.Manifest), "total": plan.Manifest.Total})
}

// startSend sends plan (built from paths) to target in the background and
// returns its transfer ID. After a network failure it retries and resumes;
// when it gives up it is kept in the outbox for POST /api/resume.
func (s *Server) startSend(plan *transfer.Plan, target transfer.Target, peerName string, paths []string) string {
	tid := randToken()
	ctx := s.startOutgoing(context.Background(), tid, peerName, target, plan.Manifest, nil)
	go func() {
		_ = s.ledger.Send(ctx, s.id, receiver.Outgoing{
			ID: tid, PeerName: peerName, Target: target, Manifest: plan.Manifest, Open: plan.Open,
			Resumable: true, Delays: s.retryDelays, Outbox: s.outbox, Paths: paths,
		})
	}()
	return tid
}

// handleResumable lists what can be resumed: GET /api/resumable gives
// {"outgoing": [interrupted sends], "incoming": [transfers kept here]}.
func (s *Server) handleResumable(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"outgoing": s.outbox.List(), "incoming": s.rx.KeptList()})
}

// handleResume sends an interrupted send again: POST /api/resume
// {transfer_id}. The files are listed again from the paths it was given;
// when they are unchanged the receiver continues where it stopped. The peer
// is looked up by fingerprint first (it may have a new address). It answers
// {ok, transfer_id} with the new transfer's ID.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id, ok := transferIDBody(w, r)
	if !ok {
		return
	}
	rec, ok := s.outbox.Get(id)
	if !ok {
		http.Error(w, "nothing to resume with that id", http.StatusNotFound)
		return
	}
	if len(rec.Picked) > 0 {
		http.Error(w, "this send was started from the rocket page with files picked there; resume it from the page, which asks for the same files again", http.StatusConflict)
		return
	}
	plan, err := transfer.PlanPaths(rec.Paths)
	if err != nil {
		http.Error(w, "cannot send these files again: "+err.Error(), http.StatusBadRequest)
		return
	}
	target := transfer.Target{Addr: rec.Addr, FP: rec.PeerFP}
	if p, found := s.reg.Find(rec.PeerFP); rec.PeerFP != "" && found {
		target.Addr = p.Addr
	}
	_ = s.outbox.Remove(id)
	tid := s.startSend(plan, target, rec.PeerName, rec.Paths)
	writeJSON(w, map[string]any{"ok": true, "transfer_id": tid, "files": transfer.Files(plan.Manifest), "total": plan.Manifest.Total})
}

// handleDiscard forgets something resumable: POST /api/discard
// {transfer_id}. An interrupted send is dropped (and its receiver told to
// drop what it kept); a transfer kept here is removed with its files.
func (s *Server) handleDiscard(w http.ResponseWriter, r *http.Request) {
	id, ok := transferIDBody(w, r)
	if !ok {
		return
	}
	if rec, found := s.outbox.Get(id); found {
		if err := receiver.DiscardOutgoing(s.outbox, s.id, rec); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if !s.rx.DiscardKept(id) {
		http.Error(w, "nothing to discard with that id", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// transferIDBody reads {transfer_id} from a control request.
func transferIDBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		TransferID string `json:"transfer_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || !protocol.ValidID(body.TransferID) {
		http.Error(w, "need transfer_id", http.StatusBadRequest)
		return "", false
	}
	return body.TransferID, true
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
// in order. One bare file goes out as kind:file, anything else as
// kind:multi; the parts are encrypted on the fly into the peer body
// (protocol v2), nothing is staged.
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
	// the browser upload (the body writer may be blocked in it).
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
