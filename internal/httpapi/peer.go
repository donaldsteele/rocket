package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// handleHello answers GET /api/hello with this host's protocol identity.
func (s *Server) handleHello(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, protocol.Hello{
		Proto: protocol.Version, Name: s.id.Name, FP: s.id.Fingerprint(),
		Pub: s.id.PublicKeyB64(), Version: s.Version, OS: s.OS,
	})
}

// handleOffer verifies a signed peer offer and long-polls for the person's
// decision (skipped for a trusted sender whose key matches the pinned key).
func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxOfferBytes)
	var o protocol.Offer
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "offer too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad offer: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.verifier.Verify(&o); err != nil {
		http.Error(w, "offer rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	if o.Manifest.Kind != protocol.KindFile {
		http.Error(w, "offer rejected: multi-file offers are not supported by this receiver yet", http.StatusBadRequest)
		return
	}

	tid := randToken()
	ip := remoteIP(r)
	name := cleanLabel(o.SenderName, o.SenderFP)
	up := &upload{transferID: tid, manifest: o.Manifest, senderName: name, senderFP: o.SenderFP, ip: ip}

	// Trust-on-first-use with proof of key possession: a sender whose
	// fingerprint is trusted AND whose key matches the pinned key skips the
	// prompt.
	if s.trust.IsTrusted(o.SenderFP, o.SenderPub) {
		writeJSON(w, protocol.OfferReply{Accepted: true, Token: s.mintUpload(up), TransferID: tid})
		return
	}

	view := viewOf(tid, o.Manifest)
	view.SenderName, view.SenderFP, view.Addr = name, o.SenderFP, ip
	po := &pendingOffer{view: view, ip: ip, reply: make(chan decision, 1)}
	if err := s.addPending(po); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	d, ok, timedOut := s.waitDecision(r, po)
	switch {
	case !ok && timedOut:
		writeJSON(w, protocol.OfferReply{Accepted: false, TransferID: tid, Reason: "timed out"})
	case !ok:
		// The sender went away; nothing to answer.
	case !d.accept:
		writeJSON(w, protocol.OfferReply{Accepted: false, TransferID: tid, Reason: "declined"})
	default:
		if d.trust {
			if err := s.trust.Approve(o.SenderFP, name, o.SenderPub); err != nil {
				s.publishError(tid, view.Name, fmt.Errorf("could not save trusted sender: %w", err))
			}
		}
		writeJSON(w, protocol.OfferReply{Accepted: true, Token: s.mintUpload(up), TransferID: tid})
	}
}

// handleRecv streams an accepted single-file upload to disk. The one-shot
// token must come from the offering IP, and Content-Length must equal the
// manifest total.
func (s *Server) handleRecv(w http.ResponseWriter, r *http.Request) {
	up := s.takeUpload(r.URL.Query().Get("token"), false, remoteIP(r))
	if up == nil {
		http.Error(w, "invalid, expired or used token", http.StatusForbidden)
		return
	}
	if up.manifest.Kind != protocol.KindFile || len(up.manifest.Entries) != 1 {
		http.Error(w, "unsupported manifest kind", http.StatusBadRequest)
		return
	}
	e := up.manifest.Entries[0]
	if r.ContentLength != up.manifest.Total {
		err := fmt.Errorf("content length %d does not match the offered size %d", r.ContentLength, up.manifest.Total)
		s.publishError(up.transferID, e.Path, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dest, err := transfer.ReceiveFile(s.cfg.DownloadDir, e.Path, e.Size, r.Body, s.progressFn(up.transferID, e.Path, "in"))
	if err != nil {
		s.publishError(up.transferID, e.Path, err)
		code := http.StatusInternalServerError
		if errors.Is(err, transfer.ErrSize) {
			code = http.StatusBadRequest
		}
		http.Error(w, "transfer failed: "+err.Error(), code)
		return
	}
	s.hub.publish(Event{Type: "done", Data: map[string]any{
		"transfer_id": up.transferID, "name": filepath.Base(dest), "path": dest, "from": up.senderName,
	}})
	writeJSON(w, map[string]any{"ok": true})
}
