package rocketcore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/protocol/securestream"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// --- phone -> desktop ---

func TestPhoneSendsToDesktop(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	if err := desk.trust.Approve(phone.Fingerprint(), "pixel", phone.PublicKey()); err != nil {
		t.Fatal(err)
	}

	// One file, read in chunks (a provider without descriptors).
	ev := newEvents(t)
	tid, err := phone.Send(desk.addr(), desk.id.Fingerprint(), &testSource{t: t, items: []item{
		{path: "hello.txt", data: []byte("hello, desk")},
	}}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if sent := ev.wait("sent"); sent.Data["transfer_id"] != tid || sent.Data["bytes"] != float64(11) {
		t.Fatalf("sent event %v", sent.Data)
	}

	// A picked folder: descriptors, a size only the descriptor knows, a
	// nested file read in chunks and an empty folder.
	big := bytes.Repeat([]byte("0123456789abcdef"), 40000)
	ev = newEvents(t)
	if _, err := phone.Send(desk.addr(), desk.id.Fingerprint(), &testSource{t: t, items: []item{
		{path: "album", dir: true},
		{path: "album/a.jpg", data: big, fd: true, noSize: true},
		{path: "album/sub/b.txt", data: []byte("bee")},
		{path: "album/empty", dir: true},
	}}, ev); err != nil {
		t.Fatal(err)
	}
	ev.wait("sent")
	sameTree(t, snapshot(t, desk.cfg.DownloadDir), map[string]string{
		"hello.txt":       "hello, desk",
		"album":           "<dir>",
		"album/a.jpg":     string(big),
		"album/sub":       "<dir>",
		"album/sub/b.txt": "bee",
		"album/empty":     "<dir>",
	})

	h, err := phone.HistoryJSON(0)
	if err != nil {
		t.Fatal(err)
	}
	var list []struct {
		Dir    string `json:"dir"`
		Result string `json:"result"`
		Name   string `json:"name"`
		PeerFP string `json:"peer_fp"`
	}
	if err := json.Unmarshal([]byte(h), &list); err != nil || len(list) != 2 {
		t.Fatalf("history %s (%v)", h, err)
	}
	for _, e := range list {
		if e.Dir != "out" || e.Result != "ok" || strings.ToLower(e.PeerFP) != desk.id.Fingerprint() {
			t.Fatalf("history entry %+v", e)
		}
	}
	if list[0].Name != "album" || list[1].Name != "hello.txt" {
		t.Fatalf("history order %+v", list)
	}
}

func TestPhoneSendDeclinedByDesktop(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	ev := newEvents(t)
	if _, err := phone.Send(desk.addr(), desk.id.Fingerprint(), &testSource{t: t, items: []item{
		{path: "x.txt", data: []byte("x")},
	}}, ev); err != nil {
		t.Fatal(err)
	}
	o := desk.waitIncoming()
	if o.SenderFP != phone.Fingerprint() || o.SenderName != "pixel" || o.Name != "x.txt" {
		t.Fatalf("desktop prompt %+v", o)
	}
	if code, b := desk.call("POST", "/api/accept", map[string]any{"id": o.ID, "accept": false}); code != 200 {
		t.Fatalf("accept: %d %s", code, b)
	}
	if e := ev.wait("error"); e.Data["result"] != "declined" {
		t.Fatalf("error event %v", e.Data)
	}
	if n := len(snapshot(t, desk.cfg.DownloadDir)); n != 0 {
		t.Fatalf("%d items received", n)
	}
}

func TestPhoneSendRefusesWrongFingerprint(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	ev := newEvents(t)
	if _, err := phone.Send(desk.addr(), "0123456789abcdef", &testSource{t: t, items: []item{
		{path: "x.txt", data: []byte("x")},
	}}, ev); err != nil {
		t.Fatal(err)
	}
	if e := ev.wait("error"); !strings.Contains(e.Data["error"].(string), "expected 0123456789abcdef") {
		t.Fatalf("error event %v", e.Data)
	}
	for _, bad := range []string{"desk", "0123"} {
		if _, err := phone.Send(desk.addr(), bad, &testSource{t: t, items: []item{{path: "x", data: []byte("x")}}}, ev); err == nil {
			t.Fatalf("Send with fp %q accepted", bad)
		}
	}
	if _, err := phone.Send("", desk.id.Fingerprint(), &testSource{t: t, items: []item{{path: "x", data: []byte("x")}}}, ev); err == nil {
		t.Fatal("Send to an undiscovered fingerprint accepted")
	}
}

func TestPhoneSendCancel(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	if err := desk.trust.Approve(phone.Fingerprint(), "pixel", phone.PublicKey()); err != nil {
		t.Fatal(err)
	}
	ev := newEvents(t)
	tid, err := phone.Send(desk.addr(), desk.id.Fingerprint(), &testSource{t: t, items: []item{
		{path: "slow.bin", data: make([]byte, 256<<20), slow: true},
	}}, ev)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if p := ev.wait("progress"); p.Data["sent"].(float64) > 0 {
			break
		}
	}
	if !phone.Cancel(tid) {
		t.Fatal("Cancel did not know the send")
	}
	c := ev.wait("cancelled")
	if c.Data["by"] != "local" || c.Data["dir"] != "out" {
		t.Fatalf("cancelled event %v", c.Data)
	}
	if e, _ := c.Data["error"].(string); e != "transfer cancelled on this device" {
		t.Fatalf("cancelled error %q, want it to say this device", e)
	}
	waitEmpty(t, desk.cfg.DownloadDir)
	if phone.TransfersJSON() != "[]" {
		t.Fatalf("still in flight: %s", phone.TransfersJSON())
	}
}

