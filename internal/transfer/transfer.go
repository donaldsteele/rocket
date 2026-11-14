// Package transfer carries files from one rocket host to another with a
// two-phase push (protocol v1): the sender fetches the receiver's hello,
// sends a signed offer (metadata only), the receiver accepts (trust-on-first-
// use prompt), then the sender streams the bytes with a one-shot token.
// Nothing is buffered whole in memory; the body streams straight to disk.
package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/protocol"
)

// Target is the peer to send to. Addr is host:port of its HTTP API. FP, when
// known (from discovery), must match the fingerprint the peer answers with.
type Target struct {
	Addr string
	FP   string
}

// ProgressFn is called with bytes transferred so far.
type ProgressFn func(sent, total int64)

// ErrDeclined is wrapped by Send/SendReader when the receiver says no.
var ErrDeclined = errors.New("receiver declined")

// helloTimeout bounds the hello round trip; the offer long-poll and the
// upload are governed only by the caller's context.
const helloTimeout = 10 * time.Second

// Send offers a local file to the target, waits for acceptance, then streams
// it. progress may be nil.
func Send(ctx context.Context, id *node.Identity, t Target, path string, progress ProgressFn) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return send(ctx, id, t, filepath.Base(path), st.Size(), uint32(st.Mode().Perm()), f, progress)
}

// SendReader offers one file of exactly size bytes named name, waits for
// acceptance, then streams src to the peer. Used to relay a browser upload
// straight through to a peer without staging it on disk.
func SendReader(ctx context.Context, id *node.Identity, t Target, name string, size int64, src io.Reader, progress ProgressFn) error {
	return send(ctx, id, t, name, size, 0o644, src, progress)
}

func send(ctx context.Context, id *node.Identity, t Target, name string, size int64, mode uint32, src io.Reader, progress ProgressFn) error {
	if size < 0 {
		return fmt.Errorf("unknown size for %s", name)
	}
	hello, err := Hello(ctx, t.Addr)
	if err != nil {
		return err
	}
	if _, err := protocol.CheckHello(hello, t.FP); err != nil {
		return fmt.Errorf("peer %s: %w", t.Addr, err)
	}
	m := protocol.FileManifest(SanitizeComponent(name), size, mode)
	offer, err := protocol.NewOffer(id, id.Name, hello.FP, m, time.Now())
	if err != nil {
		return err
	}
	reply, err := postOffer(ctx, t.Addr, offer)
	if err != nil {
		return err
	}
	if !reply.Accepted {
		if reply.Reason == "" {
			reply.Reason = "declined"
		}
		return fmt.Errorf("%w: %s", ErrDeclined, reply.Reason)
	}

	body := src
	if progress != nil {
		body = &countingReader{r: src, total: size, fn: progress}
	}
	u := fmt.Sprintf("http://%s/api/recv?token=%s", t.Addr, url.QueryEscape(reply.Token))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload failed: %s: %s", resp.Status, readMsg(resp.Body))
	}
	return nil
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

type countingReader struct {
	r          io.Reader
	total      int64
	sent       int64
	fn         ProgressFn
	lastReport time.Time
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.sent += int64(n)
	// Throttle progress callbacks to at most ~20/sec.
	if c.fn != nil && (err != nil || c.sent >= c.total || time.Since(c.lastReport) > 50*time.Millisecond) {
		c.lastReport = time.Now()
		c.fn(c.sent, c.total)
	}
	return n, err
}
