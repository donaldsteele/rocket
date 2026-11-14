package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// DropCookie remembers a browser guest whose drop was accepted. Its value is a
// server-issued token; only its SHA-256 is stored (drop_tokens.json).
const DropCookie = "rocket_drop"

type dropFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// handleDropOffer is step one of a browser drop: POST /drop/offer
// {files:[{name,size}]}. It long-polls for the person's decision unless the
// browser carries a valid rocket_drop cookie (or is this computer's own UI,
// proven by the CSRF header). On the first accept it sets the cookie. The
// reply carries a one-shot token for POST /drop/upload.
func (s *Server) handleDropOffer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxOfferBytes)
	var body struct {
		Files []dropFile `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad offer", http.StatusBadRequest)
		return
	}
	m, err := dropManifest(body.Files)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tid := randToken()
	ip := remoteIP(r)
	label := "browser guest (" + ip + ")"
	up := &upload{transferID: tid, manifest: m, senderName: label, ip: ip, drop: true}

	remembered := false
	if c, err := r.Cookie(DropCookie); err == nil && s.drops.Valid(c.Value) {
		remembered = true
	}
	if remembered || (s.isLocal(r) && hasCSRF(r)) {
		writeJSON(w, protocol.OfferReply{Accepted: true, Token: s.mintUpload(up), TransferID: tid})
		return
	}

	view := viewOf(tid, m)
	view.SenderName, view.FromBrowser, view.Addr = label, true, ip
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
	case !d.accept:
		writeJSON(w, protocol.OfferReply{Accepted: false, TransferID: tid, Reason: "declined"})
	default:
		if tok, err := s.drops.Issue(); err == nil {
			http.SetCookie(w, &http.Cookie{
				Name: DropCookie, Value: tok, Path: "/",
				MaxAge:   int(peers.DropTokenTTL.Seconds()),
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
		}
		writeJSON(w, protocol.OfferReply{Accepted: true, Token: s.mintUpload(up), TransferID: tid})
	}
}

// dropManifest checks a browser's file list and builds a manifest of safe
// bare names (duplicates are allowed; each file gets a unique name on disk).
func dropManifest(files []dropFile) (protocol.Manifest, error) {
	if len(files) == 0 {
		return protocol.Manifest{}, errors.New("no files")
	}
	if len(files) > protocol.MaxEntries {
		return protocol.Manifest{}, fmt.Errorf("at most %d files per drop", protocol.MaxEntries)
	}
	m := protocol.Manifest{Kind: protocol.KindFile}
	if len(files) > 1 {
		m.Kind = protocol.KindMulti
	}
	for _, f := range files {
		if f.Size < 0 {
			return protocol.Manifest{}, errors.New("negative size")
		}
		m.Total += f.Size
		if m.Total > protocol.MaxTotal {
			return protocol.Manifest{}, errors.New("drop too large")
		}
		m.Entries = append(m.Entries, protocol.Entry{Path: transfer.BaseName(f.Name), Size: f.Size, Mode: 0o644})
	}
	return m, nil
}

// handleDropUpload is step two: POST /drop/upload?token= with a multipart
// body, one file per part, in offer order. Names and sizes must match the
// offer exactly. Parts are streamed to disk, never buffered; on any mismatch
// every file of this upload is removed.
func (s *Server) handleDropUpload(w http.ResponseWriter, r *http.Request) {
	up := s.takeUpload(r.URL.Query().Get("token"), true, remoteIP(r))
	if up == nil {
		http.Error(w, "invalid, expired or used token", http.StatusForbidden)
		return
	}
	// Room for the announced bytes plus multipart framing.
	limit := up.manifest.Total + 1<<20 + int64(len(up.manifest.Entries))*4096
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}

	var written []string
	fail := func(code int, err error) {
		for _, p := range written {
			os.Remove(p)
		}
		s.publishError(up.transferID, firstName(up), err)
		http.Error(w, "upload failed: "+err.Error(), code)
	}
	for i, e := range up.manifest.Entries {
		part, err := mr.NextPart()
		if err != nil {
			fail(http.StatusBadRequest, fmt.Errorf("expected %d files, got %d", len(up.manifest.Entries), i))
			return
		}
		if part.FileName() == "" || transfer.BaseName(part.FileName()) != e.Path {
			fail(http.StatusBadRequest, fmt.Errorf("file %d is not %q", i+1, e.Path))
			return
		}
		dest, err := transfer.ReceiveFile(s.cfg.DownloadDir, e.Path, e.Size, part, s.progressFn(up.transferID, e.Path, "in"))
		if err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, transfer.ErrSize) {
				code = http.StatusBadRequest
			}
			fail(code, err)
			return
		}
		written = append(written, dest)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		fail(http.StatusBadRequest, errors.New("more files than offered"))
		return
	}
	for _, dest := range written {
		s.hub.publish(Event{Type: "done", Data: map[string]any{
			"transfer_id": up.transferID, "name": filepath.Base(dest), "path": dest, "from": up.senderName,
		}})
	}
	writeJSON(w, map[string]any{"ok": true, "files": len(written)})
}

func firstName(up *upload) string {
	if len(up.manifest.Entries) > 0 {
		return up.manifest.Entries[0].Path
	}
	return ""
}
