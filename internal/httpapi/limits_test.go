package httpapi

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// TestRelayRequiresFingerprintOrAddr: the UI relay names peers by full
// fingerprint only, never by an unauthenticated name or a prefix.
func TestRelayRequiresFingerprintOrAddr(t *testing.T) {
	a := newTestHost(t, "alice")
	for _, peer := range []string{"bob", "ab12", "ab12cd34ef56789", "ab12cd34ef567890x"} {
		code, body := a.call("POST", "/api/relay?peer="+peer, nil, true)
		if code != http.StatusBadRequest {
			t.Errorf("relay ?peer=%s: %d %s, want 400", peer, code, body)
		}
	}
}

// TestResolveTargetIgnoresNameImpostor: a LAN host announcing itself under
// another peer's fingerprint as its name, more recently, does not capture
// sends addressed to that fingerprint.
func TestResolveTargetIgnoresNameImpostor(t *testing.T) {
	a := newTestHost(t, "alice")
	const victim, attacker = "ab12cd34ef567890", "9999999999999999"
	a.srv.reg.Observe(discovery.ViaBeacon, discovery.Peer{Name: "laptop", Fingerprint: victim, Addr: "192.168.1.5:29005"})
	time.Sleep(5 * time.Millisecond)
	a.srv.reg.Observe(discovery.ViaBeacon, discovery.Peer{Name: victim, Fingerprint: attacker, Addr: "192.168.1.66:29005"})
	tg, name, err := a.srv.resolveTarget(victim)
	if err != nil || tg.FP != victim || tg.Addr != "192.168.1.5:29005" || name != "laptop" {
		t.Fatalf("resolveTarget(fp) = %+v %q %v; want the real peer", tg, name, err)
	}
	a.srv.reg.Observe(discovery.ViaBeacon, discovery.Peer{Name: "laptop", Fingerprint: attacker, Addr: "192.168.1.66:29005"})
	if _, _, err := a.srv.resolveTarget("laptop"); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("resolveTarget(shared name) = %v; want ambiguous", err)
	}
}

func TestOfferRateLimitedPerIP(t *testing.T) {
	a, b := trustedPair(t)
	b.srv.offers = newRateLimiter(2, time.Hour)
	for i := range 2 {
		if code, body, _ := postOffer(t, b, fileOffer(t, a, b, "x.txt", 1, time.Now())); code != 200 {
			t.Fatalf("offer %d: %d %s", i, code, body)
		}
	}
	if code, body, _ := postOffer(t, b, fileOffer(t, a, b, "x.txt", 1, time.Now())); code != http.StatusTooManyRequests {
		t.Fatalf("offer over the rate limit: %d %s", code, body)
	}
}

func TestRateLimiterRefills(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRateLimiter(2, time.Second)
	l.now = func() time.Time { return now }
	if !l.allow("a") || !l.allow("a") || l.allow("a") {
		t.Fatal("burst of 2 not enforced")
	}
	if !l.allow("b") {
		t.Fatal("another IP was limited")
	}
	now = now.Add(time.Second)
	if !l.allow("a") || l.allow("a") {
		t.Fatal("one token per refill interval expected")
	}
}

