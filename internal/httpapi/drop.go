package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/receiver"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// DropCookie remembers a browser guest whose drop was accepted. Its value is a
// server-issued token; only its SHA-256 is stored (drop_tokens.json).
const DropCookie = "rocket_drop"

// dropFile is one file a browser guest offers: its name and size.
type dropFile = transfer.BrowserFile

// handleDropOffer is step one of a browser drop: POST /drop/offer
// {files:[{name,size}]}. It long-polls for the person's decision unless the
// browser carries a valid rocket_drop cookie (or is this computer's own UI,
// proven by the CSRF header). On the first accept it sets the cookie. The
// reply carries a one-shot token for POST /drop/upload.
func (s *Server) handleDropOffer(w http.ResponseWriter, r *http.Request) {
	if !s.isLocal(r) && !s.offers.Allow(remoteIP(r)) {
		http.Error(w, "too many offers; slow down", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxOfferBytes)
	decoded := limitOfferRead(w)
	var body struct {
		Files []dropFile `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad offer", http.StatusBadRequest)
		return
	}
	decoded()
	m, err := dropManifest(body.Files)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tid := randToken()
	ip := remoteIP(r)
	label := "browser guest (" + ip + ")"
	names := make([]string, len(body.Files))
	for i, f := range body.Files {
		names[i] = transfer.BaseName(f.Name)
	}
	up := &upload{TransferID: tid, Manifest: m, SenderName: label, IP: ip, Drop: true, Names: names}

	// A remembered browser skips the prompt unless auto-accept is off.
	remembered := false
	if c, err := r.Cookie(DropCookie); err == nil && s.autoAccept() && s.drops.Valid(c.Value) {
		remembered = true
	}
	if remembered || (s.isLocal(r) && hasCSRF(r)) {
		writeJSON(w, protocol.OfferReply{Accepted: true, Token: s.rx.MintUpload(up), TransferID: tid})
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
		writeJSON(w, protocol.OfferReply{Accepted: false, TransferID: tid, Reason: protocol.ReasonTimedOut})
	case !ok:
	case !d.Accept:
		writeJSON(w, protocol.OfferReply{Accepted: false, TransferID: tid, Reason: protocol.ReasonDeclined})
	default:
		if tok, err := s.drops.Issue(); err == nil {
			http.SetCookie(w, &http.Cookie{
				Name: DropCookie, Value: tok, Path: "/",
				MaxAge:   int(peers.DropTokenTTL.Seconds()),
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
		}
		writeJSON(w, protocol.OfferReply{Accepted: true, Token: s.rx.MintUpload(up), TransferID: tid})
	}
}

// dropManifest checks a browser's file list and builds a validated manifest
// of safe bare names (repeated names are numbered: "a (1).txt").
func dropManifest(files []dropFile) (protocol.Manifest, error) {
	return transfer.BrowserManifest(files, false)
}

// handleDropUpload is step two: POST /drop/upload?token= with a multipart
// body, one file per part, in offer order. Names and sizes must match the
// offer exactly. Parts are streamed to disk, never buffered; on any mismatch
// every file of this upload is removed.
func (s *Server) handleDropUpload(w http.ResponseWriter, r *http.Request) {
	up := s.rx.TakeUpload(r.URL.Query().Get("token"), true, remoteIP(r))
	if up == nil {
		http.Error(w, "invalid, expired or used token", http.StatusForbidden)
		return
	}
	// Room for the announced bytes plus multipart framing.
	limit := up.Manifest.Total + 1<<20 + int64(len(up.Manifest.Entries))*4096
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	label := transfer.Label(up.Manifest)
	s.rx.StartIncoming(w, r, up, label)
	dir := s.downloadDir()
	x := transfer.NewExtractor(dir, up.Manifest, s.progressFn(up.TransferID, label, "in"))
	fail := func(code int, err error) {
		x.Abort()
		res, err := s.endTransfer(up.TransferID, err)
		if res == history.ResultCancelled {
			code = http.StatusGone
		}
		http.Error(w, "upload failed: "+err.Error(), code)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		fail(http.StatusBadRequest, errors.New("bad upload: not a multipart body"))
		return
	}
	for i := range up.Manifest.Entries {
		part, err := mr.NextPart()
		if err != nil {
			fail(http.StatusBadRequest, fmt.Errorf("expected %d files, got %d", len(up.Manifest.Entries), i))
			return
		}
		if part.FileName() == "" || transfer.BaseName(part.FileName()) != up.Names[i] {
			fail(http.StatusBadRequest, fmt.Errorf("file %d is not %q", i+1, up.Names[i]))
			return
		}
		if err := x.Write(i, part); err != nil {
			fail(receiver.RecvCode(err), err)
			return
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		fail(http.StatusBadRequest, errors.New("more files than offered"))
		return
	}
	res, err := x.Finish()
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	s.endTransfer(up.TransferID, nil)
	s.rx.Done(up, label, dir, res)
	writeJSON(w, map[string]any{"ok": true, "files": res.Files})
}