// A peer picked from the list keeps its name in the history even when
// discovery no longer knows it by the time the send starts.
func TestPhoneSendNamedRecordsShownName(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	if err := desk.trust.Approve(phone.Fingerprint(), "pixel", phone.PublicKey()); err != nil {
		t.Fatal(err)
	}
	send := func(name string) {
		t.Helper()
		ev := newEvents(t)
		if _, err := phone.SendNamed(desk.addr(), desk.id.Fingerprint(), name, &testSource{t: t, items: []item{
			{path: "n.txt", data: []byte("n")},
		}}, ev); err != nil {
			t.Fatal(err)
		}
		ev.wait("sent")
	}
	send(" desk\u200b ")
	send("")
	h, err := phone.HistoryJSON(0)
	if err != nil {
		t.Fatal(err)
	}
	var list []struct {
		PeerName string `json:"peer_name"`
	}
	if err := json.Unmarshal([]byte(h), &list); err != nil || len(list) != 2 {
		t.Fatalf("history %s (%v)", h, err)
	}
	if list[0].PeerName != desk.addr() || list[1].PeerName != "desk" {
		t.Fatalf("peer names %+v, want %q then desk", list, desk.addr())
	}
}

func TestPhoneSendUnknownSizeWithoutFdRefused(t *testing.T) {
	phone := newPhone(t, "pixel")
	_, err := phone.Send("127.0.0.1:29402", "", &testSource{t: t, items: []item{
		{path: "x.bin", data: []byte("x"), noSize: true},
	}}, newEvents(t))
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Send = %v, want an unknown-size error", err)
	}
}

// --- desktop -> phone ---

// startReceiving starts the phone's receiver and returns its address.
func startReceiving(t *testing.T, phone *Node, d *decider, sink *tempSink, ev *events) string {
	t.Helper()
	port, err := phone.StartReceiver(0, d, sink, ev)
	if err != nil {
		t.Fatal(err)
	}
	if phone.ReceiverPort() != port {
		t.Fatalf("ReceiverPort %d, StartReceiver %d", phone.ReceiverPort(), port)
	}
	return "127.0.0.1:" + strconv.Itoa(port)
}

