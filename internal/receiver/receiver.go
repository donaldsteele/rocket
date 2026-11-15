// Package receiver is the peer-facing receive side of a rocket host
// (protocol v1, docs/protocol.md): GET /api/hello, POST /api/offer and POST
// /api/recv, with offer verification, rate and pending limits, one-shot
// upload tokens, and the checks of the body against the signed manifest.
//
// Two interfaces connect it to its host. A Decider asks the person about an
// offer; the answer comes back later through Receiver.Answer. A Sink stores
// the content of an accepted transfer: FSSink writes to a download folder as
// desktop rocket does, FilesSink adapts a per-file store such as Android's
// MediaStore. Desktop rocket (internal/httpapi) and the Android core
// (mobile/rocketcore) both receive through this package, so the wire has one
// implementation.
package receiver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// Decider asks the person whether to accept an offer.
type Decider interface {
	// Ask shows offer o. It must not block: the answer comes later through
	// Receiver.Answer(o.ID, ...). The offer stops waiting after
	// protocol.OfferWait or when the sender withdraws it; either way an
	// "offer_gone" event is published.
	Ask(o IncomingOffer)
}

// DeciderFunc is a func that implements Decider.
type DeciderFunc func(IncomingOffer)

// Ask implements Decider.
func (f DeciderFunc) Ask(o IncomingOffer) { f(o) }

