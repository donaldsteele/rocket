// Package transfer carries files from one rocket host to another with a
// two-phase push (protocol v2): the sender fetches the receiver's hello,
// sends a signed offer (metadata and an ephemeral key), the receiver accepts
// (trust-on-first-use prompt) with a signed reply carrying its own ephemeral
// key, then the sender streams the bytes with a one-shot token as an
// encrypted, authenticated securestream (see body.go). Nothing is buffered
// whole in memory or staged; the body streams straight to disk (ReceiveBody
// with an Extractor or another EntryWriter). The browser drop page keeps its
// own plain multipart upload (internal/httpapi).
package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/protocol/securestream"
)

// Target is the peer to send to. Addr is host:port of its HTTP API. FP, when
// known (from discovery), must match the fingerprint the peer answers with.
type Target struct {
	Addr string
	FP   string
}

// ErrDeclined is wrapped by the Send functions when the receiver says no.
var ErrDeclined = errors.New("receiver declined")

// helloTimeout bounds the hello round trip; the offer long-poll and the
// upload are governed only by the caller's context.
const helloTimeout = 10 * time.Second

// OpenFunc opens the content of manifest entry i, which is never a folder.
// The sender calls it once per file, in manifest order, and closes what it
// returns. It must yield at least e.Size bytes; only e.Size bytes are read.
type OpenFunc func(i int, e protocol.Entry) (io.ReadCloser, error)

// Send offers a local file or folder to the target, waits for acceptance,
// then streams it. progress may be nil.
func Send(ctx context.Context, id *node.Identity, t Target, path string, progress ProgressFn) error {
	return SendPaths(ctx, id, t, []string{path}, progress)
}

// SendPaths offers local files and folders in one transfer (see PlanPaths).
func SendPaths(ctx context.Context, id *node.Identity, t Target, paths []string, progress ProgressFn) error {
	p, err := PlanPaths(paths)
	if err != nil {
		return err
	}
	return SendPlan(ctx, id, t, p, progress)
}

// SendPlan sends a plan built by PlanPaths.
func SendPlan(ctx context.Context, id *node.Identity, t Target, p *Plan, progress ProgressFn) error {
	return SendPlanOpts(ctx, id, t, p, progress, SendOptions{})
}

// SendPlanOpts is SendPlan with options (see SendManifestOpts). A plan's
// files can be opened again, so it may retry and resume.
func SendPlanOpts(ctx context.Context, id *node.Identity, t Target, p *Plan, progress ProgressFn, opts SendOptions) error {
	return SendManifestOpts(ctx, id, t, p.Manifest, p.Open, progress, opts)
}

// Open opens the local file behind manifest entry i (an OpenFunc).
func (p *Plan) Open(i int, _ protocol.Entry) (io.ReadCloser, error) {
	return os.Open(p.Sources[i])
}

// SendReader offers one file of exactly size bytes named name, waits for
// acceptance, then streams src to the peer. Used to relay a browser upload
// straight through to a peer without staging it on disk.
func SendReader(ctx context.Context, id *node.Identity, t Target, name string, size int64, src io.Reader, progress ProgressFn) error {
	if size < 0 {
		return fmt.Errorf("unknown size for %s", name)
	}
	m := protocol.FileManifest(BaseName(name), size, 0o644)
	return SendManifest(ctx, id, t, m, func(int, protocol.Entry) (io.ReadCloser, error) {
		return io.NopCloser(src), nil
	}, progress)
}

// SendManifest is the core sender: hello (the peer must speak protocol 2),
// a signed offer for m with a fresh ephemeral key, the receiver's signed
// reply (verified against the key from its hello), then the body: the
// content (open supplies each file) after a start header, as a securestream
// under the key both sides derive. Nothing is staged. It makes one attempt
// and ignores resume points (see SendManifestOpts).
func SendManifest(ctx context.Context, id *node.Identity, t Target, m protocol.Manifest, open OpenFunc, progress ProgressFn) error {
	return SendManifestOpts(ctx, id, t, m, open, progress, SendOptions{})
}

