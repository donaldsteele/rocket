package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// csrfHeader must be "1" on every non-GET call to rocket's control API.
const csrfHeader = "X-Rocket-CSRF"

// maxBody caps how much of a response the tray reads.
const maxBody = 1 << 20

// Client talks to one rocket instance through its loopback-only API.
type Client struct {
	Base string // e.g. http://127.0.0.1:29005, no trailing slash
	HTTP *http.Client
}

// NewClient returns a client for rocket on 127.0.0.1:port. rocket's local API
// accepts only loopback callers whose Host is 127.0.0.1:<port>, localhost or
// [::1], so the address is fixed to 127.0.0.1.
func NewClient(port int) *Client {
	return &Client{
		Base: fmt.Sprintf("http://127.0.0.1:%d", port),
		HTTP: &http.Client{Timeout: 2 * time.Second},
	}
}

// UIURL is the address of rocket's full local UI.
func (c *Client) UIURL() string { return c.Base + "/" }

// Status is one poll of rocket's local API.
type Status struct {
	Running  bool   // rocket answered all three reads
	Name     string // this device's name in rocket
	Version  string // rocket's version
	Peers    int    // peers rocket can see on the LAN
	Incoming int    // offers waiting for an answer
	Err      error  // why Running is false
}

// Status reads /api/self, /api/peers and /api/incoming. Any failure, or an
// answer that does not look like rocket, reports Running false with Err set.
func (c *Client) Status(ctx context.Context) Status {
	var self struct {
		Name        string `json:"name"`
		Fingerprint string `json:"fingerprint"`
		Version     string `json:"version"`
	}
	if err := c.getJSON(ctx, "/api/self", &self); err != nil {
		return Status{Err: err}
	}
	if self.Fingerprint == "" {
		return Status{Err: errors.New("the program on this port is not rocket")}
	}
	var peers, incoming []json.RawMessage
	if err := c.getJSON(ctx, "/api/peers", &peers); err != nil {
		return Status{Err: err}
	}
	if err := c.getJSON(ctx, "/api/incoming", &incoming); err != nil {
		return Status{Err: err}
	}
	return Status{
		Running:  true,
		Name:     self.Name,
		Version:  self.Version,
		Peers:    len(peers),
		Incoming: len(incoming),
	}
}

// Quit asks rocket to shut down (POST /api/quit with the CSRF header).
func (c *Client) Quit(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/quit", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, "1")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("quit: %s", resp.Status)
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
