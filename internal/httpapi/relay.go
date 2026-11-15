package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/resume"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// Sends of files picked in this computer's browser page. Only the page holds
// the files (File objects, no paths), so the page drives every attempt and
// the server keeps what the attempts share in a relaySession, the way the
// sender's retry loop does for a send of local paths:
//
//	POST /api/relay/offer   {peer, files}: a new send; {resume, files}: an
//	                        interrupted one again, after the person picked
//	                        the same files; {transfer_id}: the next attempt
//	                        of a send that broke. Hello, signed offer, signed
//	                        reply (waiting out the retry delay first).
//	POST /api/relay/upload?transfer_id=&seq=
//	                        multipart, one part per file, from the file the
//	                        answer named on, in the page's order.
//	GET  /api/relay/result?transfer_id=&seq=
//	                        how upload seq ended, for a page whose upload
//	                        request the server stopped reading early.
//
// Every answer is a relayAnswer. When the receiver offers a resume point
// (entry i, offset n, sha256), the page uploads from the beginning of the
// file behind entry i; the server hashes its first n bytes as they arrive
// and forwards only the bytes after them when the hash matches, or answers
// "restart" (upload everything) when it does not. The page never chooses a
// start point. After a network failure the answer is "interrupted" with
// retryable set and the wait before the next offer (2, 5, 10, 20, 30 s,
// like every other send); after the last retry, or when the page goes away,
// the send is kept in the outbox with the list of picked files, so the page
// can resume it later once the person picks the same files again.

