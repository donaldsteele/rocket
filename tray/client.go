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
	// OldPeers counts the peers that speak another protocol than rocket
	// (an older or newer rocket): they cannot be sent to until updated.
	OldPeers int
	// Reconnecting counts sends whose connection broke and that are trying
	// again; Interrupted counts sends that gave up plus receives kept for a
	// resume. Both are 0 when rocket cannot tell (an older rocket).
	Reconnecting int
	Interrupted  int
	// Pair is rocket's pairing window (GET /api/pair).
	Pair PairState
	Err  error // why Running is false
}

// PairState is rocket's pairing window as the tray knows it. While it is
// open, rocket accepts files from rocket senders it does not trust yet
// without asking, and trusts their keys from then on.
type PairState struct {
	// Supported is true when rocket answered /api/pair. An older rocket
	// (404/405) or a failed read leaves it false and the menu item hidden.
	Supported bool
	Open      bool
	Seconds   int // seconds left while Open, rounded up by rocket
}

// pairSeconds is how long "Pair new devices" opens the window for; every
// rocket UI uses the same 5 minutes.
const pairSeconds = 300

// errPairUnsupported means rocket has no /api/pair (it is older than the
// pairing window).
var errPairUnsupported = errors.New("this rocket has no pairing window")

// proto is the wire protocol this tray's rocket speaks.
const proto = 2

// Status reads /api/self, /api/peers and /api/incoming. Any failure, or an
// answer that does not look like rocket, reports Running false with Err set.
// /api/transfers and /api/resumable add the reconnecting and interrupted
// counts; a failure there leaves them 0. /api/pair adds the pairing window;
// a failure there (or an older rocket without it) leaves Pair unsupported.
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
	var peers []struct {
		Proto int `json:"proto"`
	}
	var incoming []json.RawMessage
	if err := c.getJSON(ctx, "/api/peers", &peers); err != nil {
		return Status{Err: err}
	}
	if err := c.getJSON(ctx, "/api/incoming", &incoming); err != nil {
		return Status{Err: err}
	}
	st := Status{
		Running:  true,
		Name:     self.Name,
		Version:  self.Version,
		Peers:    len(peers),
		Incoming: len(incoming),
	}
	for _, p := range peers {
		if p.Proto != 0 && p.Proto != proto { // 0: a rocket that does not say
			st.OldPeers++
		}
	}
	var xfers []struct {
		State string `json:"state"`
	}
	if c.getJSON(ctx, "/api/transfers", &xfers) == nil {
		for _, x := range xfers {
			if x.State == "reconnecting" {
				st.Reconnecting++
			}
		}
	}
	var res struct {
		Outgoing []json.RawMessage `json:"outgoing"`
		Incoming []json.RawMessage `json:"incoming"`
	}
	if c.getJSON(ctx, "/api/resumable", &res) == nil {
		st.Interrupted = len(res.Outgoing) + len(res.Incoming)
	}
	if p, err := c.Pair(ctx); err == nil {
		st.Pair = p
	}
	return st
}

// pairWire is what GET and POST /api/pair answer.
type pairWire struct {
	Open    bool `json:"open"`
	Seconds int  `json:"seconds"`
}

func (w pairWire) state() PairState {
	p := PairState{Supported: true, Open: w.Open, Seconds: w.Seconds}
	if !p.Open {
		p.Seconds = 0
	}
	return p
}

// Pair reads the pairing window (GET /api/pair). An older rocket answers
// 404 or 405 there, which is errPairUnsupported.
func (c *Client) Pair(ctx context.Context) (PairState, error) {
	var w pairWire
	if err := c.getJSON(ctx, "/api/pair", &w); err != nil {
		return PairState{}, pairErr(err)
	}
	return w.state(), nil
}

// SetPair opens the pairing window for seconds, or closes it when seconds
// is 0 (POST /api/pair with the CSRF header), and returns the new state.
func (c *Client) SetPair(ctx context.Context, seconds int) (PairState, error) {
	var w pairWire
	body := fmt.Sprintf(`{"seconds":%d}`, seconds)
	if err := c.postJSON(ctx, "/api/pair", body, &w); err != nil {
		return PairState{}, pairErr(err)
	}
	return w.state(), nil
}

func pairErr(err error) error {
	var se *statusError
	if errors.As(err, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusMethodNotAllowed) {
		return errPairUnsupported
	}
	return err
}

// Quit asks rocket to shut down (POST /api/quit with the CSRF header).
func (c *Client) Quit(ctx context.Context) error {
	if err := c.postJSON(ctx, "/api/quit", "{}", nil); err != nil {
		var se *statusError
		if errors.As(err, &se) {
			return fmt.Errorf("quit: %s", se.Status)
		}
		return err
	}
	return nil
}

// statusError is a non-200 answer from rocket.
type statusError struct {
	Path   string
	Code   int
	Status string
}

func (e *statusError) Error() string { return e.Path + ": " + e.Status }

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, path, v)
}

// postJSON sends body to path with the CSRF header rocket requires on every
// non-GET call, and decodes the answer into v unless v is nil.
func (c *Client) postJSON(ctx context.Context, path, body string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, "1")
	return c.do(req, path, v)
}

func (c *Client) do(req *http.Request, path string, v any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || v == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		if resp.StatusCode != http.StatusOK {
			return &statusError{Path: path, Code: resp.StatusCode, Status: resp.Status}
		}
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