// RetryDelays are the waits before each of the sender's five retries after
// an accepted transfer broke for a network reason.
var RetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second}

// StallTimeout is how long an upload may move no byte at all before its
// connection counts as lost.
const StallTimeout = 60 * time.Second

// stallCheck is how often the stall watchdog looks.
const stallCheck = 2 * time.Second

// cancelNoticeTimeout bounds a cancel notice round trip.
const cancelNoticeTimeout = 3 * time.Second

// RenewAfter is how long after the receiver's reply a sender trusts its
// one-shot upload token (valid for protocol.TokenTTL) to be good. A resume
// check that reads a long prefix can take longer; the attempt then offers
// the same files again (Accepted.Renew) before it uploads.
const RenewAfter = protocol.TokenTTL / 2

// SendOptions tunes SendManifestOpts.
type SendOptions struct {
	// Retry makes the send resumable. open may then be called more than
	// once for a file: to check a resume point and on every attempt. A
	// resume point the receiver offers is used when the SHA-256 of the
	// local file's prefix matches it (otherwise the body starts over), and
	// when the transfer breaks for a network reason after the receiver
	// accepted it, it is offered again (new offer, new keys) after each of
	// Delays. When the last retry fails too, the error wraps
	// ErrInterrupted. Without Retry a send makes one attempt and starts at
	// the beginning.
	Retry bool
	// Delays are the waits before each retry; nil means RetryDelays.
	Delays []time.Duration
	// OnRetry is told before each wait: retry n of max, after err.
	OnRetry func(n, max int, delay time.Duration, err error)
	// OnAccepted is told of every accepted attempt: the receiver's
	// fingerprint and its transfer ID, which a later cancel notice names.
	OnAccepted func(receiverFP, remoteID string)
	// RenewAfter overrides the package's RenewAfter when non-zero (tests);
	// a negative value renews every time (see Accepted.Stale).
	RenewAfter time.Duration
}

// remote names the receiver's side of an accepted attempt.
type remote struct{ fp, id string }

// SendManifestOpts is SendManifest with options. However it ends, an
// accepted transfer is never left half-done on the receiver except after a
// network failure it can resume: a cancel here (ctx), a failure reading
// the local files, and a network failure of a send without Retry are
// followed by a signed cancel notice (NotifyCancel), sent before the body
// is cut off where possible, so the receiver removes what it stored.
func SendManifestOpts(ctx context.Context, id *node.Identity, t Target, m protocol.Manifest, open OpenFunc, progress ProgressFn, opts SendOptions) error {
	if err := m.Validate(); err != nil {
		return err
	}
	delays := opts.Delays
	if delays == nil {
		delays = RetryDelays
	}
	var last remote // the latest accepted attempt
	for n := 0; ; n++ {
		r, err := sendAttempt(ctx, id, t, m, open, progress, opts)
		if r.id != "" {
			last = r
			// Every later attempt must reach the receiver that accepted
			// this one, also when t named only an address: whoever answers
			// there after a broken connection may be someone else.
			if t.FP == "" {
				t.FP = r.fp
			}
		} else if ctx.Err() != nil {
			// Stopped before this attempt was accepted: the receiver may
			// still keep what an earlier attempt stored.
			notifyCancel(id, t.Addr, last)
		}
		if err == nil {
			return nil
		}
		if !opts.Retry || last.id == "" || ctx.Err() != nil || !IsNetwork(err) {
			return err
		}
		if n == len(delays) {
			return fmt.Errorf("%w: %v (gave up after %d retries)", ErrInterrupted, err, len(delays))
		}
		if opts.OnRetry != nil {
			opts.OnRetry(n+1, len(delays), delays[n], err)
		}
		wait := time.NewTimer(delays[n])
		select {
		case <-ctx.Done():
			wait.Stop()
			notifyCancel(id, t.Addr, last)
			return fmt.Errorf("send stopped: %w", ctx.Err())
		case <-wait.C:
		}
	}
}