func writeTree(t *testing.T, files map[string]string, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDesktopSendsToPhone(t *testing.T) {
	for _, useFd := range []bool{true, false} {
		t.Run("fd="+strconv.FormatBool(useFd), func(t *testing.T) {
			phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
			d := &decider{n: phone, accept: true}
			sink, ev := newTempSink(t, useFd), newEvents(t)
			addr := startReceiving(t, phone, d, sink, ev)
			target := transfer.Target{Addr: addr, FP: phone.Fingerprint()}

			src := writeTree(t, map[string]string{
				"note.txt":          "a note",
				"photos/beach.jpg":  strings.Repeat("sand", 100000),
				"photos/2024/a.txt": "a",
				"photos/2024/b.txt": "",
			}, "photos/empty")
			ctx := context.Background()
			if err := transfer.Send(ctx, desk.id, target, filepath.Join(src, "note.txt"), nil); err != nil {
				t.Fatal(err)
			}
			done := ev.wait("done")
			if done.Data["files"] != float64(1) || done.Data["from"] != "desk" || !strings.HasSuffix(done.Data["path"].(string), "note.txt") {
				t.Fatalf("done event %v", done.Data)
			}
			if err := transfer.Send(ctx, desk.id, target, filepath.Join(src, "photos"), nil); err != nil {
				t.Fatal(err)
			}
			ev.wait("done")
			// The same names again get fresh ones, never overwriting.
			if err := transfer.SendPaths(ctx, desk.id, target, []string{filepath.Join(src, "note.txt"), filepath.Join(src, "photos")}, nil); err != nil {
				t.Fatal(err)
			}
			if done := ev.wait("done"); done.Data["path"] != sink.root || len(done.Data["paths"].([]any)) != 2 {
				t.Fatalf("done event %v", done.Data)
			}

			photos := map[string]string{
				"":            "<dir>",
				"/beach.jpg":  strings.Repeat("sand", 100000),
				"/2024":       "<dir>",
				"/2024/a.txt": "a",
				"/2024/b.txt": "",
				"/empty":      "<dir>",
			}
			want := map[string]string{"note.txt": "a note", "note (1).txt": "a note"}
			for _, top := range []string{"photos", "photos (1)"} {
				for k, v := range photos {
					want[top+k] = v
				}
			}
			sameTree(t, sink.tree(), want)
			if d.count() != 3 {
				t.Fatalf("asked %d times, want 3", d.count())
			}
			if begun, aborted := sink.counts(); begun != 3 || aborted != 0 {
				t.Fatalf("sink begun %d aborted %d", begun, aborted)
			}
		})
	}
}

func TestPhoneDeclines(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	d := &decider{n: phone, accept: false}
	sink, ev := newTempSink(t, true), newEvents(t)
	addr := startReceiving(t, phone, d, sink, ev)
	src := writeTree(t, map[string]string{"x.txt": "x"})
	err := transfer.Send(context.Background(), desk.id, transfer.Target{Addr: addr, FP: phone.Fingerprint()}, filepath.Join(src, "x.txt"), nil)
	if !errors.Is(err, transfer.ErrDeclined) {
		t.Fatalf("send = %v, want declined", err)
	}
	if g := ev.wait("offer_gone"); g.Data["reason"] != "declined" {
		t.Fatalf("offer_gone %v", g.Data)
	}
	d.mu.Lock()
	shown := d.asked[0]
	d.mu.Unlock()
	if shown["sender_fp"] != desk.id.Fingerprint() || shown["name"] != "x.txt" || shown["addr"] != "127.0.0.1" {
		t.Fatalf("offer shown %v", shown)
	}
	if begun, _ := sink.counts(); begun != 0 || len(sink.tree()) != 0 {
		t.Fatalf("declined offer reached the sink")
	}
	h, _ := phone.HistoryJSON(10)
	if !strings.Contains(h, `"result":"declined"`) {
		t.Fatalf("history %s", h)
	}
}

func TestPhoneTrustsSender(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	d := &decider{n: phone, accept: true, trust: true}
	sink, ev := newTempSink(t, true), newEvents(t)
	addr := startReceiving(t, phone, d, sink, ev)
	target := transfer.Target{Addr: addr, FP: phone.Fingerprint()}
	src := writeTree(t, map[string]string{"x.txt": "x"})
	send := func() {
		t.Helper()
		if err := transfer.Send(context.Background(), desk.id, target, filepath.Join(src, "x.txt"), nil); err != nil {
			t.Fatal(err)
		}
		ev.wait("done")
	}
	send()
	tr, _ := phone.TrustedJSON()
	if !strings.Contains(tr, desk.id.Fingerprint()) || !strings.Contains(tr, desk.id.PublicKeyB64()) {
		t.Fatalf("trusted %s", tr)
	}
	send() // auto-accepted
	if d.count() != 1 {
		t.Fatalf("asked %d times, want 1", d.count())
	}
	if err := phone.SetAutoAccept(false); err != nil {
		t.Fatal(err)
	}
	send() // auto-accept off: asked again
	if err := phone.SetAutoAccept(true); err != nil {
		t.Fatal(err)
	}
	if err := phone.Revoke(desk.id.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if err := phone.Revoke(desk.id.Fingerprint()); err == nil {
		t.Fatal("second revoke succeeded")
	}
	d.mu.Lock()
	d.trust = false
	d.mu.Unlock()
	send() // revoked: asked again
	if d.count() != 3 {
		t.Fatalf("asked %d times, want 3", d.count())
	}
	if tr, _ := phone.TrustedJSON(); tr != "[]" {
		t.Fatalf("trusted after revoke %s", tr)
	}
}

func TestPhoneCancelsReceive(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	d := &decider{n: phone, accept: true}
	sink, ev := newTempSink(t, true), newEvents(t)
	addr := startReceiving(t, phone, d, sink, ev)

	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "big", Mode: protocol.ModeDir | 0o755},
		{Path: "big/one.bin", Size: 64 << 10, Mode: 0o644},
		{Path: "big/two.bin", Size: 512 << 20, Mode: 0o644},
	}, Total: 64<<10 + 512<<20}
	open := func(_ int, e protocol.Entry) (io.ReadCloser, error) {
		return io.NopCloser(&slowZeros{n: e.Size}), nil
	}
	errc := make(chan error, 1)
	go func() {
		errc <- transfer.SendManifest(context.Background(), desk.id, transfer.Target{Addr: addr, FP: phone.Fingerprint()}, m, open, nil)
	}()
	var tid string
	for {
		p := ev.wait("progress")
		if p.Data["sent"].(float64) > 64<<10 {
			tid = p.Data["transfer_id"].(string)
			break
		}
	}
	if !phone.Cancel(tid) {
		t.Fatal("Cancel did not know the receive")
	}
	if c := ev.wait("cancelled"); c.Data["by"] != "local" || c.Data["dir"] != "in" {
		t.Fatalf("cancelled event %v", c.Data)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, transfer.ErrCancelled) {
			t.Fatalf("sender got %v, want cancelled by the receiver", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sender still sending")
	}
	waitEmpty(t, sink.root)
	if _, aborted := sink.counts(); aborted != 1 {
		t.Fatalf("sink aborted %d times", aborted)
	}
}

