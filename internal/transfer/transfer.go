// Package transfer carries files from one rocket host to another with a
// two-phase push: the sender offers (metadata only), the receiver accepts
// (trust-on-first-use prompt), then the sender streams the bytes. Nothing is
// buffered whole in memory; the body streams straight to disk.
package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Offer is the metadata a sender presents before any bytes flow.
type Offer struct {
	ID          string `json:"id"`          // assigned by the receiver
	Name        string `json:"name"`        // base filename
	Size        int64  `json:"size"`        // bytes
	SenderName  string `json:"sender_name"` // human label
	SenderFP    string `json:"sender_fp"`   // sender fingerprint (TOFU identity)
	FromBrowser bool   `json:"from_browser"`
}

// OfferReply is the receiver's answer to an offer.
type OfferReply struct {
	Accepted bool   `json:"accepted"`
	Token    string `json:"token"` // one-shot token for the /recv upload
	Reason   string `json:"reason,omitempty"`
}

// ProgressFn is called with bytes transferred so far.
type ProgressFn func(sent, total int64)

// Send offers a local file to peerAddr (host:port), waits for acceptance, then
// streams it. progress may be nil.
func Send(ctx context.Context, peerAddr string, offer Offer, path string, progress ProgressFn) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if offer.Name == "" {
		offer.Name = filepath.Base(path)
	}
	if offer.Size == 0 {
		if st, err := f.Stat(); err == nil {
			offer.Size = st.Size()
		}
	}
	return SendReader(ctx, peerAddr, offer, f, progress)
}

// SendReader offers metadata, waits for acceptance, then streams body to the
// peer. offer.Name and offer.Size must be set. Used to relay a browser upload
// straight through to a peer without staging it on disk.
func SendReader(ctx context.Context, peerAddr string, offer Offer, src io.Reader, progress ProgressFn) error {
	reply, err := postOffer(ctx, peerAddr, offer)
	if err != nil {
		return err
	}
	if !reply.Accepted {
		if reply.Reason == "" {
			reply.Reason = "declined"
		}
		return fmt.Errorf("receiver declined: %s", reply.Reason)
	}

	body := src
	if progress != nil {
		body = &countingReader{r: src, total: offer.Size, fn: progress}
	}
	url := fmt.Sprintf("http://%s/api/recv?token=%s", peerAddr, reply.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.ContentLength = offer.Size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload failed: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

func postOffer(ctx context.Context, peerAddr string, offer Offer) (OfferReply, error) {
	var reply OfferReply
	b, _ := json.Marshal(offer)
	url := fmt.Sprintf("http://%s/api/offer", peerAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return reply, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return reply, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reply, fmt.Errorf("offer rejected: %s", resp.Status)
	}
	return reply, json.NewDecoder(resp.Body).Decode(&reply)
}

func httpClient() *http.Client {
	// No overall timeout: large files take as long as they take. The caller's
	// context governs cancellation.
	return &http.Client{}
}

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
	if c.fn != nil && (err != nil || time.Since(c.lastReport) > 50*time.Millisecond) {
		c.lastReport = time.Now()
		c.fn(c.sent, c.total)
	}
	return n, err
}