// errStalled is the cause of an upload stopped by the stall watchdog.
var errStalled = errors.New("stalled")

// ErrSourceLost is wrapped by the reader an OpenFunc returns when its
// content stopped arriving in a way a later attempt can continue: a browser
// page that uploads the files to this host went away. Upload then cuts the
// body off like a lost connection instead of telling the receiver to drop
// what it stored, so the receiver keeps it (see Accepted.Upload).
var ErrSourceLost = errors.New("the files stopped arriving")

// sendAttempt makes one attempt: hello, offer, reply, body. r names the
// receiver's side once the offer was accepted.
func sendAttempt(ctx context.Context, id *node.Identity, t Target, m protocol.Manifest, open OpenFunc, progress ProgressFn, opts SendOptions) (r remote, err error) {
	a, err := offerAttempt(ctx, id, t, m)
	if a != nil {
		r = a.r
		if opts.OnAccepted != nil {
			opts.OnAccepted(r.fp, r.id)
		}
	}
	if err != nil {
		return r, err
	}
	start := Start{}
	if opts.Retry && a.resume != nil {
		start = resumeStart(m, a.resume, open)
		if a.Stale(opts.RenewAfter) {
			// The prefix check took so long that the upload token may have
			// expired: offer again. The receiver still keeps the same part,
			// so it accepts without asking and names the same point.
			checked := *a.resume
			if a, err = a.Renew(ctx); a != nil {
				r = a.r
				if opts.OnAccepted != nil {
					opts.OnAccepted(r.fp, r.id)
				}
			}
			if err != nil {
				return r, err
			}
			if a.resume == nil || *a.resume != checked {
				start = Start{}
			}
		}
	}
	return r, a.Upload(ctx, start, open, progress, opts.Retry)
}

// Accepted is an offer the receiver accepted whose body has not been sent
// yet: the first half of one attempt (Offer). Its body goes out once, with
// Upload. A sender that splits its attempts this way (the desktop's
// browser page, which holds the files) makes its own retries.
type Accepted struct {
	id     *node.Identity
	addr   string
	m      protocol.Manifest
	r      remote
	key    []byte
	token  string
	resume *protocol.ResumePoint
	at     time.Time // when the reply arrived
	once   sync.Once
	used   atomic.Bool
}