// slowZeros yields n zero bytes, a little at a time.
type slowZeros struct{ n int64 }

func (z *slowZeros) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	time.Sleep(2 * time.Millisecond)
	k := min(int64(len(p)), z.n, 32<<10)
	clear(p[:k])
	z.n -= k
	return int(k), nil
}

func TestPhoneRejectsForgedOffer(t *testing.T) {
	phone, desk, mallory := newPhone(t, "pixel"), newDesktop(t, "desk"), newDesktop(t, "mallory")
	d := &decider{n: phone, accept: true, trust: true}
	sink, ev := newTempSink(t, true), newEvents(t)
	addr := startReceiving(t, phone, d, sink, ev)
	src := writeTree(t, map[string]string{"x.txt": "x"})
	// The phone trusts desk.
	if err := transfer.Send(context.Background(), desk.id, transfer.Target{Addr: addr, FP: phone.Fingerprint()}, filepath.Join(src, "x.txt"), nil); err != nil {
		t.Fatal(err)
	}
	ev.wait("done")

	post := func(o *protocol.Offer) (int, string) {
		b, _ := json.Marshal(o)
		resp, err := http.Post("http://"+addr+"/api/offer", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	o, _, err := protocol.NewOffer(mallory.id, "desk", phone.Fingerprint(), protocol.FileManifest("evil.exe", 1, 0o644), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Mallory claims desk's fingerprint with her own key...
	o.SenderFP = desk.id.Fingerprint()
	_ = o.SignWith(mallory.id)
	if code, body := post(o); code != 400 || !strings.Contains(body, protocol.ErrFingerprint.Error()) {
		t.Fatalf("forged fingerprint: %d %s", code, body)
	}
	// ...and desk's real key, but cannot sign as desk.
	o.SenderPub = desk.id.PublicKeyB64()
	if code, body := post(o); code != 400 || !strings.Contains(body, protocol.ErrSignature.Error()) {
		t.Fatalf("stolen key: %d %s", code, body)
	}
	// An offer addressed to another host.
	o2, _, _ := protocol.NewOffer(desk.id, "desk", mallory.id.Fingerprint(), protocol.FileManifest("x", 1, 0o644), time.Now())
	if code, body := post(o2); code != 400 || !strings.Contains(body, protocol.ErrWrongReceiver.Error()) {
		t.Fatalf("wrong receiver: %d %s", code, body)
	}
	if d.count() != 1 {
		t.Fatalf("forged offers were shown: asked %d times", d.count())
	}
}

// TestPhoneRejectsBadBodies: the phone's receiver checks the encrypted body
// like the desktop's: a tampered frame, a cut-off stream, data after the end
// and a body sent in clear each get 400, and nothing stays in the sink.
func TestPhoneRejectsBadBodies(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	d := &decider{n: phone, accept: true}
	sink, ev := newTempSink(t, false), newEvents(t)
	addr := startReceiving(t, phone, d, sink, ev)
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d", Mode: protocol.ModeDir | 0o755},
		{Path: "d/a.txt", Size: 3, Mode: 0o644},
		{Path: "d/b.txt", Size: 3, Mode: 0o644},
	}, Total: 6}
	h := transfer.Start{}.Header()
	good := append(h[:], "aaabbb"...)
	sealed := func(key, plain []byte) []byte {
		b, err := securestream.Seal(key, plain)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := map[string]func(key []byte) []byte{
		"tampered":  func(key []byte) []byte { b := sealed(key, good); b[len(b)-3] ^= 1; return b },
		"cut off":   func(key []byte) []byte { b := sealed(key, good); return b[:len(b)-1] },
		"trailing":  func(key []byte) []byte { return append(sealed(key, good), 'x') },
		"in clear":  func([]byte) []byte { return good },
		"too short": func(key []byte) []byte { return sealed(key, append(h[:], "aaabb"...)) },
	}
	for name, body := range cases {
		o, eph, err := protocol.NewOffer(desk.id, "desk", phone.Fingerprint(), m, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(o)
		resp, err := http.Post("http://"+addr+"/api/offer", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		var reply protocol.OfferReply
		_ = json.NewDecoder(resp.Body).Decode(&reply)
		resp.Body.Close()
		sb, _ := protocol.SigningBytes(o)
		if err := protocol.VerifyReply(phone.identity().Pub, sb, &reply); err != nil || !reply.Accepted {
			t.Fatalf("%s: reply %+v (%v)", name, reply, err)
		}
		key, err := protocol.BodyKey(eph, reply.Eph, o, reply.TransferID)
		if err != nil {
			t.Fatal(err)
		}
		resp, err = http.Post("http://"+addr+"/api/recv?token="+reply.Token, transfer.BodyType, bytes.NewReader(body(key)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
		if e := ev.wait("error"); e.Data["dir"] != "in" {
			t.Fatalf("%s: error event %v", name, e.Data)
		}
		waitEmpty(t, sink.root)
	}
}

// TestProtocol1RefusedByPhone: the phone refuses a protocol-1 offer with
// 426, and refuses to send to a protocol-1 peer with the update message.
func TestProtocol1RefusedByPhone(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	d := &decider{n: phone, accept: true}
	addr := startReceiving(t, phone, d, newTempSink(t, true), newEvents(t))
	o, _, err := protocol.NewOffer(desk.id, "desk", phone.Fingerprint(), protocol.FileManifest("x", 1, 0o644), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	o.Proto, o.Eph = 1, ""
	_ = o.SignWith(desk.id)
	b, _ := json.Marshal(o)
	resp, err := http.Post("http://"+addr+"/api/offer", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired || strings.TrimSpace(string(msg)) != protocol.OldSenderMsg {
		t.Fatalf("protocol-1 offer: %d %q", resp.StatusCode, msg)
	}
	if d.count() != 0 {
		t.Fatal("protocol-1 offer shown")
	}

	// A 1.0 desktop: its hello says protocol 1.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hello" {
			t.Errorf("1.0 peer got %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.Hello{Proto: 1, Name: "old laptop", FP: desk.id.Fingerprint(), Pub: desk.id.PublicKeyB64(), Version: "1.0.0"})
	}))
	defer old.Close()
	ev := newEvents(t)
	if _, err := phone.Send(strings.TrimPrefix(old.URL, "http://"), desk.id.Fingerprint(), &testSource{t: t, items: []item{
		{path: "x.txt", data: []byte("x")},
	}}, ev); err != nil {
		t.Fatal(err)
	}
	if e := ev.wait("error"); e.Data["error"] != "old laptop runs an older rocket (protocol 1); update it to 1.1 or later" {
		t.Fatalf("error event %v", e.Data)
	}
}

// TestEncryptedHistoryAndProto: both ends record peer transfers as
// encrypted, and the peer list carries each peer's protocol version.
func TestEncryptedHistoryAndProto(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	if err := desk.trust.Approve(phone.Fingerprint(), "pixel", phone.PublicKey()); err != nil {
		t.Fatal(err)
	}
	ev := newEvents(t)
	if _, err := phone.Send(desk.addr(), desk.id.Fingerprint(), &testSource{t: t, items: []item{
		{path: "e.txt", data: []byte("encrypted")},
	}}, ev); err != nil {
		t.Fatal(err)
	}
	ev.wait("sent")
	h, _ := phone.HistoryJSON(0)
	if !strings.Contains(h, `"encrypted":true`) {
		t.Fatalf("phone history %s", h)
	}
	code, body := desk.call("GET", "/api/history", nil)
	if code != 200 || !strings.Contains(string(body), `"encrypted":true`) {
		t.Fatalf("desktop history %d %s", code, body)
	}

	phone.ObserveMDNS("new desk", "00000000000000a2", "10.0.0.2", 29005, "1.1.0", "windows", "2")
	phone.ObserveMDNS("old desk", "00000000000000a1", "10.0.0.1", 29005, "1.0.0", "windows", "1")
	phone.ObserveMDNS("ancient", "00000000000000a0", "10.0.0.3", 29005, "0.9.0", "linux", "")
	var ps []struct {
		Name  string `json:"name"`
		Proto int    `json:"proto"`
	}
	if err := json.Unmarshal([]byte(phone.PeersJSON()), &ps); err != nil || len(ps) != 3 {
		t.Fatalf("peers %s (%v)", phone.PeersJSON(), err)
	}
	want := map[string]int{"new desk": ProtoVersion, "old desk": 1, "ancient": 1}
	for _, p := range ps {
		if want[p.Name] != p.Proto {
			t.Errorf("%s: proto %d, want %d", p.Name, p.Proto, want[p.Name])
		}
	}
}

func TestStopReceiverDeclinesWaitingOffers(t *testing.T) {
	phone, desk := newPhone(t, "pixel"), newDesktop(t, "desk")
	d := &decider{n: phone, silent: true}
	sink, ev := newTempSink(t, true), newEvents(t)
	addr := startReceiving(t, phone, d, sink, ev)
	src := writeTree(t, map[string]string{"x.txt": "x"})
	errc := make(chan error, 1)
	go func() {
		errc <- transfer.Send(context.Background(), desk.id, transfer.Target{Addr: addr, FP: phone.Fingerprint()}, filepath.Join(src, "x.txt"), nil)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for phone.IncomingJSON() == "[]" {
		if time.Now().After(deadline) {
			t.Fatal("offer never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(phone.IncomingJSON(), desk.id.Fingerprint()) {
		t.Fatalf("incoming %s", phone.IncomingJSON())
	}
	phone.StopReceiver()
	if err := <-errc; !errors.Is(err, transfer.ErrDeclined) {
		t.Fatalf("send = %v, want declined", err)
	}
	if phone.ReceiverPort() != 0 || phone.IncomingJSON() != "[]" {
		t.Fatal("receiver still running")
	}
	if _, err := http.Get("http://" + addr + "/api/hello"); err == nil {
		t.Fatal("port still open")
	}
	// It can start again.
	startReceiving(t, phone, d, sink, ev)
}

func TestPhoneHelloAndSurface(t *testing.T) {
	phone := newPhone(t, "my‮phone")
	addr := startReceiving(t, phone, &decider{n: phone}, newTempSink(t, true), newEvents(t))
	resp, err := http.Get("http://" + addr + "/api/hello")
	if err != nil {
		t.Fatal(err)
	}
	var h protocol.Hello
	_ = json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if _, err := protocol.CheckHello(h, phone.Fingerprint()); err != nil || h.OS != "android" || h.Version != "test" || h.Name != "myphone" {
		t.Fatalf("hello %+v (%v)", h, err)
	}
	// Only the peer protocol: no UI, control API or drop page.
	for _, p := range []string{"/", "/api/self", "/api/peers", "/api/incoming", "/api/events", "/drop.html"} {
		resp, err := http.Get("http://" + addr + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, resp.StatusCode)
		}
	}
	for _, p := range []string{"/api/accept", "/api/send", "/api/cancel", "/api/quit", "/drop/offer"} {
		resp, err := http.Post("http://"+addr+p, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: %d, want 404", p, resp.StatusCode)
		}
	}
}
