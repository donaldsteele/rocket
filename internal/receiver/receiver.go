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
	"github.com/donaldsteele/rocket/internal/resume"
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
	SenderPub  string // peer only: the sender's key (base64), proven by its signed offer
	IP         string
	Drop       bool     // desktop browser drop (POST /drop/upload) vs peer (POST /api/recv)
	Names      []string // drop only: base name the browser gave each file, in order
	key        []byte   // peer only: the body key of this transfer
	// peer only: the resume point offered in the reply, and the kept
	// transfer it continues.
	resume  *protocol.ResumePoint
	keptID  string
	mh      string        // peer only: the manifest hash
	done    chan struct{} // closed when its receive has ended
	expires time.Time
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
	// Kept holds the transfers kept for a resume (docs/protocol.md
	// "Resume"); nil keeps nothing: a transfer broken by a network failure
	// is removed like any other failure. It needs a Sink whose Outputs are
	// Keepers to keep anything.
	Kept *KeptStore
	// TokenTTL is how long an upload token stays good; 0 means
	// protocol.TokenTTL (tests shorten it).
	TokenTTL time.Duration
}

// Receiver holds the receive state: offers waiting for a decision and
// accepted uploads waiting for their bytes.
type Receiver struct {
	cfg      Config
	verifier *protocol.Verifier

	mu         sync.Mutex
	pending    map[string]*Pending // keyed by transfer ID
	uploads    map[string]*Upload  // keyed by one-shot upload token
	active     map[string]*Upload  // peer bodies being received, keyed by transfer ID
	peerCancel map[string]bool     // transfer IDs the sender cancelled with a notice
	// broken are accepted peer transfers that broke for a network reason
	// before anything was stored, keyed by sender key and manifest hash
	// (acceptedKey), with when they stop counting: a retry continues them
	// without a new prompt, like a kept transfer.
	broken map[string]time.Time
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
		cfg:        cfg,
		verifier:   protocol.NewVerifier(cfg.Identity.Fingerprint()),
		pending:    map[string]*Pending{},
		uploads:    map[string]*Upload{},
		active:     map[string]*Upload{},
		peerCancel: map[string]bool{},
		broken:     map[string]time.Time{},
	}
}

// Ledger returns the ledger the receiver records its transfers in.
func (rx *Receiver) Ledger() *Ledger { return rx.cfg.Ledger }