// Offer makes the first half of an attempt to send m to t: hello (the peer
// must speak protocol 2), a signed offer with a fresh ephemeral key, and the
// receiver's signed reply, verified against the key from its hello. A
// decline wraps ErrDeclined and no answer ErrTimedOut; network failures are
// IsNetwork. m must be valid.
func Offer(ctx context.Context, id *node.Identity, t Target, m protocol.Manifest) (*Accepted, error) {
	a, err := offerAttempt(ctx, id, t, m)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// offerAttempt is Offer, except that it returns the accepted offer also
// when its body key cannot be derived (the receiver has then already been
// sent a cancel notice).
func offerAttempt(ctx context.Context, id *node.Identity, t Target, m protocol.Manifest) (*Accepted, error) {
	hello, err := Hello(ctx, t.Addr)
	if err != nil {
		return nil, err
	}
	pub, err := protocol.CheckHello(hello, t.FP)
	if errors.Is(err, protocol.ErrProto) {
		return nil, err // names the peer and says what to update
	}
	if err != nil {
		return nil, fmt.Errorf("peer %s: %w", t.Addr, err)
	}
	offer, eph, err := protocol.NewOffer(id, id.Name, hello.FP, m, time.Now())
	if err != nil {
		return nil, err
	}
	offerSB, err := protocol.SigningBytes(offer)
	if err != nil {
		return nil, err
	}
	reply, err := postOffer(ctx, t.Addr, offer)
	if err != nil {
		return nil, err
	}
	if err := protocol.VerifyReply(pub, offerSB, &reply); err != nil {
		return nil, fmt.Errorf("peer %s: %w", t.Addr, err)
	}
	if !reply.Accepted {
		if reply.Reason == protocol.ReasonTimedOut {
			return nil, fmt.Errorf("%w (%s)", ErrTimedOut, t.Addr)
		}
		if reply.Reason == "" {
			reply.Reason = "declined"
		}
		return nil, fmt.Errorf("%w: %s", ErrDeclined, reply.Reason)
	}
	a := &Accepted{
		id: id, addr: t.Addr, m: m, r: remote{fp: hello.FP, id: reply.TransferID},
		token: reply.Token, resume: reply.Resume, at: time.Now(),
	}
	key, err := protocol.BodyKey(eph, reply.Eph, offer, reply.TransferID)
	if err != nil {
		a.Abandon()
		return a, fmt.Errorf("peer %s: %w", t.Addr, err)
	}
	a.key = key
	return a, nil
}

// ReceiverFP is the fingerprint of the receiver that accepted.
func (a *Accepted) ReceiverFP() string { return a.r.fp }

// RemoteID is the receiver's transfer ID, which a cancel notice names.
func (a *Accepted) RemoteID() string { return a.r.id }

// Age is the time since the receiver accepted. Its upload token is good for
// protocol.TokenTTL from then; past RenewAfter, Renew before Upload.
func (a *Accepted) Age() time.Duration { return time.Since(a.at) }

// Stale reports whether the upload token should be renewed before Upload:
// Age is past RenewAfter, or past override when override is positive. A
// negative override always renews (tests: the monotonic clock on Windows
// can be too coarse for a tiny positive threshold to have passed).
func (a *Accepted) Stale(override time.Duration) bool {
	switch {
	case override < 0:
		return true
	case override > 0:
		return a.Age() > override
	}
	return a.Age() > RenewAfter
}

// Renew offers the same files to the same receiver again (pinned to the
// fingerprint that accepted) for an accepted offer whose body has not gone
// out and whose upload token may have expired, after a long resume check.
// The receiver accepts an offer that continues one it accepted without
// asking, with the same resume point while it keeps the same part. a can
// no longer be uploaded; the result is used instead (it may be non-nil
// with an error, as from Offer's internals: see offerAttempt).
func (a *Accepted) Renew(ctx context.Context) (*Accepted, error) {
	a.used.Store(true)
	return offerAttempt(ctx, a.id, Target{Addr: a.addr, FP: a.r.fp}, a.m)
}

// Resume returns the resume point the receiver offered, and the SHA-256
// (lowercase hex) of the bytes of entry s.Entry before s.Offset as the
// receiver holds them, when there is one that fits the manifest and is not
// the beginning. The body may then start at s if the sender's own prefix
// has that hash, and must otherwise start at the beginning.
func (a *Accepted) Resume() (s Start, sha256 string, ok bool) {
	if a.resume == nil {
		return Start{}, "", false
	}
	s = Start{Entry: a.resume.Entry, Offset: a.resume.Offset}
	if s == (Start{}) || !validStart(a.m, s) {
		return Start{}, "", false
	}
	return s, a.resume.SHA256, true
}

// Abandon tells the receiver, once and on a best-effort basis, that this
// sender gave the transfer up (a signed cancel notice), so that it removes
// what it stored or kept of it.
func (a *Accepted) Abandon() {
	a.once.Do(func() { notifyCancel(a.id, a.addr, a.r) })
}

// Upload sends the body of the accepted offer beginning at start, which
// must be the beginning or exactly the point Resume returned (the caller
// has checked the prefix hash); open supplies the content from there on.
// However it ends, the receiver is left nothing half-done except after a
// network failure with keep set, which it keeps for a resume: a cancel
// (ctx), a failure reading the content, and a network failure without keep
// are followed by a cancel notice, sent before the body is cut off where
// possible. A content error wrapping ErrSourceLost counts as a network
// failure here (the body is cut off, no notice) when keep is set; Upload
// returns it as it is. progress may be nil. Upload may be called once.
func (a *Accepted) Upload(ctx context.Context, start Start, open OpenFunc, progress ProgressFn, keep bool) error {
	if a.used.Swap(true) {
		return errors.New("this accepted offer was already used")
	}
	if a.key == nil {
		return errors.New("no body key for this offer")
	}
	if start != (Start{}) {
		if s, _, ok := a.Resume(); !ok || s != start {
			a.Abandon()
			return fmt.Errorf("%w: entry %d offset %d", ErrStart, start.Entry, start.Offset)
		}
	}
	m, abandon := a.m, a.Abandon
	sent, done := Before(m, start)
	tr := newTracker(progress, m.Total, Files(m)).from(sent, done)

	// The upload has its own context so that a cancel here first tells the
	// receiver (the cancel notice) and only then cuts the body off: the
	// receiver then knows not to keep what it stored.
	upCtx, upCancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer upCancel(nil)
	stopWatch := context.AfterFunc(ctx, func() {
		abandon()
		upCancel(context.Canceled)
	})
	defer stopWatch()

	// The body is produced by a goroutine into a pipe. Only that goroutine
	// touches the sources (open, read, close); the HTTP transport only reads
	// the pipe, which is safe even when it keeps reading after Do has
	// returned.
	pr, pw := io.Pipe()
	defer pr.Close()
	var lastMove atomic.Int64 // unix nanoseconds of the last byte taken by the transport; 0 once the body is written
	lastMove.Store(time.Now().UnixNano())
	writeDone := make(chan error, 1)
	go func() {
		err := writeSealed(stallWriter{w: pw, last: &lastMove}, a.key, m, start, open, tr)
		if err != nil && !errors.Is(err, io.ErrClosedPipe) && !(keep && errors.Is(err, ErrSourceLost)) {
			abandon() // a local failure: tell the receiver before the body is cut off
		}
		lastMove.Store(0)
		pw.CloseWithError(err)
		writeDone <- err
	}()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		tick := time.NewTicker(stallCheck)
		defer tick.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-tick.C:
				if t := lastMove.Load(); t != 0 && time.Since(time.Unix(0, t)) > StallTimeout {
					upCancel(errStalled)
					return
				}
			}
		}
	}()

	u := fmt.Sprintf("http://%s/api/recv?token=%s", a.addr, url.QueryEscape(a.token))
	req, err := http.NewRequestWithContext(upCtx, http.MethodPost, u, pr)
	if err != nil {
		pr.Close()
		<-writeDone
		abandon()
		return err
	}
	req.ContentLength = SealedBodySize(m, start)
	req.Header.Set("Content-Type", BodyType)
	resp, err := httpClient.Do(req)
	pr.Close() // unblocks the writer if the request ended early
	werr := <-writeDone
	if errors.Is(werr, io.ErrClosedPipe) {
		werr = nil // the request ended first; err or the status says why
	}
	if err == nil {
		defer resp.Body.Close()
	}
	switch {
	case ctx.Err() != nil && (err != nil || werr != nil || resp.StatusCode != http.StatusOK):
		return fmt.Errorf("send stopped: %w", ctx.Err())
	case werr != nil:
		// A failure while reading local files is the real cause; report it
		// rather than the aborted request it caused. This comes before the
		// 410 check: the cancel notice the writer sent (abandon) makes the
		// receiver answer 410, which would otherwise hide the cause. werr
		// is never set by a receiver-side end, which closes the pipe.
		return werr
	case err == nil && resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w by the receiver", ErrCancelled)
	case err != nil && context.Cause(upCtx) == errStalled:
		err = fmt.Errorf("%w: nothing moved for %s", ErrConnLost, StallTimeout)
		if !keep {
			abandon()
		}
		return err
	case err != nil:
		if !keep && IsNetwork(err) {
			abandon()
		}
		return err
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("upload failed: %s: %s", resp.Status, readMsg(resp.Body))
	}
	tr.finish()
	return nil
}

