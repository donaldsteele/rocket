// Package receiver is the peer-facing receive side of a rocket host
// (protocol v2, docs/protocol.md): GET /api/hello, POST /api/offer and POST
// /api/recv, with offer verification, signed replies, rate and pending
// limits, one-shot upload tokens, the decryption of the body and its checks
// against the signed manifest.
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
	"github.com/donaldsteele/rocket/internal/protocol/securestream"
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
	key        []byte   // peer only: the body key of this transfer
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
		Files: transfer.Files(up.Manifest), Total: up.Manifest.Total, Encrypted: !up.Drop,
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
func (rx *Receiver) failIncoming(w http.ResponseWriter, r *http.Request, up *Upload, err error) {
	res, err := rx.cfg.Ledger.End(up.TransferID, err)
	if res == history.ResultCancelled {
		answerGone(w, r)
		return
	}
	http.Error(w, "transfer failed: "+err.Error(), RecvCode(err))
}

// goneDrain bounds how long answerGone keeps reading a cancelled upload.
const goneDrain = 2 * time.Second

// goneMsg is the body of the 410 answer to a cancelled upload.
const goneMsg = "cancelled by the receiver"

// answerGone tells the sender its upload was cancelled here. The 410 is
// flushed first, then the rest of the body is read and dropped for up to
// goneDrain, and the connection is closed. Returning with unread data would
// close the connection with bytes still queued, and the kernel answers that
// with a reset; on Windows the reset can destroy the 410 before the sender
// has read it, so the sender would report a network error instead of the
// cancel. The sender stops writing once it has the answer, which ends the
// drain early.
//
// The drain reads the raw connection (hijacked): the cancel woke the body
// read with a past read deadline, and the chunked decoder and
// http.MaxBytesReader keep that timeout error for good, so draining through
// r.Body would stop at once and bring the reset back. The connection is
// never reused either: the failed read may have cancelled its context.
func answerGone(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	if conn, brw, err := rc.Hijack(); err == nil {
		defer conn.Close()
		if conn.SetDeadline(time.Now().Add(goneDrain)) != nil {
			return
		}
		fmt.Fprintf(brw, "HTTP/1.1 410 Gone\r\nContent-Type: text/plain; charset=utf-8\r\n"+
			"X-Content-Type-Options: nosniff\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s\n",
			len(goneMsg)+1, goneMsg)
		if brw.Flush() == nil {
			_, _ = io.Copy(io.Discard, brw)
		}
		return
	}
	// Not hijackable (HTTP/2, a test recorder): answer through w and drain
	// what r.Body still gives.
	duplex := rc.EnableFullDuplex() == nil
	w.Header().Set("Connection", "close")
	http.Error(w, goneMsg, http.StatusGone)
	if !duplex || rc.Flush() != nil {
		return
	}
	body := r.Body
	if cb, ok := body.(*ctxBody); ok {
		body = cb.rc // ctxBody refuses reads once the transfer is cancelled
	}
	if rc.SetReadDeadline(time.Now().Add(goneDrain)) != nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
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
// match the offer (sizes, entries, start point, body limit) or the encrypted
// stream is bad (a frame that fails authentication, cut off, oversized or
// followed by more data), 500 otherwise.
func RecvCode(err error) int {
	var mbe *http.MaxBytesError
	for _, bad := range []error{
		transfer.ErrSize, transfer.ErrArchive, transfer.ErrStart,
		securestream.ErrAuth, securestream.ErrTruncated, securestream.ErrTrailingData,
		securestream.ErrFrameSize, securestream.ErrCounter,
	} {
		if errors.Is(err, bad) {
			return http.StatusBadRequest
		}
	}
	if errors.As(err, &mbe) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
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
// Every answer is signed with this host's key (protocol.SignReply); an
// accepted one carries a fresh ephemeral key, from which both sides derive
// the body key. An offer of an older protocol gets 426 and
// protocol.OldSenderMsg.
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
	if o.Proto < protocol.Version {
		// A rocket 1.0 sender: tell it plainly to update.
		w.Header().Set("Upgrade", fmt.Sprintf("rocket/%d", protocol.Version))
		http.Error(w, protocol.OldSenderMsg, http.StatusUpgradeRequired)
		return
	}
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
	offerSB, err := protocol.SigningBytes(&o)
	if err != nil {
		http.Error(w, "offer rejected: "+err.Error(), http.StatusBadRequest)
		return
	}

	// This side's ephemeral key and the body key are made before anyone is
	// asked, so an offer whose key cannot be used fails at once.
	tid := NewToken()
	ephPriv, eph, err := protocol.NewEphemeral()
	if err != nil {
		http.Error(w, "cannot make a key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	key, err := protocol.BodyKey(ephPriv, o.Eph, &o, tid)
	if err != nil {
		http.Error(w, "offer rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := CleanLabel(o.SenderName, o.SenderFP)
	up := &Upload{TransferID: tid, Manifest: o.Manifest, SenderName: name, SenderFP: o.SenderFP, IP: ip, key: key}
	answer := func(reply protocol.OfferReply) {
		reply.TransferID = tid
		if reply.Accepted {
			reply.Token, reply.Eph = rx.MintUpload(up), eph
		}
		protocol.SignReply(rx.cfg.Identity, offerSB, &reply)
		writeJSON(w, reply)
	}

	if autoOK {
		answer(protocol.OfferReply{Accepted: true})
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
		answer(protocol.OfferReply{Reason: protocol.ReasonTimedOut})
	case !ok:
		// The sender went away; nothing to answer.
	case !d.Accept:
		answer(protocol.OfferReply{Reason: protocol.ReasonDeclined})
	default:
		if d.Trust {
			if err := rx.cfg.Trust.Approve(o.SenderFP, name, o.SenderPub); err != nil {
				rx.cfg.Ledger.Error(tid, view.Name, fmt.Errorf("could not save trusted sender: %w", err))
			}
		}
		answer(protocol.OfferReply{Accepted: true})
	}
}

// HandleRecv streams an accepted upload into the Sink. The one-shot token
// must come from the offering IP. The body is a securestream under the key
// agreed for this transfer (protocol v2): a start header, which must be the
// beginning of the transfer, then every file's bytes in manifest order,
// checked against the signed manifest. Content-Length, when given, must be
// exactly the sealed size. Any failure, including a frame that fails
// authentication, removes everything stored. The receive is registered with
// the transfer manager: cancelling it stops the read, removes what was
// stored and answers the sender 410 Gone.
func (rx *Receiver) HandleRecv(w http.ResponseWriter, r *http.Request) {
	up := rx.TakeUpload(r.URL.Query().Get("token"), false, RemoteIP(r))
	if up == nil || up.key == nil {
		http.Error(w, "invalid, expired or used token", http.StatusForbidden)
		return
	}
	label := transfer.Label(up.Manifest)
	want := transfer.SealedBodySize(up.Manifest, transfer.Start{})
	r.Body = http.MaxBytesReader(w, r.Body, want)
	rx.StartIncoming(w, r, up, label)
	progress := rx.cfg.Ledger.Progress(up.TransferID, label, "in")
	out, err := rx.cfg.Sink.Begin(up.TransferID, up.Manifest, progress)
	if err != nil {
		rx.failIncoming(w, r, up, err)
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != want {
		out.Abort()
		rx.failIncoming(w, r, up, fmt.Errorf("%w: content length %d, the offer needs %d", transfer.ErrSize, r.ContentLength, want))
		return
	}
	body, err := securestream.NewReader(r.Body, up.key)
	if err != nil {
		out.Abort()
		rx.failIncoming(w, r, up, err)
		return
	}
	res, err := transfer.ReceiveBody(out, up.Manifest, body)
	if err != nil {
		rx.failIncoming(w, r, up, err)
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