// IncomingOffer is what the person sees of an offer waiting for a decision
// (the "offer" event, GET /api/incoming on desktop).
type IncomingOffer struct {
	ID          string   `json:"id"` // same as TransferID; answer with Receiver.Answer
	TransferID  string   `json:"transfer_id"`
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`  // first entry path
	Size        int64    `json:"size"`  // total bytes
	Files       int      `json:"files"` // file count (folders not counted)
	Names       []string `json:"names"` // first few entry paths (files and folders)
	SenderName  string   `json:"sender_name"`
	SenderFP    string   `json:"sender_fp,omitempty"`
	FromBrowser bool     `json:"from_browser"`
	Addr        string   `json:"addr"` // remote IP
}

// Decision is the person's answer to a pending offer.
type Decision struct {
	Accept bool
	Trust  bool // remember the sender (peer offers) or the browser (drops)
}

// Pending is an offer waiting for a decision. Reply must have room for one
// Decision (buffered, capacity 1).
type Pending struct {
	View  IncomingOffer
	IP    string
	Reply chan Decision
}

// Upload is an accepted offer waiting for its bytes, keyed by a one-shot
// token that is valid for protocol.TokenTTL and only from the offering IP.
type Upload struct {
	TransferID string
	Manifest   protocol.Manifest
	SenderName string
	SenderFP   string
	IP         string
	Drop       bool     // desktop browser drop (POST /drop/upload) vs peer (POST /api/recv)
	Names      []string // drop only: base name the browser gave each file, in order
	expires    time.Time
}

// ErrTooMany is returned when no further offer may wait for a decision.
var ErrTooMany = errors.New("too many pending offers")

// Config wires a Receiver to its host.
type Config struct {
	Identity *node.Identity
	Trust    *peers.Store
	Ledger   *Ledger
	Decider  Decider
	Sink     Sink
	// Hello returns the version and OS that GET /api/hello reports; nil
	// reports "dev" and no OS.
	Hello func() (version, os string)
	// AutoAccept reports whether a trusted sender (pinned key) skips the
	// prompt; nil means always.
	AutoAccept func() bool
	// AllowOffer rate-limits offers per remote IP; nil uses a RateLimiter
	// of OfferBurst / OfferRefill.
	AllowOffer func(ip string) bool
	// OfferReadTimeout bounds an offer body's arrival; 0 means
	// DefaultOfferReadTimeout.
	OfferReadTimeout time.Duration
}

// Receiver holds the receive state: offers waiting for a decision and
// accepted uploads waiting for their bytes.
type Receiver struct {
	cfg      Config
	verifier *protocol.Verifier

	mu      sync.Mutex
	pending map[string]*Pending // keyed by transfer ID
	uploads map[string]*Upload  // keyed by one-shot upload token
}

// New builds a receiver. Identity, Trust, Ledger (with Transfers), Decider
// and Sink are required.
func New(cfg Config) *Receiver {
	if cfg.AllowOffer == nil {
		cfg.AllowOffer = NewRateLimiter(OfferBurst, OfferRefill).Allow
	}
	if cfg.OfferReadTimeout <= 0 {
		cfg.OfferReadTimeout = DefaultOfferReadTimeout
	}
	if cfg.AutoAccept == nil {
		cfg.AutoAccept = func() bool { return true }
	}
	if cfg.Hello == nil {
		cfg.Hello = func() (string, string) { return "dev", "" }
	}
	return &Receiver{
		cfg:      cfg,
		verifier: protocol.NewVerifier(cfg.Identity.Fingerprint()),
		pending:  map[string]*Pending{},
		uploads:  map[string]*Upload{},
	}
}

// Ledger returns the ledger the receiver records its transfers in.
func (rx *Receiver) Ledger() *Ledger { return rx.cfg.Ledger }

// Handler serves only the peer protocol: GET /api/hello, POST /api/offer and
// POST /api/recv. Desktop rocket mounts the three handlers in its own route
// table instead.
func (rx *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hello", rx.HandleHello)
	mux.HandleFunc("POST /api/offer", rx.HandleOffer)
	mux.HandleFunc("POST /api/recv", rx.HandleRecv)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

// --- pending offers ---

// AddPending registers p unless the global or per-IP limit is reached
// (ErrTooMany).
func (rx *Receiver) AddPending(p *Pending) error {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	if err := rx.pendingRoomLocked(p.IP); err != nil {
		return err
	}
	rx.pending[p.View.TransferID] = p
	return nil
}

// PendingRoom reports ErrTooMany when no further offer from ip could wait
// for a decision.
func (rx *Receiver) PendingRoom(ip string) error {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	return rx.pendingRoomLocked(ip)
}

func (rx *Receiver) pendingRoomLocked(ip string) error {
	if len(rx.pending) >= protocol.MaxPending {
		return ErrTooMany
	}
	n := 0
	for _, p := range rx.pending {
		if p.IP == ip {
			n++
		}
	}
	if n >= protocol.MaxPendingIP {
		return ErrTooMany
	}
	return nil
}

// DropPending forgets pending offer id.
func (rx *Receiver) DropPending(id string) {
	rx.mu.Lock()
	delete(rx.pending, id)
	rx.mu.Unlock()
}

// PendingOffers lists the offers waiting for a decision.
func (rx *Receiver) PendingOffers() []IncomingOffer {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	out := make([]IncomingOffer, 0, len(rx.pending))
	for _, p := range rx.pending {
		out = append(out, p.View)
	}
	return out
}

// Answer delivers the person's decision to pending offer id. It reports
// false when no such offer waits (already answered, timed out, withdrawn).
func (rx *Receiver) Answer(id string, accept, trust bool) bool {
	rx.mu.Lock()
	p := rx.pending[id]
	delete(rx.pending, id)
	rx.mu.Unlock()
	if p == nil {
		return false
	}
	p.Reply <- Decision{Accept: accept, Trust: trust}
	return true
}

// Wait asks the Decider about p and blocks until the person decides, ctx
// ends (the requester went away: the offer is withdrawn), or
// protocol.OfferWait passes. ok is false when no decision arrived (timedOut
// tells which). Every way out publishes "offer_gone" {transfer_id, reason}
// so every prompt is dropped; a decline, a time-out or a withdrawal is
// written to the history (an accepted offer is recorded when its transfer
// ends). p must have been added with AddPending.
func (rx *Receiver) Wait(ctx context.Context, p *Pending) (d Decision, ok, timedOut bool) {
	rx.cfg.Decider.Ask(p.View)
	t := time.NewTimer(protocol.OfferWait)
	defer t.Stop()
	reason := "withdrawn"
	select {
	case d = <-p.Reply:
		ok = true
		reason = "declined"
		if d.Accept {
			reason = "accepted"
		}
	case <-ctx.Done():
	case <-t.C:
		timedOut = true
		reason = "timed out"
	}
	rx.DropPending(p.View.TransferID)
	l := rx.cfg.Ledger
	l.Publish(Event{Type: "offer_gone", Data: map[string]any{"transfer_id": p.View.TransferID, "reason": reason}})
	switch {
	case ok && !d.Accept:
		l.RecordOffer(p.View, history.ResultDeclined, "declined on "+l.here())
	case timedOut:
		l.RecordOffer(p.View, history.ResultTimeout, "no answer within "+protocol.OfferWait.String())
	case !ok:
		l.RecordOffer(p.View, history.ResultCancelled, "the sender withdrew the offer")
	}
	return d, ok, timedOut
}

// --- upload tokens ---

// MintUpload stores up under a fresh one-shot token and returns the token.
func (rx *Receiver) MintUpload(up *Upload) string {
	token := NewToken()
	now := time.Now()
	up.expires = now.Add(protocol.TokenTTL)
	rx.mu.Lock()
	for k, u := range rx.uploads {
		if now.After(u.expires) {
			delete(rx.uploads, k)
		}
	}
	rx.uploads[token] = up
	rx.mu.Unlock()
	return token
}

// TakeUpload consumes a token. It returns nil if the token is unknown, used,
// expired, of the wrong flow (drop vs peer), or presented from another IP.
func (rx *Receiver) TakeUpload(token string, drop bool, ip string) *Upload {
	if token == "" {
		return nil
	}
	rx.mu.Lock()
	up := rx.uploads[token]
	delete(rx.uploads, token) // one-shot, even when the checks below fail
	rx.mu.Unlock()
	if up == nil || up.Drop != drop || up.IP != ip || time.Now().After(up.expires) {
		return nil
	}
	return up
}

// --- incoming transfers ---

// StartIncoming registers an accepted upload with the transfer manager and
// makes r.Body cancellable: cancelling the transfer stops the body read at
// once (a past read deadline on the connection wakes a blocked read) and
// every later read fails, so the receive aborts and removes its partial
// files.
func (rx *Receiver) StartIncoming(w http.ResponseWriter, r *http.Request, up *Upload, label string) {
	rc := http.NewResponseController(w)
	ctx := rx.cfg.Ledger.Transfers.Start(r.Context(), transfer.Info{
		ID: up.TransferID, Dir: history.DirIn, Name: label, PeerName: up.SenderName, PeerFP: up.SenderFP,
		Files: transfer.Files(up.Manifest), Total: up.Manifest.Total,
	}, func() { _ = rc.SetReadDeadline(time.Now()) })
	r.Body = &ctxBody{ctx: ctx, rc: r.Body}
}

// ctxBody fails reads once its context is done.
type ctxBody struct {
	ctx context.Context
	rc  io.ReadCloser
}

func (b *ctxBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, transfer.ErrCancelled
	}
	n, err := b.rc.Read(p)
	if err != nil && b.ctx.Err() != nil {
		return n, transfer.ErrCancelled
	}
	return n, err
}

func (b *ctxBody) Close() error { return b.rc.Close() }

// failIncoming ends a failed receive: history, an "error" or "cancelled"
// event, and the HTTP answer. A cancel on this side answers 410 Gone so the
// sender can tell a cancel from a failure.
func (rx *Receiver) failIncoming(w http.ResponseWriter, up *Upload, err error) {
	res, err := rx.cfg.Ledger.End(up.TransferID, err)
	if res == history.ResultCancelled {
		http.Error(w, "cancelled by the receiver", http.StatusGone)
		return
	}
	http.Error(w, "transfer failed: "+err.Error(), RecvCode(err))
}

// Done emits the "done" event for a finished receive. name is the transfer
// label used by its progress events; path is the single top-level file or
// folder created, or where when there are several.
func (rx *Receiver) Done(up *Upload, name, where string, res transfer.Result) {
	path := where
	if len(res.Paths) == 1 {
		path = res.Paths[0]
	}
	rx.cfg.Ledger.Publish(Event{Type: "done", Data: map[string]any{
		"transfer_id": up.TransferID, "name": name, "path": path, "paths": res.Paths,
		"files": res.Files, "bytes": res.Bytes, "from": up.SenderName,
	}})
}

// RecvCode maps a receive error to an HTTP status: 400 when the body does not
// match the offer (sizes, archive entries, body limit), 500 otherwise.
func RecvCode(err error) int {
	var mbe *http.MaxBytesError
	if errors.Is(err, transfer.ErrSize) || errors.Is(err, transfer.ErrArchive) || errors.As(err, &mbe) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// TarLimit bounds a kind:multi body: the announced bytes plus room for tar
// framing (a header, PAX records for long or non-ASCII names, padding) per
// entry and the end-of-archive marker.
func TarLimit(m protocol.Manifest) int64 {
	return m.Total + int64(len(m.Entries))*16<<10 + 1<<20
}

// --- peer protocol handlers ---

// HandleHello answers GET /api/hello with this host's protocol identity.
func (rx *Receiver) HandleHello(w http.ResponseWriter, _ *http.Request) {
	version, osName := rx.cfg.Hello()
	id := rx.cfg.Identity
	writeJSON(w, protocol.Hello{
		Proto: protocol.Version, Name: id.Name, FP: id.Fingerprint(),
		Pub: id.PublicKeyB64(), Version: version, OS: osName,
	})
}

// HandleOffer verifies a signed peer offer and long-polls for the person's
// decision (skipped for a trusted sender whose key matches the pinned key).
//
// Every offer with a good signature spends a slot in the replay cache, so
// offers are rate-limited per IP, and an offer that could only be refused
// for want of room to wait (an untrusted sender while the pending limits are
// reached) is refused before its signature is checked.
func (rx *Receiver) HandleOffer(w http.ResponseWriter, r *http.Request) {
	ip := RemoteIP(r)
	if !rx.cfg.AllowOffer(ip) {
		http.Error(w, "too many offers; slow down", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxOfferBytes)
	decoded := LimitOfferRead(w, rx.cfg.OfferReadTimeout)
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
	decoded()
	// Trust-on-first-use with proof of key possession: a sender whose
	// fingerprint is trusted AND whose key matches the pinned key skips the
	// prompt, unless auto-accept is switched off. (The key is proven by
	// Verify below; until then this only picks the path.)
	autoOK := rx.cfg.AutoAccept() && rx.cfg.Trust.IsTrusted(o.SenderFP, o.SenderPub)
	if !autoOK {
		if err := rx.PendingRoom(ip); err != nil {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
	}
	if _, err := rx.verifier.Verify(&o); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, protocol.ErrBusy) {
			code = http.StatusServiceUnavailable
		}
		http.Error(w, "offer rejected: "+err.Error(), code)
		return
	}

	tid := NewToken()
	name := CleanLabel(o.SenderName, o.SenderFP)
	up := &Upload{TransferID: tid, Manifest: o.Manifest, SenderName: name, SenderFP: o.SenderFP, IP: ip}

	if autoOK {
		writeJSON(w, protocol.OfferReply{Accepted: true, Token: rx.MintUpload(up), TransferID: tid})
		return
	}

	view := View(tid, o.Manifest)
	view.SenderName, view.SenderFP, view.Addr = name, o.SenderFP, ip
	p := &Pending{View: view, IP: ip, Reply: make(chan Decision, 1)}
	if err := rx.AddPending(p); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	d, ok, timedOut := rx.Wait(r.Context(), p)
	switch {
	case !ok && timedOut:
		writeJSON(w, protocol.OfferReply{Accepted: false, TransferID: tid, Reason: protocol.ReasonTimedOut})
	case !ok:
		// The sender went away; nothing to answer.
	case !d.Accept:
		writeJSON(w, protocol.OfferReply{Accepted: false, TransferID: tid, Reason: protocol.ReasonDeclined})
	default:
		if d.Trust {
			if err := rx.cfg.Trust.Approve(o.SenderFP, name, o.SenderPub); err != nil {
				rx.cfg.Ledger.Error(tid, view.Name, fmt.Errorf("could not save trusted sender: %w", err))
			}
		}
		writeJSON(w, protocol.OfferReply{Accepted: true, Token: rx.MintUpload(up), TransferID: tid})
	}
}

// HandleRecv streams an accepted upload into the Sink. The one-shot token
// must come from the offering IP. For kind:file the body is the raw file and
// Content-Length must equal the manifest total; for kind:multi it is a tar
// whose entries must match the manifest exactly. The receive is registered
// with the transfer manager: cancelling it stops the read, removes what was
// stored and answers the sender 410 Gone.
func (rx *Receiver) HandleRecv(w http.ResponseWriter, r *http.Request) {
	up := rx.TakeUpload(r.URL.Query().Get("token"), false, RemoteIP(r))
	if up == nil {
		http.Error(w, "invalid, expired or used token", http.StatusForbidden)
		return
	}
	label := transfer.Label(up.Manifest)
	if up.Manifest.Kind == protocol.KindMulti {
		r.Body = http.MaxBytesReader(w, r.Body, TarLimit(up.Manifest))
	}
	rx.StartIncoming(w, r, up, label)
	progress := rx.cfg.Ledger.Progress(up.TransferID, label, "in")
	out, err := rx.cfg.Sink.Begin(up.TransferID, up.Manifest, progress)
	if err != nil {
		rx.failIncoming(w, up, err)
		return
	}
	var res transfer.Result
	switch up.Manifest.Kind {
	case protocol.KindFile:
		if r.ContentLength != up.Manifest.Total {
			err = fmt.Errorf("%w: content length %d does not match the offered size %d", transfer.ErrSize, r.ContentLength, up.Manifest.Total)
			out.Abort()
			break
		}
		if err = out.Write(0, r.Body); err == nil {
			res, err = out.Finish()
		}
		if err != nil {
			out.Abort()
		}
	default: // kind:multi (Verify accepts no other kind)
		res, err = transfer.ExtractTar(out, up.Manifest, r.Body)
	}
	if err != nil {
		rx.failIncoming(w, up, err)
		return
	}
	rx.cfg.Ledger.End(up.TransferID, nil)
	rx.Done(up, label, out.Location(), res)
	writeJSON(w, map[string]any{"ok": true})
}

// --- helpers ---

// View is the IncomingOffer for manifest m of transfer id, without the
// sender fields. Names are shown as they will be written: sanitized, so
// bidi overrides and other invisible characters cannot disguise them.
func View(id string, m protocol.Manifest) IncomingOffer {
	v := IncomingOffer{ID: id, TransferID: id, Kind: m.Kind, Size: m.Total, Files: transfer.Files(m)}
	for i, e := range m.Entries {
		if i == 5 {
			break
		}
		p := transfer.SanitizePath(e.Path)
		if i == 0 {
			v.Name = p
		}
		v.Names = append(v.Names, p)
	}
	return v
}

// CleanLabel makes an untrusted display name safe to show: control and
// format characters removed, whitespace collapsed, at most 64 runes; an
// empty result is fallback.
func CleanLabel(s, fallback string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.Join(strings.Fields(s), " ") {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n == 64 {
			break
		}
		b.WriteRune(r)
		n++
	}
	if b.Len() == 0 {
		return fallback
	}
	return b.String()
}

// NewToken returns 32 random hex characters: transfer IDs and upload tokens.
func NewToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