// TestUntrustedOfferRefusedBeforeSpendingNonce: while the pending limits are
// reached, an untrusted sender's offer is refused before its nonce is
// recorded, so throwaway keys cannot churn the replay cache that way.
func TestUntrustedOfferRefusedBeforeSpendingNonce(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	for i := range 4 {
		po := &pendingOffer{view: IncomingOffer{TransferID: fmt.Sprint("p", i)}, ip: "127.0.0.1", reply: make(chan decision, 1)}
		if err := b.srv.addPending(po); err != nil {
			t.Fatal(err)
		}
	}
	o := fileOffer(t, a, b, "x.txt", 1, time.Now())
	if code, body, _ := postOffer(t, b, o); code != http.StatusTooManyRequests {
		t.Fatalf("offer while pending is full: %d %s", code, body)
	}
	for i := range 4 {
		b.srv.dropPending(fmt.Sprint("p", i))
	}
	// Had the refused offer spent its nonce, this would be a replay (400).
	if err := b.srv.trust.Approve(a.id.Fingerprint(), "alice", a.id.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	if code, body, reply := postOffer(t, b, o); code != 200 || !reply.Accepted {
		t.Fatalf("same offer once there is room: %d %s", code, body)
	}
}

// TestOfferBodyReadDeadline: an offer body trickled in (here: never
// finished) is cut off after offerReadTimeout instead of holding the
// connection for ever.
func TestOfferBodyReadDeadline(t *testing.T) {
	old := offerReadTimeout
	offerReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { offerReadTimeout = old })
	b := newTestHost(t, "bob")
	for _, path := range []string{"/api/offer", "/drop/offer"} {
		c, err := net.Dial("tcp", b.addr())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n{\"proto\":1,", path, b.addr())
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		start := time.Now()
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: trickled body answered %d", path, resp.StatusCode)
			}
		} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("%s: server still waiting for the body after 10s", path)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: took %s to give up", path, d)
		}
		c.Close()
	}
}

func TestLimitListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := LimitListener(inner, 2).(*limitListener)
	ln.exempt = func(net.IP) bool { return false } // treat loopback like a LAN host
	defer ln.Close()
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	take := func() net.Conn {
		select {
		case c := <-accepted:
			t.Cleanup(func() { c.Close() })
			return c
		case <-time.After(5 * time.Second):
			t.Fatal("connection not accepted")
			return nil
		}
	}
	refused := func(c net.Conn, what string) {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := c.Read(make([]byte, 1))
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("%s was left open", what)
		}
		if err == nil {
			t.Fatalf("%s was served", what)
		}
		select {
		case <-accepted:
			t.Fatalf("%s was accepted", what)
		default:
		}
	}
	dial()
	dial()
	s1 := take()
	take()
	refused(dial(), "a third connection from one IP")
	s1.Close()
	s1.Close() // a second Close must not release the slot twice
	dial()
	take()
	refused(dial(), "a connection over the cap after a double Close")
}

// TestCancelTransfersOnQuit: quitting while a transfer is being received
// cancels it properly: partial files removed, history says cancelled, the
// sender is told (410), and nothing is left in flight.
func TestCancelTransfersOnQuit(t *testing.T) {
	a, b := trustedPair(t)
	sr := startSlowRelay(t, a, b, []transfer.BrowserFile{
		{Name: "photos/one.bin", Size: 64 << 10}, {Name: "photos/two.bin", Size: 256 << 20},
	})
	in := b.waitMoving("in")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.srv.CancelTransfers(ctx)
	if ctx.Err() != nil {
		t.Fatal("CancelTransfers did not finish in time")
	}
	if n := b.srv.xfers.Len(); n != 0 {
		t.Fatalf("%d transfers still in flight", n)
	}
	if e := b.waitHistory(in.ID); e.Result != history.ResultCancelled {
		t.Fatalf("receiver history: %+v", e)
	}
	b.assertNoPartials()
	if r := sr.wait(t); r.code != http.StatusGone {
		t.Fatalf("relay: %d %q %v", r.code, r.msg, r.err)
	}
}

// TestViewOfSanitizesNames: the accept prompt shows names as they will be
// written, so a bidi override cannot make "invoice<RLO>fdp.exe" read as a PDF.
func TestViewOfSanitizesNames(t *testing.T) {
	rlo := string(rune(0x202e))
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "docs", Mode: protocol.ModeDir}, {Path: "docs/invoice" + rlo + "fdp.exe", Size: 1},
	}, Total: 1}
	v := viewOf("id", m)
	if len(v.Names) != 2 || v.Names[1] != "docs/invoice_fdp.exe" || v.Name != "docs" {
		t.Fatalf("view names = %q (name %q)", v.Names, v.Name)
	}
}