// relayAnswer is every answer of the relay routes.
type relayAnswer struct {
	// Result is accepted, restart (upload again from file From), sent,
	// interrupted (retry when Retryable), or how the send ended otherwise:
	// cancelled, declined, timeout or error.
	Result     string `json:"result"`
	TransferID string `json:"transfer_id"`
	// Seq is the number of the upload to make (accepted, restart).
	Seq int `json:"seq,omitempty"`
	// From is the index, in the page's file list, of the first file to
	// upload (accepted, restart).
	From int `json:"from"`
	// Resume says the receiver kept part of an earlier attempt (accepted).
	Resume bool `json:"resume,omitempty"`
	// Retryable: offer again with {transfer_id} (interrupted). Attempt of
	// Attempts is the retry that offer makes, after DelayMS.
	Retryable bool   `json:"retryable,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	Attempts  int    `json:"attempts,omitempty"`
	DelayMS   int64  `json:"delay_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Phases of a relay session.
const (
	relayOffering = iota // a request is making an offer
	relayUpload          // accepted: waiting for upload seq
	relaySending         // a request is uploading
	relayRetry           // broke: waiting for the next offer
	relayDone
)

// relayWaitDefault is how long a session waits for the page's next request
// (the upload after an accepted offer, the offer after a retry delay)
// before it counts the page as gone. Generous, since a hidden tab's timers
// may be slowed down.
const relayWaitDefault = 60 * time.Second

// relayKeep is how long an ended session still answers the page.
const relayKeep = 2 * time.Minute

// errPageGone is the cause of a send whose page went away.
var errPageGone = errors.New("the rocket page that started it was closed")

// relaySession is one send of browser-picked files across its attempts.
type relaySession struct {
	id       string
	name     string
	peerName string
	target   transfer.Target
	m        protocol.Manifest
	files    []transfer.BrowserFile // as the page listed them; file k is the k-th file entry of m
	ctx      context.Context        // ends on POST /api/cancel and quit
	resumes  string                 // the outbox record this send resumes (claimed), if any

	mu       sync.Mutex
	pin      string // the fingerprint every attempt must reach when target has none
	phase    int
	retries  int                // retries made so far
	last     *transfer.Accepted // the latest accepted attempt
	acc      *transfer.Accepted // the accepted attempt waiting for its upload
	noResume bool               // acc's resume point failed the prefix check
	seq      int
	from     int
	retryAt  time.Time
	timer    *time.Timer
	stopRead func() // stops reading the upload in progress (cancel)
	results  map[int]relayAnswer
	final    *relayAnswer
	changed  chan struct{} // closed and replaced when results or final change
}

// abort is the transfer's abort (see transfer.Manager.Start): a cancel
// also stops the read of the page's upload, where the body writer may be
// blocked.
func (rs *relaySession) abort() {
	rs.mu.Lock()
	stop := rs.stopRead
	rs.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// signal wakes GET /api/relay/result waiters. rs.mu must be held.
func (rs *relaySession) signal() {
	close(rs.changed)
	rs.changed = make(chan struct{})
}

// offerTarget is where the next attempt goes: the target, pinned to the
// receiver that accepted an earlier attempt (or the send resumed) when the
// target is a bare address.
func (rs *relaySession) offerTarget() transfer.Target {
	t := rs.target
	rs.mu.Lock()
	if t.FP == "" {
		t.FP = rs.pin
	}
	rs.mu.Unlock()
	return t
}

// fileOf is the index in rs.files of the file behind manifest entry e.
func (rs *relaySession) fileOf(e uint32) int {
	k := 0
	for i := 0; i < int(e) && i < len(rs.m.Entries); i++ {
		if rs.m.Entries[i].Mode&protocol.ModeDir == 0 {
			k++
		}
	}
	return k
}

func (s *Server) relayWaitFor() time.Duration {
	if s.relayWait > 0 {
		return s.relayWait
	}
	return relayWaitDefault
}

func (s *Server) renewAfterFor() time.Duration {
	if s.renewAfter > 0 {
		return s.renewAfter
	}
	return transfer.RenewAfter
}

func (s *Server) delays() []time.Duration {
	if s.retryDelays != nil {
		return s.retryDelays
	}
	return transfer.RetryDelays
}

// relay returns session id, if any.
func (s *Server) relay(id string) *relaySession {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	return s.relays[id]
}

// arm (re)starts the page watchdog for the idle phase rs is in. rs.mu must
// be held.
func (s *Server) arm(rs *relaySession, d time.Duration) {
	if rs.timer != nil {
		rs.timer.Stop()
	}
	phase, seq := rs.phase, rs.seq
	rs.timer = time.AfterFunc(d, func() {
		rs.mu.Lock()
		gone := rs.phase == phase && rs.seq == seq
		if gone {
			rs.phase = relayOffering // claimed by the watchdog
		}
		rs.mu.Unlock()
		if gone {
			s.relayEnd(rs, s.goneErr(rs, errPageGone))
		}
	})
}

// goneErr is how a send ends when its page cannot carry on (cause): kept for a
// resume when the receiver accepted an attempt, cancelled otherwise.
func (s *Server) goneErr(rs *relaySession, cause error) error {
	rs.mu.Lock()
	accepted := rs.last != nil
	rs.mu.Unlock()
	if accepted {
		return fmt.Errorf("%w: %v", transfer.ErrInterrupted, cause)
	}
	return fmt.Errorf("%w: %v", transfer.ErrCancelled, cause)
}

// handleRelayOffer starts a send of browser-picked files, starts one again
// after the person picked the same files (resume), or makes the next
// attempt of a send that broke. See the comment at the top of this file.
func (s *Server) handleRelayOffer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Peer       string                 `json:"peer"`
		Files      []transfer.BrowserFile `json:"files"`
		TransferID string                 `json:"transfer_id"`
		Resume     string                 `json:"resume"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, protocol.MaxOfferBytes)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.TransferID != "" {
		rs := s.relay(body.TransferID)
		if rs == nil {
			http.Error(w, "no send with that id", http.StatusNotFound)
			return
		}
		rs.mu.Lock()
		switch {
		case rs.phase == relayDone && rs.final != nil:
			res := *rs.final
			rs.mu.Unlock()
			writeJSON(w, res)
			return
		case rs.phase != relayRetry:
			rs.mu.Unlock()
			http.Error(w, "this send is not waiting to reconnect", http.StatusConflict)
			return
		}
		rs.phase = relayOffering
		rs.timer.Stop()
		wait := time.Until(rs.retryAt)
		rs.mu.Unlock()
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-rs.ctx.Done():
			case <-r.Context().Done():
			}
			t.Stop()
		}
		writeJSON(w, s.relayAttempt(r, rs))
		return
	}
	rs, code, err := s.newRelay(body.Peer, body.Resume, body.Files)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, s.relayAttempt(r, rs))
}

// newRelay builds and registers a session: to peer (a full fingerprint or
// host:port), or, with resumeID, the interrupted send of that ID again,
// whose files must be the ones picked then. It returns an HTTP status with
// an error.
func (s *Server) newRelay(peer, resumeID string, files []transfer.BrowserFile) (*relaySession, int, error) {
	var (
		target   transfer.Target
		peerName string
		rec      resume.Outgoing
	)
	if resumeID != "" {
		var ok bool
		if rec, ok = s.outbox.Claim(resumeID); !ok {
			return nil, http.StatusNotFound, errors.New("nothing to resume with that id")
		}
		claimed := true
		defer func() {
			if claimed {
				s.outbox.Release(resumeID)
			}
		}()
		if len(rec.Picked) == 0 {
			return nil, http.StatusConflict, errors.New("this send was not started from the rocket page; resume it with POST /api/resume")
		}
		if msg := pickedMismatch(rec.Picked, files); msg != "" {
			return nil, http.StatusConflict, errors.New(msg)
		}
		m, err := transfer.BrowserManifest(files, true)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		if mh, _ := m.Hash(); mh != rec.ManifestHash {
			return nil, http.StatusConflict, errors.New("these files do not make the same send as the interrupted one; pick the same files or folder again")
		}
		claimed = false // the session releases it when it ends
		target, peerName = transfer.Target{Addr: rec.Addr, FP: rec.PeerFP}, rec.PeerName
		if p, found := s.reg.Find(rec.PeerFP); rec.PeerFP != "" && found {
			target.Addr = p.Addr
		}
	} else {
		if !discovery.IsFingerprint(peer) && !transfer.IsHostPort(peer) {
			return nil, http.StatusBadRequest, errors.New("peer must be a full fingerprint or host:port")
		}
		var err error
		if target, peerName, err = s.resolveTarget(peer); err != nil {
			return nil, http.StatusNotFound, err
		}
	}
	m, err := transfer.BrowserManifest(files, true)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	rs := &relaySession{
		id: randToken(), name: transfer.Label(m), peerName: peerName, target: target, m: m, files: files,
		phase: relayOffering, results: map[int]relayAnswer{}, changed: make(chan struct{}),
	}
	if resumeID != "" {
		// The interrupted send stays in the outbox (hidden) until the
		// receiver accepts this one; a bare address must lead to the
		// receiver that accepted it.
		rs.resumes, rs.pin = resumeID, rec.ReceiverFP
	}
	rs.ctx = s.startOutgoing(context.Background(), rs.id, peerName, target, m, rs.abort)
	s.relayMu.Lock()
	s.relays[rs.id] = rs
	s.relayMu.Unlock()
	// A cancel while the session waits for the page ends it at once; while
	// a request runs, that request ends it.
	context.AfterFunc(rs.ctx, func() {
		rs.mu.Lock()
		idle := rs.phase == relayUpload || rs.phase == relayRetry
		if idle {
			rs.phase = relayOffering // claimed
		}
		rs.mu.Unlock()
		if idle {
			s.relayEnd(rs, fmt.Errorf("send stopped: %w", context.Canceled))
		}
	})
	return rs, 0, nil
}

// pickedMismatch compares the files picked again for a resume with the
// ones the interrupted send had, and says how they differ ("" when they
// are the same: names, sizes and order).
func pickedMismatch(want []resume.PickedFile, got []transfer.BrowserFile) string {
	const again = " Pick the same files or folder as before, or start a new send."
	if len(got) != len(want) {
		return fmt.Sprintf("You picked %s, but the interrupted send had %d.%s", fileCount(len(got)), len(want), again)
	}
	for k, w := range want {
		g := got[k]
		if g.Name != w.Name {
			return fmt.Sprintf("File %d is %q, but the interrupted send had %q there.%s", k+1, g.Name, w.Name, again)
		}
		if g.Size != w.Size {
			return fmt.Sprintf("%q has %d bytes now, but had %d when the send started, so it changed.%s", g.Name, g.Size, w.Size, again)
		}
	}
	return ""
}

// fileCount is "1 file" or "n files".
func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return strconv.Itoa(n) + " files"
}

// relayAttempt makes the offer of one attempt for a session the caller
// claimed (phase relayOffering) and says what the page does next.
func (s *Server) relayAttempt(r *http.Request, rs *relaySession) relayAnswer {
	if rs.ctx.Err() != nil {
		return s.relayEnd(rs, fmt.Errorf("send stopped: %w", context.Canceled))
	}
	// The offer waits for the receiver's decision; it is withdrawn when the
	// send is cancelled or the page goes away.
	octx, cancel := context.WithCancel(rs.ctx)
	defer cancel()
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()
	acc, err := transfer.Offer(octx, s.id, rs.offerTarget(), rs.m)
	if err == nil && (rs.ctx.Err() != nil || r.Context().Err() != nil) {
		// Accepted just as it was stopped: the receiver's side counts.
		err = context.Canceled
	}
	if acc != nil {
		s.relayAccepted(rs, acc)
	}
	if err != nil {
		if rs.ctx.Err() == nil && r.Context().Err() != nil {
			err = errPageGone
		}
		return s.relayFailed(rs, err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.acc, rs.noResume = acc, false
	rs.seq++
	rs.from = 0
	st, _, resumed := acc.Resume()
	if resumed {
		rs.from = rs.fileOf(st.Entry)
	}
	rs.phase = relayUpload
	s.arm(rs, s.relayWaitFor())
	return relayAnswer{Result: "accepted", TransferID: rs.id, Seq: rs.seq, From: rs.from, Resume: resumed}
}

// relayAccepted records accepted attempt acc: later attempts must reach
// the same receiver, and an interrupted send this one resumes leaves the
// outbox (this send's own record replaces it if it is interrupted too).
func (s *Server) relayAccepted(rs *relaySession, acc *transfer.Accepted) {
	rs.mu.Lock()
	first := rs.last == nil
	rs.last = acc
	if rs.pin == "" {
		rs.pin = acc.ReceiverFP()
	}
	rs.mu.Unlock()
	if first && rs.resumes != "" {
		_ = s.outbox.Remove(rs.resumes)
	}
}

// relayFailed decides what an attempt that failed with err leads to, like
// the sender's retry loop (transfer.SendManifestOpts): the next retry
// after a network failure of a send the receiver accepted, or the end.
func (s *Server) relayFailed(rs *relaySession, err error) relayAnswer {
	rs.mu.Lock()
	accepted := rs.last != nil
	rs.mu.Unlock()
	delays := s.delays()
	switch {
	case rs.ctx.Err() != nil:
		return s.relayEnd(rs, fmt.Errorf("send stopped: %w", context.Canceled))
	case errors.Is(err, transfer.ErrSourceLost), errors.Is(err, errPageGone):
		return s.relayEnd(rs, s.goneErr(rs, err))
	case !accepted || !transfer.IsNetwork(err):
		return s.relayEnd(rs, err)
	}
	rs.mu.Lock()
	if rs.retries == len(delays) {
		n := rs.retries
		rs.mu.Unlock()
		return s.relayEnd(rs, fmt.Errorf("%w: %v (gave up after %d retries)", transfer.ErrInterrupted, err, n))
	}
	d := delays[rs.retries]
	rs.retries++
	n := rs.retries
	rs.acc = nil
	rs.phase = relayRetry
	rs.retryAt = time.Now().Add(d)
	s.arm(rs, d+s.relayWaitFor())
	rs.mu.Unlock()
	s.ledger.Retrying(rs.id, rs.name, n, len(delays), d, err)
	return relayAnswer{
		Result: string(history.ResultInterrupted), TransferID: rs.id, Retryable: true,
		Attempt: n, Attempts: len(delays), DelayMS: d.Milliseconds(), Error: err.Error(),
	}
}

// relayEnd ends a session the caller claimed: history, events and, for a
// send interrupted for good, the outbox record its Resume needs. A
// cancelled send tells the receiver to drop what it kept.
func (s *Server) relayEnd(rs *relaySession, err error) relayAnswer {
	rs.mu.Lock()
	if rs.phase == relayDone {
		res := relayAnswer{Result: string(history.ResultError), TransferID: rs.id, Error: "this send has ended"}
		if rs.final != nil {
			res = *rs.final
		}
		rs.mu.Unlock()
		return res
	}
	rs.phase = relayDone
	if rs.timer != nil {
		rs.timer.Stop()
	}
	last := rs.last
	rs.mu.Unlock()
	if rs.resumes != "" {
		s.outbox.Release(rs.resumes) // listed again, if still there, before the end is published
	}

	cancelled := s.xfers.Cancelled(rs.id)
	if err != nil && (cancelled || rs.ctx.Err() != nil) && last != nil {
		last.Abandon()
	}
	mh, _ := rs.m.Hash()
	if err == nil {
		s.outbox.Supersede(mh, rs.target.FP, rs.target.Addr)
	}
	if errors.Is(err, transfer.ErrInterrupted) && !cancelled {
		rec := resume.Outgoing{
			ID: rs.id, Name: rs.name, PeerName: rs.peerName, PeerFP: rs.target.FP, Addr: rs.target.Addr,
			Files: transfer.Files(rs.m), Bytes: rs.m.Total, ManifestHash: mh, Error: err.Error(),
		}
		for _, f := range rs.files {
			rec.Picked = append(rec.Picked, resume.PickedFile{Name: f.Name, Size: f.Size})
		}
		if last != nil {
			rec.ReceiverFP, rec.RemoteID = last.ReceiverFP(), last.RemoteID()
		}
		if perr := s.outbox.Put(rec); perr != nil {
			s.ledger.Error(rs.id, rs.name, fmt.Errorf("could not keep the send for a resume: %w", perr))
		}
	}
	outcome, err := s.ledger.End(rs.id, err)
	res := relayAnswer{Result: string(outcome), TransferID: rs.id}
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Result = "sent"
		s.hub.publish(Event{Type: "sent", Data: map[string]any{
			"transfer_id": rs.id, "name": rs.name, "to": rs.target.Addr, "files": transfer.Files(rs.m), "bytes": rs.m.Total,
		}})
	}
	rs.mu.Lock()
	rs.final = &res
	rs.signal()
	rs.mu.Unlock()
	time.AfterFunc(relayKeep, func() {
		s.relayMu.Lock()
		delete(s.relays, rs.id)
		s.relayMu.Unlock()
	})
	return res
}

// handleRelayUpload takes upload seq of a session: the page's files from
// the one the accepted answer named on, which go on to the peer as the
// encrypted body. See the comment at the top of this file.
func (s *Server) handleRelayUpload(w http.ResponseWriter, r *http.Request) {
	rs, seq, ok := s.relayFromQuery(w, r)
	if !ok {
		return
	}
	rs.mu.Lock()
	switch {
	case rs.phase == relayDone && rs.final != nil:
		res := *rs.final
		rs.mu.Unlock()
		writeJSON(w, res)
		return
	case rs.phase != relayUpload || rs.seq != seq:
		rs.mu.Unlock()
		http.Error(w, "this upload is not expected now", http.StatusConflict)
		return
	}
	rs.phase = relaySending
	rs.timer.Stop()
	rc := http.NewResponseController(w)
	rs.stopRead = func() { _ = rc.SetReadDeadline(time.Now()) }
	acc, noResume, from := rs.acc, rs.noResume, rs.from
	rs.mu.Unlock()

	res := s.relayBody(r, rs, acc, noResume, from)

	rs.mu.Lock()
	rs.stopRead = nil
	rs.results[seq] = res
	rs.signal()
	rs.mu.Unlock()
	writeJSON(w, res)
}

// relayBody uploads the body of accepted attempt acc from the page's
// multipart upload, which begins with file from.
func (s *Server) relayBody(r *http.Request, rs *relaySession, acc *transfer.Accepted, noResume bool, from int) relayAnswer {
	if rs.ctx.Err() != nil {
		return s.relayEnd(rs, fmt.Errorf("send stopped: %w", context.Canceled))
	}
	mr, err := r.MultipartReader()
	if err != nil {
		acc.Abandon()
		return s.relayEnd(rs, fmt.Errorf("bad upload: %w", err))
	}
	parts := &relayParts{mr: mr, files: rs.files, k: from}
	start := transfer.Start{}
	var first io.ReadCloser // the file at the resume point, its prefix read
	if st, sha, ok := acc.Resume(); ok && !noResume {
		// The page uploads the whole file at the resume point. Its first
		// st.Offset bytes must be what the receiver holds; only the bytes
		// after them go to the peer.
		part, err := parts.next()
		if err == nil {
			h := sha256.New()
			var n int64
			n, err = io.CopyN(h, part, int64(st.Offset))
			if err == nil && hex.EncodeToString(h.Sum(nil)) != sha {
				return s.relayRestart(rs)
			}
			if errors.Is(err, io.EOF) {
				err = fmt.Errorf("%s: %w: it has only %d bytes", rs.files[from].Name, transfer.ErrSize, n)
			}
		}
		if err != nil {
			if !errors.Is(err, transfer.ErrSourceLost) {
				acc.Abandon()
			}
			return s.relayFailed(rs, err)
		}
		if acc.Age() > s.renewAfterFor() {
			// The upload token may have expired meanwhile: offer again.
			// The receiver still keeps the same part, so it accepts
			// without asking and names the same point.
			octx, cancel := context.WithCancel(rs.ctx)
			stop := context.AfterFunc(r.Context(), cancel)
			nacc, err := acc.Renew(octx)
			stop()
			cancel()
			if nacc != nil {
				s.relayAccepted(rs, nacc)
			}
			if err != nil {
				return s.relayFailed(rs, err)
			}
			acc = nacc
			if st2, sha2, ok := acc.Resume(); !ok || st2 != st || sha2 != sha {
				// Not the same point any more: everything goes again.
				rs.mu.Lock()
				rs.acc = acc
				rs.mu.Unlock()
				return s.relayRestart(rs)
			}
		}
		start, first = st, &skipped{r: part, n: int64(st.Offset)}
	}
	open := func(i int, _ protocol.Entry) (io.ReadCloser, error) {
		if first != nil && i == int(start.Entry) {
			return first, nil
		}
		return parts.next()
	}
	err = acc.Upload(rs.ctx, start, open, s.progressFn(rs.id, rs.name, "out"), true)
	if err == nil {
		return s.relayEnd(rs, nil)
	}
	return s.relayFailed(rs, err)
}

// relayRestart answers an upload whose prefix did not match the receiver's
// resume point (the file changed): the same accepted attempt takes a body
// from the beginning, so the page uploads every file.
func (s *Server) relayRestart(rs *relaySession) relayAnswer {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.noResume = true
	rs.from = 0
	rs.seq++
	rs.phase = relayUpload
	s.arm(rs, s.relayWaitFor())
	return relayAnswer{Result: "restart", TransferID: rs.id, Seq: rs.seq, From: 0}
}

// handleRelayResult answers how upload seq of a session ended, once it has:
// GET /api/relay/result?transfer_id=&seq=. It waits up to 30 seconds and
// then answers {"result":"pending"}; ask again.
func (s *Server) handleRelayResult(w http.ResponseWriter, r *http.Request) {
	rs, seq, ok := s.relayFromQuery(w, r)
	if !ok {
		return
	}
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		rs.mu.Lock()
		res, have := rs.results[seq]
		if !have && rs.final != nil {
			res, have = *rs.final, true
		}
		ch := rs.changed
		rs.mu.Unlock()
		if have {
			writeJSON(w, res)
			return
		}
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		case <-t.C:
			writeJSON(w, relayAnswer{Result: "pending", TransferID: rs.id, Seq: seq})
			return
		}
	}
}

// relayFromQuery finds the session and upload number a request names
// (?transfer_id=&seq=).
func (s *Server) relayFromQuery(w http.ResponseWriter, r *http.Request) (*relaySession, int, bool) {
	q := r.URL.Query()
	seq, err := strconv.Atoi(q.Get("seq"))
	if err != nil || seq < 1 || !protocol.ValidID(q.Get("transfer_id")) {
		http.Error(w, "need transfer_id and seq", http.StatusBadRequest)
		return nil, 0, false
	}
	rs := s.relay(q.Get("transfer_id"))
	if rs == nil {
		http.Error(w, "no send with that id", http.StatusNotFound)
		return nil, 0, false
	}
	return rs, seq, true
}

// relayParts hands out the file parts of a page's upload in order, checked
// against the page's list.
type relayParts struct {
	mr    *multipart.Reader
	files []transfer.BrowserFile
	k     int // index in files of the next part
}

// next returns the next file part. A body that stops arriving (the page
// went away) is a transfer.ErrSourceLost, also while it is read.
func (p *relayParts) next() (io.ReadCloser, error) {
	part, err := p.mr.NextPart()
	switch {
	case errors.Is(err, io.EOF):
		return nil, fmt.Errorf("expected %d files, got %d", len(p.files), p.k)
	case err != nil:
		return nil, fmt.Errorf("%w from the rocket page: %v", transfer.ErrSourceLost, err)
	case p.k >= len(p.files):
		return nil, fmt.Errorf("more files than the %d listed", len(p.files))
	}
	want := transfer.BaseName(p.files[p.k].Name)
	if part.FileName() == "" || transfer.BaseName(part.FileName()) != want {
		return nil, fmt.Errorf("file %d is not %q", p.k+1, want)
	}
	p.k++
	// Closing a multipart part drains the rest of it; the next NextPart
	// skips what is left anyway.
	return io.NopCloser(sourceReader{part}), nil
}

// sourceReader marks a failed read of the page's upload as ErrSourceLost
// (the inner error is kept as text only, so it does not look like a
// network failure of the peer connection).
type sourceReader struct{ r io.Reader }

func (s sourceReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if err != nil && err != io.EOF {
		err = fmt.Errorf("%w from the rocket page: %v", transfer.ErrSourceLost, err)
	}
	return n, err
}

// skipped is a file part whose first n bytes were already read (the prefix
// checked against the resume point). It seeks to exactly n, so the body
// writer skips them without reading.
type skipped struct {
	r io.Reader
	n int64
}

func (s *skipped) Read(b []byte) (int, error) { return s.r.Read(b) }
func (s *skipped) Close() error               { return nil }
func (s *skipped) Seek(off int64, whence int) (int64, error) {
	if whence != io.SeekStart || off != s.n {
		return 0, errors.New("relay: cannot seek a browser upload")
	}
	return off, nil
}
