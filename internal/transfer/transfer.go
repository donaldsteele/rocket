// Package transfer carries files from one rocket host to another with a
// two-phase push (protocol v1): the sender fetches the receiver's hello,
// sends a signed offer (metadata only), the receiver accepts (trust-on-first-
// use prompt), then the sender streams the bytes with a one-shot token: the
// raw file for a kind:file manifest, a tar built on the fly for kind:multi
// (several files and folders). Nothing is buffered whole in memory or staged;
// the body streams straight to disk (Extractor, ReceiveTar, ReceiveFile).
package transfer

import (
	"archive/tar"
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
	return SendManifest(ctx, id, t, p.Manifest, func(i int, _ protocol.Entry) (io.ReadCloser, error) {
		return os.Open(p.Sources[i])
	}, progress)
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

// SendManifest is the core sender: hello, signed offer for m, wait for the
// decision, then stream the content (open supplies each file) as the raw
// body for kind:file or as a tar for kind:multi. Nothing is staged.
func SendManifest(ctx context.Context, id *node.Identity, t Target, m protocol.Manifest, open OpenFunc, progress ProgressFn) error {
	if err := m.Validate(); err != nil {
		return err
	}
	hello, err := Hello(ctx, t.Addr)
	if err != nil {
		return err
	}
	if _, err := protocol.CheckHello(hello, t.FP); err != nil {
		return fmt.Errorf("peer %s: %w", t.Addr, err)
	}
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

	tr := newTracker(progress, m.Total, Files(m))
	var (
		body    io.Reader
		length  int64
		ctype   = "application/octet-stream"
		pr      *io.PipeReader
		tarDone chan error
	)
	if m.Kind == protocol.KindFile {
		e := m.Entries[0]
		rc, err := open(0, e)
		if err != nil {
			return err
		}
		defer rc.Close()
		tr.file(e.Path)
		body, length = trackReader{r: io.LimitReader(rc, e.Size), t: tr}, e.Size
	} else {
		var pw *io.PipeWriter
		pr, pw = io.Pipe()
		tarDone = make(chan error, 1)
		go func() {
			err := writeTar(pw, m, open, tr)
			pw.CloseWithError(err)
			tarDone <- err
		}()
		body, length, ctype = pr, -1, "application/x-tar"
	}

	u := fmt.Sprintf("http://%s/api/recv?token=%s", t.Addr, url.QueryEscape(reply.Token))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return err
	}
	req.ContentLength = length
	if length == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := httpClient.Do(req)
	if tarDone != nil {
		// A failure while reading local files is the real cause; report it
		// rather than the aborted request it caused.
		pr.Close() // unblocks the tar writer if the request ended early
		if terr := <-tarDone; terr != nil && !errors.Is(terr, io.ErrClosedPipe) {
			if err == nil {
				resp.Body.Close()
			}
			return terr
		}
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload failed: %s: %s", resp.Status, readMsg(resp.Body))
	}
	tr.finish()
	return nil
}

// writeTar streams the entries of m as a tar archive: folders as TypeDir
// ("path/"), files as TypeReg with exactly the manifest size, in manifest
// order, so the receiver can match entry for entry.
func writeTar(w io.Writer, m protocol.Manifest, open OpenFunc, tr *tracker) error {
	tw := tar.NewWriter(w)
	now := time.Now()
	for i, e := range m.Entries {
		if e.Mode&protocol.ModeDir != 0 {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: e.Path + "/", Mode: 0o755, ModTime: now}); err != nil {
				return err
			}
			continue
		}
		rc, err := open(i, e)
		if err != nil {
			return err
		}
		mode := int64(e.Mode & 0o777)
		if mode == 0 {
			mode = 0o644
		}
		err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: e.Path, Size: e.Size, Mode: mode, ModTime: now})
		if err == nil {
			tr.file(e.Path)
			var n int64
			n, err = io.CopyN(trackWriter{w: tw, t: tr}, rc, e.Size)
			if errors.Is(err, io.EOF) {
				err = fmt.Errorf("%s: %w: it shrank while sending (%d of %d bytes)", e.Path, ErrSize, n, e.Size)
			}
		}
		rc.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
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
