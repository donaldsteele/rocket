package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

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

// handleRecv streams an accepted upload to disk. The one-shot token must come
// from the offering IP. For kind:file the body is the raw file and
// Content-Length must equal the manifest total; for kind:multi it is a tar
// whose entries must match the manifest exactly, extracted under a fresh
// top-level name in the download folder.
func (s *Server) handleRecv(w http.ResponseWriter, r *http.Request) {
	up := s.takeUpload(r.URL.Query().Get("token"), false, remoteIP(r))
	if up == nil {
		http.Error(w, "invalid, expired or used token", http.StatusForbidden)
		return
	}
	label := transfer.Label(up.manifest)
	progress := s.progressFn(up.transferID, label, "in")
	switch up.manifest.Kind {
	case protocol.KindFile:
		e := up.manifest.Entries[0]
		if r.ContentLength != up.manifest.Total {
			err := fmt.Errorf("content length %d does not match the offered size %d", r.ContentLength, up.manifest.Total)
			s.publishError(up.transferID, label, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		dest, err := transfer.ReceiveFile(s.cfg.DownloadDir, e.Path, e.Size, r.Body, progress)
		if err != nil {
			s.failRecv(w, up, err)
			return
		}
		s.publishDone(up, label, transfer.Result{Paths: []string{dest}, Files: 1, Bytes: e.Size})
	case protocol.KindMulti:
		r.Body = http.MaxBytesReader(w, r.Body, tarLimit(up.manifest))
		res, err := transfer.ReceiveTar(s.cfg.DownloadDir, up.manifest, r.Body, progress)
		if err != nil {
			s.failRecv(w, up, err)
			return
		}
		s.publishDone(up, label, res)
	default:
		http.Error(w, "unsupported manifest kind", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// tarLimit bounds a kind:multi body: the announced bytes plus room for tar
// framing (a header, PAX records for long or non-ASCII names, padding) per
// entry and the end-of-archive marker.
func tarLimit(m protocol.Manifest) int64 {
	return m.Total + int64(len(m.Entries))*16<<10 + 1<<20
}

// failRecv reports a failed receive as an error event and HTTP error: 400 for
// a body that does not match the offer, 500 for a local failure.
func (s *Server) failRecv(w http.ResponseWriter, up *upload, err error) {
	s.publishError(up.transferID, transfer.Label(up.manifest), err)
	http.Error(w, "transfer failed: "+err.Error(), recvCode(err))
}

// recvCode maps a receive error to an HTTP status: 400 when the body does not
// match the offer (sizes, archive entries, body limit), 500 otherwise.
func recvCode(err error) int {
	var mbe *http.MaxBytesError
	if errors.Is(err, transfer.ErrSize) || errors.Is(err, transfer.ErrArchive) || errors.As(err, &mbe) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// publishDone emits the "done" event for a finished receive. name is the
// transfer label used by its progress events; path is the single top-level
// file or folder created (or the download folder when there are several).
func (s *Server) publishDone(up *upload, name string, res transfer.Result) {
	path := s.cfg.DownloadDir
	if len(res.Paths) == 1 {
		path = res.Paths[0]
	}
	s.hub.publish(Event{Type: "done", Data: map[string]any{
		"transfer_id": up.transferID, "name": name, "path": path, "paths": res.Paths,
		"files": res.Files, "bytes": res.Bytes, "from": up.senderName,
	}})
}