// Handler serves only the peer protocol: GET /api/hello, POST /api/offer,
// POST /api/recv and POST /api/recv/cancel. Desktop rocket mounts the
// handlers in its own route table instead.
func (rx *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hello", rx.HandleHello)
	mux.HandleFunc("POST /api/offer", rx.HandleOffer)
	mux.HandleFunc("POST /api/recv", rx.HandleRecv)
	mux.HandleFunc("POST /api/recv/cancel", rx.HandleCancelNotice)
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
	ttl := rx.cfg.TokenTTL
	if ttl <= 0 {
		ttl = protocol.TokenTTL
	}
	up.expires = now.Add(ttl)
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
// makes r.Body cancellable: cancelling the transfer (here, or by the
// sender's cancel notice) stops the body read at once (a past read deadline
// on the connection wakes a blocked read) and every later read fails with
// transfer.ErrCancelled, so the receive aborts and removes its partial
// files. A connection that goes away by itself fails reads with
// transfer.ErrConnLost instead, and one that moves nothing for
// transfer.StallTimeout times out: both are network failures.
func (rx *Receiver) StartIncoming(w http.ResponseWriter, r *http.Request, up *Upload, label string) {
	rc := http.NewResponseController(w)
	tid := up.TransferID
	ctx := rx.cfg.Ledger.Transfers.Start(r.Context(), transfer.Info{
		ID: tid, Dir: history.DirIn, Name: label, PeerName: up.SenderName, PeerFP: up.SenderFP,
		Files: transfer.Files(up.Manifest), Total: up.Manifest.Total, Encrypted: !up.Drop,
	}, func() { _ = rc.SetReadDeadline(time.Now()) })
	r.Body = &ctxBody{ctx: ctx, rc: r.Body, ctl: rc, stopped: func() bool {
		return rx.cfg.Ledger.Transfers.Cancelled(tid) || rx.cancelledByPeer(tid)
	}}
}

// ctxBody fails reads once its context is done, and keeps a read deadline
// transfer.StallTimeout ahead while data comes in.
type ctxBody struct {
	ctx     context.Context
	rc      io.ReadCloser
	ctl     *http.ResponseController
	stopped func() bool // the transfer was stopped on purpose (a cancel)
	armed   time.Time   // when the read deadline was last moved
}

func (b *ctxBody) Read(p []byte) (int, error) {
	if b.ctx.Err() != nil && b.stopped() {
		return 0, transfer.ErrCancelled
	}
	if b.ctl != nil && time.Since(b.armed) > time.Second {
		b.armed = time.Now()
		if b.ctl.SetReadDeadline(b.armed.Add(transfer.StallTimeout)) == nil && b.ctx.Err() != nil {
			_ = b.ctl.SetReadDeadline(time.Now()) // a cancel came in meanwhile
		}
	}
	n, err := b.rc.Read(p)
	if err != nil && b.ctx.Err() != nil {
		if b.stopped() {
			return n, transfer.ErrCancelled
		}
		return n, fmt.Errorf("%w: %w", transfer.ErrConnLost, err)
	}
	return n, err
}

// disarm clears the read deadline once the body has been read.
func (b *ctxBody) disarm() {
	if b.ctl != nil {
		_ = b.ctl.SetReadDeadline(time.Time{})
	}
}

// cancelledByPeer reports whether the sender cancelled transfer tid with a
// notice.
func (rx *Receiver) cancelledByPeer(tid string) bool {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	return rx.peerCancel[tid]
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
	if errors.Is(err, errKeptGone) {
		return http.StatusConflict
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
	// prompt, unless auto-accept is switched off. So does an offer that
	// continues a transfer the person already accepted: same sender key,
	// same manifest, kept after a network failure, still being received
	// over a connection this side has not yet seen break, accepted but its
	// upload never came (or broke before anything was stored). (The key is
	// proven by Verify below; until then this only picks the path.)
	mh, _ := o.Manifest.Hash()
	continued := mh != "" && rx.acceptedFor(o.SenderPub, mh)
	resumable := rx.cfg.Kept != nil && mh != "" &&
		(rx.activeFor(o.SenderPub, mh) != nil || rx.cfg.Kept.Find(o.SenderFP, o.SenderPub, mh) != nil)
	trusted := rx.cfg.AutoAccept() && rx.cfg.Trust.IsTrusted(o.SenderFP, o.SenderPub)
	if !resumable && !continued && !trusted {
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
	var kept *Kept
	if resumable {
		// The sender only offers the same files again when it lost its
		// connection: let the old receive see the break and keep what it
		// stored, then continue that.
		rx.settleActive(o.SenderPub, mh)
		kept = rx.cfg.Kept.Find(o.SenderFP, o.SenderPub, mh)
		// The old receive may have ended before storing anything.
		continued = continued || rx.acceptedFor(o.SenderPub, mh)
	}
	if continued || kept != nil {
		// This offer takes over from the earlier attempt: its unused
		// upload token goes.
		rx.forgetAccepted(o.SenderPub, mh)
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
	up := &Upload{TransferID: tid, Manifest: o.Manifest, SenderName: name, SenderFP: o.SenderFP, SenderPub: o.SenderPub, IP: ip, key: key, mh: mh}
	answer := func(reply protocol.OfferReply) {
		reply.TransferID = tid
		if reply.Accepted {
			reply.Token, reply.Eph = rx.MintUpload(up), eph
		}
		protocol.SignReply(rx.cfg.Identity, offerSB, &reply)
		writeJSON(w, reply)
	}

	if kept != nil {
		p := kept.Point
		up.resume, up.keptID = &p, kept.ID
		rx.cfg.Kept.Offered(kept.ID, tid)
		answer(protocol.OfferReply{Accepted: true, Resume: &p})
		return
	}
	if trusted || continued {
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
// beginning of the transfer or the resume point offered in the reply, then
// every file's bytes from there in manifest order, checked against the
// signed manifest. Content-Length, when given, must be exactly the sealed
// size for that start. The receive is registered with the transfer
// manager: cancelling it stops the read, removes what was stored and
// answers the sender 410 Gone.
//
// When the body breaks for a network reason (the connection is lost, reset
// or stalls, or the stream is cut short) and Config.Kept is set, what was
// stored is kept for a resume (KeptStore); history says "interrupted". Any
// other failure, including a frame that fails authentication, removes
// everything stored, an earlier attempt's included.
func (rx *Receiver) HandleRecv(w http.ResponseWriter, r *http.Request) {
	up := rx.TakeUpload(r.URL.Query().Get("token"), false, RemoteIP(r))
	if up == nil || up.key == nil {
		http.Error(w, "invalid, expired or used token", http.StatusForbidden)
		return
	}
	tid, m := up.TransferID, up.Manifest
	label := transfer.Label(m)
	full := transfer.SealedBodySize(m, transfer.Start{})
	resumeSize := int64(-1)
	var resumeAt transfer.Start
	if up.resume != nil {
		resumeAt = transfer.Start{Entry: up.resume.Entry, Offset: up.resume.Offset}
		resumeSize = transfer.SealedBodySize(m, resumeAt)
	}
	r.Body = http.MaxBytesReader(w, r.Body, full) // a resumed body is never longer
	rx.StartIncoming(w, r, up, label)
	cb, _ := r.Body.(*ctxBody)
	up.done = make(chan struct{})
	rx.mu.Lock()
	rx.active[tid] = up
	rx.mu.Unlock()
	defer func() {
		rx.mu.Lock()
		delete(rx.active, tid)
		delete(rx.peerCancel, tid)
		rx.mu.Unlock()
		close(up.done)
	}()

	// failEarly ends a receive that stored nothing yet. A kept transfer it
	// was to continue stays kept after a network failure and is dropped
	// after any other.
	failEarly := func(err error) {
		err = rx.stopErr(tid, err)
		if up.keptID != "" && !transfer.IsNetwork(err) {
			rx.discardKept(up.keptID, "the sender's next attempt failed")
		}
		if up.keptID == "" {
			rx.markBroken(up, err)
		}
		rx.failIncoming(w, r, up, err)
	}
	if r.ContentLength >= 0 && r.ContentLength != full && r.ContentLength != resumeSize {
		failEarly(fmt.Errorf("%w: content length %d, the offer needs %d", transfer.ErrSize, r.ContentLength, full))
		return
	}
	body, err := securestream.NewReader(r.Body, up.key)
	if err != nil {
		failEarly(err)
		return
	}
	s, err := transfer.ReadStart(body)
	if err == nil {
		err = transfer.CheckStart(s, up.resume)
	}
	if err == nil && r.ContentLength >= 0 && r.ContentLength != transfer.SealedBodySize(m, s) {
		err = fmt.Errorf("%w: content length %d does not fit the start point", transfer.ErrSize, r.ContentLength)
	}
	if err != nil {
		failEarly(err)
		return
	}

	progress := rx.cfg.Ledger.Progress(tid, label, "in")
	var kept *Kept
	var out Output
	from := 0
	if up.keptID != "" && s == resumeAt && s != (transfer.Start{}) {
		// Continue what an earlier attempt kept.
		if kept, err = rx.cfg.Kept.Claim(up.keptID, *up.resume); err == nil {
			defer rx.cfg.Kept.Release(kept.ID)
			from = kept.Next
			out, err = rx.cfg.Kept.sink.Resume(tid, m, kept.Sink, progress)
		}
		if err != nil {
			rx.discardKept(up.keptID, "it could not be continued")
			rx.failIncoming(w, r, up, rx.stopErr(tid, fmt.Errorf("%w: %v", errKeptGone, err)))
			return
		}
	} else {
		if up.keptID != "" {
			// The sender starts over (its file changed): drop what was kept.
			rx.discardKept(up.keptID, "the sender started over")
		}
		if out, err = rx.cfg.Sink.Begin(tid, m, progress); err != nil {
			rx.failIncoming(w, r, up, rx.stopErr(tid, err))
			return
		}
	}
	res, err := transfer.ReceiveFrom(out, m, from, s, body)
	if err != nil {
		rx.endBroken(w, r, up, out, kept, err)
		return
	}
	if cb != nil {
		cb.disarm()
	}
	if kept != nil {
		rx.cfg.Kept.Remove(kept.ID)
		rx.publishKeptGone(kept.ID, "completed")
	}
	rx.cfg.Ledger.End(tid, nil)
	rx.Done(up, label, out.Location(), res)
	writeJSON(w, map[string]any{"ok": true})
}

// stopErr is err, or a cancel by the sender when it sent a cancel notice
// for transfer tid.
func (rx *Receiver) stopErr(tid string, err error) error {
	if rx.cancelledByPeer(tid) && !rx.cfg.Ledger.Transfers.Cancelled(tid) {
		return fmt.Errorf("%w by the sender", transfer.ErrCancelled)
	}
	return err
}

// endBroken ends a receive whose body failed after out began storing. A
// cancel (here or by the sender) removes everything; a network failure
// keeps what out stored when it can (a Keeper, with Config.Kept set);
// anything else removes everything.
func (rx *Receiver) endBroken(w http.ResponseWriter, r *http.Request, up *Upload, out Output, prev *Kept, err error) {
	tid := up.TransferID
	stopped := rx.cfg.Ledger.Transfers.Cancelled(tid) || rx.cancelledByPeer(tid)
	if keeper, ok := out.(Keeper); ok && !stopped && rx.cfg.Kept != nil && transfer.IsNetwork(err) {
		ko, kerr := keeper.Keep()
		if kerr == nil {
			k, kerr := rx.cfg.Kept.Keep(prev, up, ko, out.Location())
			if kerr == nil {
				err = fmt.Errorf("%w: %v; what arrived is kept for %s so the sender can resume", transfer.ErrInterrupted, err, resumeKeepFor)
				rx.failIncoming(w, r, up, err)
				rx.cfg.Ledger.Publish(Event{Type: "kept", Data: k.view()})
				return
			}
			// The record could not be written; Keep already removed what
			// was stored.
			if prev != nil {
				rx.cfg.Kept.Remove(prev.ID)
				rx.publishKeptGone(prev.ID, "it could not be kept")
			}
			rx.failIncoming(w, r, up, err)
			return
		}
	}
	out.Abort()
	if prev != nil {
		rx.cfg.Kept.Remove(prev.ID) // Abort removed what it kept
		rx.publishKeptGone(prev.ID, "the transfer failed")
	} else if !stopped {
		rx.markBroken(up, err)
	}
	rx.failIncoming(w, r, up, rx.stopErr(tid, err))
}

// maxBroken bounds Receiver.broken.
const maxBroken = 64

// acceptedKey names the transfers of manifest hash mh from sender key pub.
func acceptedKey(pub, mh string) string { return pub + "\n" + mh }

// markBroken records that accepted peer upload up broke with err before
// anything of it was kept: after a network failure (not a cancel), the
// sender's retry continues it without a new prompt for resume.KeepFor.
func (rx *Receiver) markBroken(up *Upload, err error) {
	if up.Drop || up.mh == "" || !transfer.IsNetwork(err) || errors.Is(err, transfer.ErrCancelled) {
		return
	}
	now := time.Now()
	rx.mu.Lock()
	defer rx.mu.Unlock()
	for k, until := range rx.broken {
		if now.After(until) {
			delete(rx.broken, k)
		}
	}
	if len(rx.broken) >= maxBroken {
		return
	}
	rx.broken[acceptedKey(up.SenderPub, up.mh)] = now.Add(resume.KeepFor)
}

// acceptedFor reports whether the person already accepted a transfer of
// manifest hash mh from sender key pub that has not run yet: its upload
// token is unused and still good, or it broke before anything was stored
// (markBroken). An offer of the same files from the same key continues it.
func (rx *Receiver) acceptedFor(pub, mh string) bool {
	now := time.Now()
	rx.mu.Lock()
	defer rx.mu.Unlock()
	if until, ok := rx.broken[acceptedKey(pub, mh)]; ok && !now.After(until) {
		return true
	}
	for _, u := range rx.uploads {
		if !u.Drop && u.SenderPub == pub && u.mh == mh && !now.After(u.expires) {
			return true
		}
	}
	return false
}

// forgetAccepted drops what acceptedFor found for pub and mh: a new offer
// of the same files took over.
func (rx *Receiver) forgetAccepted(pub, mh string) {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	delete(rx.broken, acceptedKey(pub, mh))
	for k, u := range rx.uploads {
		if !u.Drop && u.SenderPub == pub && u.mh == mh {
			delete(rx.uploads, k)
		}
	}
}

// resumeKeepFor is how long a kept transfer waits, for messages.
const resumeKeepFor = "24 hours"

// HandleCancelNotice handles POST /api/recv/cancel: the sender gave up a
// transfer it had sent an accepted offer for (protocol.CancelNotice, signed
// with the key of that offer). A transfer still being received stops and
// removes what it stored (history: cancelled by the sender); an unused
// upload token is dropped; a kept transfer is discarded with what it kept.
// Answers: 200, 400 for a malformed notice, 403 for a bad signature, 404
// when nothing of that transfer is left.
func (rx *Receiver) HandleCancelNotice(w http.ResponseWriter, r *http.Request) {
	if !rx.cfg.AllowOffer(RemoteIP(r)) {
		http.Error(w, "too many requests; slow down", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	decoded := LimitOfferRead(w, rx.cfg.OfferReadTimeout)
	var c protocol.CancelNotice
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil || !protocol.ValidID(c.TransferID) {
		http.Error(w, "bad cancel notice", http.StatusBadRequest)
		return
	}
	decoded()
	self := rx.cfg.Identity.Fingerprint()
	verify := func(pubB64 string) bool {
		pub, err := node.DecodePublicKey(pubB64)
		return err == nil && protocol.VerifyCancel(pub, self, &c) == nil
	}
	tid := c.TransferID

	rx.mu.Lock()
	act := rx.active[tid]
	var token string
	var pend *Upload
	for k, u := range rx.uploads {
		if u.TransferID == tid && !u.Drop {
			token, pend = k, u
			break
		}
	}
	rx.mu.Unlock()
	switch {
	case act != nil:
		if !verify(act.SenderPub) {
			http.Error(w, "bad cancel notice", http.StatusForbidden)
			return
		}
		rx.mu.Lock()
		rx.peerCancel[tid] = true
		rx.mu.Unlock()
		rx.cfg.Ledger.Transfers.Stop(tid)
	case pend != nil:
		if !verify(pend.SenderPub) {
			http.Error(w, "bad cancel notice", http.StatusForbidden)
			return
		}
		rx.mu.Lock()
		delete(rx.uploads, token)
		rx.mu.Unlock()
		if pend.keptID != "" {
			rx.discardKept(pend.keptID, "cancelled by the sender")
		}
	default:
		var k *Kept
		if rx.cfg.Kept != nil {
			k = rx.cfg.Kept.ByTransfer(tid)
		}
		if k == nil {
			http.Error(w, "no such transfer", http.StatusNotFound)
			return
		}
		if !verify(k.SenderPub) {
			http.Error(w, "bad cancel notice", http.StatusForbidden)
			return
		}
		rx.discardKept(k.ID, "cancelled by the sender")
	}
	writeJSON(w, map[string]any{"ok": true})
}

// --- kept transfers ---

// settleIdle is how long a receive of the same files must stand still
// before a new offer of them stops it; settleWait bounds the wait for it
// to end.
const (
	settleIdle = 3 * time.Second
	settleWait = 10 * time.Second
)

// activeFor returns the peer receive in progress from sender key pub of
// the manifest with hash mh, if any.
func (rx *Receiver) activeFor(pub, mh string) *Upload {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	for _, up := range rx.active {
		if up.SenderPub == pub && up.mh == mh {
			return up
		}
	}
	return nil
}

// settleActive lets a receive from sender key pub of the manifest with
// hash mh end before a new offer of the same files is answered, so that
// what it stored is kept and can be continued. A sender offers the same
// files again only after it lost its connection, but this side may not
// have seen the break yet: the receive gets settleIdle to end by itself,
// and is then stopped as a lost connection if it moved no byte meanwhile.
// One that still moves (the same files sent twice at once) is left alone.
func (rx *Receiver) settleActive(pub, mh string) {
	up := rx.activeFor(pub, mh)
	if up == nil {
		return
	}
	before := rx.sentOf(up.TransferID)
	idle := time.NewTimer(settleIdle)
	defer idle.Stop()
	select {
	case <-up.done:
		return
	case <-idle.C:
	}
	if rx.sentOf(up.TransferID) != before {
		return
	}
	rx.cfg.Ledger.Transfers.Stop(up.TransferID)
	wait := time.NewTimer(settleWait)
	defer wait.Stop()
	select {
	case <-up.done:
	case <-wait.C:
	}
}

// sentOf is how many bytes transfer id has stored so far.
func (rx *Receiver) sentOf(id string) int64 {
	for _, t := range rx.cfg.Ledger.Transfers.List() {
		if t.ID == id {
			return t.Sent
		}
	}
	return -1
}

// discardKept discards kept transfer id and reports it.
func (rx *Receiver) discardKept(id, reason string) bool {
	if rx.cfg.Kept == nil || !rx.cfg.Kept.Discard(id) {
		return false
	}
	rx.publishKeptGone(id, reason)
	return true
}

func (rx *Receiver) publishKeptGone(id, reason string) {
	rx.cfg.Ledger.Publish(Event{Type: "kept_gone", Data: map[string]any{"id": id, "reason": reason}})
}

// KeptList lists the transfers kept for a resume, most recent first.
func (rx *Receiver) KeptList() []KeptView {
	if rx.cfg.Kept == nil {
		return []KeptView{}
	}
	return rx.cfg.Kept.List()
}

// DiscardKept removes kept transfer id and what it kept (the person's
// Discard). It reports false when there is no such kept transfer, or a
// transfer is continuing it right now.
func (rx *Receiver) DiscardKept(id string) bool { return rx.discardKept(id, "discarded") }

// Sweep removes the kept transfers whose time is up, with what they kept.
// Run it at start-up and every resume.SweepEvery.
func (rx *Receiver) Sweep() {
	if rx.cfg.Kept == nil {
		return
	}
	for _, id := range rx.cfg.Kept.Sweep() {
		rx.publishKeptGone(id, "expired")
	}
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
