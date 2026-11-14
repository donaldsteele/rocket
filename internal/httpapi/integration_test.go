package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// TestTwoServerSignedTransfer runs a signed single-file transfer between two
// independent rocket hosts (separate temp data and download dirs), through
// the accept prompt, then checks trust pinning and the trusted fast path.
func TestTwoServerSignedTransfer(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	events := b.srv.hub.subscribe()
	defer b.srv.hub.unsubscribe(events)

	data := make([]byte, 300<<10)
	_, _ = rand.Read(data)
	src := writeTemp(t, "photo.jpg", data)

	// 1. Alice's UI asks her rocket to send to Bob by host:port.
	code, body := a.call("POST", "/api/send", map[string]any{"peer": b.addr(), "paths": []string{src}}, true)
	if code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}

	// 2. Bob sees a signed offer from Alice's fingerprint and accepts with
	// "always accept from this host".
	offers := b.waitIncoming(1)
	o := offers[0]
	if o.SenderFP != a.id.Fingerprint() || o.Name != "photo.jpg" || o.Size != int64(len(data)) || o.Kind != "file" || o.TransferID == "" {
		t.Fatalf("offer view: %+v", o)
	}
	code, body = b.call("POST", "/api/accept", map[string]any{"id": o.ID, "accept": true, "trust": true}, true)
	if code != 200 {
		t.Fatalf("accept: %d %s", code, body)
	}

	// 3. The bytes arrive intact, via .rocket-part then rename.
	got := waitFile(t, filepath.Join(b.cfg.DownloadDir, "photo.jpg"), len(data))
	if !bytes.Equal(got, data) {
		t.Fatal("received bytes differ")
	}
	waitEvent(t, events, "done", o.TransferID)
	for _, n := range b.downloads() {
		if strings.HasSuffix(n, transfer.PartSuffix) {
			t.Fatalf("part file left: %s", n)
		}
	}

	// 4. Bob now trusts Alice, pinned to her full public key.
	if !b.srv.trust.IsTrusted(a.id.Fingerprint(), a.id.PublicKeyB64()) {
		t.Fatal("accept with trust did not pin Alice's key")
	}

	// 5. Trusted fast path: a second send needs no prompt.
	src2 := writeTemp(t, "notes.txt", []byte("second file"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := transfer.Send(ctx, a.id, transfer.Target{Addr: b.addr(), FP: b.id.Fingerprint()}, src2, nil); err != nil {
		t.Fatalf("trusted send: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(b.cfg.DownloadDir, "notes.txt")); string(got) != "second file" {
		t.Fatalf("second file: %q", got)
	}

	// 6. Same name again lands beside, never over, the first.
	if err := transfer.Send(ctx, a.id, transfer.Target{Addr: b.addr()}, src2, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.DownloadDir, "notes (1).txt")); err != nil {
		t.Fatalf("unique name: %v (have %v)", err, b.downloads())
	}
}

func TestTransferDeclined(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	src := writeTemp(t, "x.bin", []byte("data"))
	errc := make(chan error, 1)
	go func() {
		errc <- transfer.Send(context.Background(), a.id, transfer.Target{Addr: b.addr()}, src, nil)
	}()
	o := b.waitIncoming(1)[0]
	b.call("POST", "/api/accept", map[string]any{"id": o.ID, "accept": false}, true)
	if err := <-errc; !errors.Is(err, transfer.ErrDeclined) {
		t.Fatalf("declined send: %v", err)
	}
	if len(b.downloads()) != 0 || b.srv.trust.IsTrusted(a.id.Fingerprint(), a.id.PublicKeyB64()) {
		t.Fatal("declined offer left files or trust behind")
	}
}

func TestSendRefusesUnexpectedFingerprint(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	src := writeTemp(t, "x.bin", []byte("data"))
	// Discovery said this address was someone else: the hello must match.
	err := transfer.Send(context.Background(), a.id, transfer.Target{Addr: b.addr(), FP: "0123456789abcdef"}, src, nil)
	if err == nil {
		t.Fatal("send to a host answering with another fingerprint succeeded")
	}
	if n := len(b.waitIncomingNow()); n != 0 {
		t.Fatalf("offer reached the wrong host: %d pending", n)
	}
}

func (h *testHost) waitIncomingNow() []IncomingOffer {
	_, body := h.call("GET", "/api/incoming", nil, false)
	var list []IncomingOffer
	_ = json.Unmarshal(body, &list)
	return list
}

// TestDropFlow covers the browser drop: offer, accept prompt, cookie set
// (stored hashed), multipart upload matched against the offer, cookie skips
// the prompt next time, and a mismatched upload leaves nothing behind.
func TestDropFlow(t *testing.T) {
	b := newTestHost(t, "bob")
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	files := []dropFile{{Name: "a.txt", Size: 5}, {Name: `C:\fakepath\b.txt`, Size: 3}}
	replyc := make(chan protocol.OfferReply, 1)
	go func() { replyc <- dropOffer(t, client, b, files) }()
	o := b.waitIncoming(1)[0]
	if !o.FromBrowser || o.Files != 2 || o.Size != 8 || o.Name != "a.txt" {
		t.Fatalf("drop offer view: %+v", o)
	}
	b.call("POST", "/api/accept", map[string]any{"id": o.ID, "accept": true}, true)
	reply := <-replyc
	if !reply.Accepted || reply.Token == "" {
		t.Fatalf("drop reply: %+v", reply)
	}
	var cookie string
	for _, c := range jar.Cookies(mustURL(b.ts.URL)) {
		if c.Name == DropCookie {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("rocket_drop cookie not set")
	}
	raw, _ := os.ReadFile(filepath.Join(b.cfg.DataDir, "drop_tokens.json"))
	if strings.Contains(string(raw), cookie) {
		t.Fatal("drop token stored in clear")
	}

	// A drop token is not a peer token.
	resp, _ := client.Post(b.ts.URL+"/api/recv?token="+reply.Token, "application/octet-stream", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("drop token on /api/recv: %d", resp.StatusCode)
	}

	// The token was consumed above; a remembered browser gets a new one at once.
	reply = dropOffer(t, client, b, files)
	if !reply.Accepted {
		t.Fatalf("remembered browser prompted again: %+v", reply)
	}
	code, body := dropUpload(t, client, b, reply.Token, []part{{"a.txt", "hello"}, {"b.txt", "abc"}})
	if code != 200 {
		t.Fatalf("drop upload: %d %s", code, body)
	}
	for name, want := range map[string]string{"a.txt": "hello", "b.txt": "abc"} {
		got, _ := os.ReadFile(filepath.Join(b.cfg.DownloadDir, name))
		if string(got) != want {
			t.Fatalf("%s = %q", name, got)
		}
	}

	// Size mismatch on the second part: the whole upload is removed.
	before := len(b.downloads())
	reply = dropOffer(t, client, b, files)
	code, _ = dropUpload(t, client, b, reply.Token, []part{{"a.txt", "hello"}, {"b.txt", "too long"}})
	if code != 400 || len(b.downloads()) != before {
		t.Fatalf("mismatched upload: %d, files %v", code, b.downloads())
	}
	// Name mismatch.
	reply = dropOffer(t, client, b, files)
	code, _ = dropUpload(t, client, b, reply.Token, []part{{"evil.exe", "hello"}, {"b.txt", "abc"}})
	if code != 400 || len(b.downloads()) != before {
		t.Fatalf("renamed upload: %d, files %v", code, b.downloads())
	}
}

func dropOffer(t *testing.T, c *http.Client, h *testHost, files []dropFile) protocol.OfferReply {
	b, _ := json.Marshal(map[string]any{"files": files})
	resp, err := c.Post(h.ts.URL+"/drop/offer", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Error(err)
		return protocol.OfferReply{}
	}
	defer resp.Body.Close()
	var r protocol.OfferReply
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return r
}

type part struct{ name, data string }

func dropUpload(t *testing.T, c *http.Client, h *testHost, token string, parts []part) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		w, _ := mw.CreateFormFile("file", p.name)
		_, _ = io.WriteString(w, p.data)
	}
	_ = mw.Close()
	resp, err := c.Post(h.ts.URL+"/drop/upload?token="+token, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func waitFile(t *testing.T, path string, size int) []byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) == size {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file %s did not arrive", path)
	return nil
}

func waitEvent(t *testing.T, ch chan Event, typ, transferID string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			m, _ := ev.Data.(map[string]any)
			if ev.Type == typ && m["transfer_id"] == transferID {
				return
			}
		case <-timeout:
			t.Fatalf("no %q event for %s", typ, transferID)
		}
	}
}