// stallWriter records when the transport last took bytes.
type stallWriter struct {
	w    io.Writer
	last *atomic.Int64
}

func (s stallWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if n > 0 {
		s.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// resumeStart returns the receiver's resume point rp when it fits m and
// the SHA-256 of the local file's first rp.Offset bytes matches rp.SHA256,
// else the beginning. Files before the point are trusted as they are: their
// sizes match the manifest, and the receiver authenticated their content
// when it got them.
func resumeStart(m protocol.Manifest, rp *protocol.ResumePoint, open OpenFunc) Start {
	s := Start{Entry: rp.Entry, Offset: rp.Offset}
	if s == (Start{}) || !validStart(m, s) {
		return Start{}
	}
	i := int(s.Entry)
	rc, err := open(i, m.Entries[i])
	if err != nil {
		return Start{}
	}
	defer rc.Close()
	h := sha256.New()
	if n, err := io.CopyN(h, rc, int64(s.Offset)); err != nil || n != int64(s.Offset) {
		return Start{}
	}
	if hex.EncodeToString(h.Sum(nil)) != rp.SHA256 {
		return Start{}
	}
	return s
}

// notifyCancel sends a cancel notice for the accepted attempt r, if any,
// ignoring the outcome (best effort).
func notifyCancel(id *node.Identity, addr string, r remote) {
	if r.id != "" {
		_ = NotifyCancel(context.Background(), id, addr, r.fp, r.id)
	}
}

// NotifyCancel tells the receiver at addr (fingerprint receiverFP) that
// this sender gave up its transfer remoteID: POST /api/recv/cancel with a
// signed protocol.CancelNotice. The receiver stops the transfer if it is
// still running and removes whatever it stored or kept of it. It takes at
// most a few seconds.
func NotifyCancel(ctx context.Context, id protocol.Signer, addr, receiverFP, remoteID string) error {
	ctx, cancel := context.WithTimeout(ctx, cancelNoticeTimeout)
	defer cancel()
	b, err := json.Marshal(protocol.SignCancel(id, receiverFP, remoteID))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/api/recv/cancel", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cancel notice: %s: %s", resp.Status, readMsg(resp.Body))
	}
	return nil
}

// BodyType is the Content-Type of a protocol v2 body.
const BodyType = "application/octet-stream"

// writeSealed writes the body of m beginning at start to w as a
// securestream under key, final frame included.
func writeSealed(w io.Writer, key []byte, m protocol.Manifest, start Start, open OpenFunc, tr *tracker) error {
	sw, err := securestream.NewWriter(w, key)
	if err != nil {
		return err
	}
	if err := writeBody(sw, m, start, open, tr); err != nil {
		return err
	}
	return sw.Close()
}

// IsHostPort reports whether s is a literal host:port (numeric port), as
// opposed to a peer name or fingerprint prefix.
func IsHostPort(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

// Hello fetches GET /api/hello from addr (host:port).
func Hello(ctx context.Context, addr string) (protocol.Hello, error) {
	var h protocol.Hello
	ctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/hello", nil)
	if err != nil {
		return h, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("hello from %s: %s", addr, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&h); err != nil {
		return h, fmt.Errorf("hello from %s: %w", addr, err)
	}
	return h, nil
}

func postOffer(ctx context.Context, addr string, offer *protocol.Offer) (protocol.OfferReply, error) {
	var reply protocol.OfferReply
	b, err := json.Marshal(offer)
	if err != nil {
		return reply, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/api/offer", bytes.NewReader(b))
	if err != nil {
		return reply, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return reply, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reply, fmt.Errorf("offer rejected: %s: %s", resp.Status, readMsg(resp.Body))
	}
	return reply, json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&reply)
}

func readMsg(r io.Reader) string {
	msg, _ := io.ReadAll(io.LimitReader(r, 512))
	return string(bytes.TrimSpace(msg))
}

// httpClient has no overall timeout: large files take as long as they take.
// The caller's context governs cancellation.
var httpClient = &http.Client{}
