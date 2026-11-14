package httpapi

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// relayResult is how a POST /api/relay ended for the browser.
type relayResult struct {
	code int
	msg  string
	err  error
}

// slowRelay drives a browser-style relay from A to B whose upload trickles in,
// so a transfer can be cancelled mid-stream. The declared files are far
// larger than what is written before the test cancels.
type slowRelay struct {
	done chan relayResult
	pr   *io.PipeReader
}

func startSlowRelay(t *testing.T, a, b *testHost, files []transfer.BrowserFile) *slowRelay {
	t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	req, err := http.NewRequest("POST", a.ts.URL+"/api/relay?peer="+b.addr(), pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(CSRFHeader, "1")
	sr := &slowRelay{done: make(chan relayResult, 1), pr: pr}
	t.Cleanup(func() { pr.CloseWithError(io.ErrClosedPipe) })
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			sr.done <- relayResult{err: err}
			return
		}
		msg, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		sr.done <- relayResult{code: resp.StatusCode, msg: string(msg)}
	}()
	go func() {
		mf, _ := mw.CreateFormField("manifest")
		if err := json.NewEncoder(mf).Encode(map[string]any{"files": files}); err != nil {
			return
		}
		chunk := make([]byte, 32<<10)
		for _, f := range files {
			w, err := mw.CreateFormFile("file", f.Name)
			if err != nil {
				return
			}
			for left := f.Size; left > 0; left -= int64(len(chunk)) {
				n := min(left, int64(len(chunk)))
				if _, err := w.Write(chunk[:n]); err != nil {
					return // the relay stopped reading
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		_ = mw.Close()
		_ = pw.Close()
	}()
	return sr
}

func (sr *slowRelay) wait(t *testing.T) relayResult {
	t.Helper()
	select {
	case r := <-sr.done:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("relay did not finish")
		return relayResult{}
	}
}

// transfers reads GET /api/transfers.
func (h *testHost) transfers() []transfer.Info {
	h.t.Helper()
	code, b := h.call("GET", "/api/transfers", nil, false)
	if code != 200 {
		h.t.Fatalf("transfers: %d %s", code, b)
	}
	var list []transfer.Info
	if err := json.Unmarshal(b, &list); err != nil {
		h.t.Fatal(err)
	}
	return list
}

// waitMoving waits until h has one transfer in direction dir with bytes
// moving, and returns it.
func (h *testHost) waitMoving(dir string) transfer.Info {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, x := range h.transfers() {
			if x.Dir == dir && x.State == transfer.StateActive && x.Sent > 0 {
				return x
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("no moving %q transfer", dir)
	return transfer.Info{}
}

// history reads GET /api/history.
func (h *testHost) history(q string) []history.Entry {
	h.t.Helper()
	code, b := h.call("GET", "/api/history"+q, nil, false)
	if code != 200 {
		h.t.Fatalf("history: %d %s", code, b)
	}
	var list []history.Entry
	if err := json.Unmarshal(b, &list); err != nil {
		h.t.Fatal(err)
	}
	return list
}

// waitHistory waits for the history entry of transfer id.
func (h *testHost) waitHistory(id string) history.Entry {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.history("") {
			if id == "" || e.ID == id {
				return e
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("no history entry for %q", id)
	return history.Entry{}
}

func (h *testHost) cancel(id string) {
	h.t.Helper()
	if code, b := h.call("POST", "/api/cancel", map[string]any{"transfer_id": id}, true); code != 200 {
		h.t.Fatalf("cancel %s: %d %s", id, code, b)
	}
}

// assertNoPartials checks that nothing at all is left in h's download folder.
func (h *testHost) assertNoPartials() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d := h.downloads()
		if len(d) == 0 {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("left in downloads: %v", d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCancelReceiverMidStream: the receiver cancels a folder transfer while
// the bytes are moving. Its partial files are removed, its history says
// cancelled, and the sender learns it was cancelled (410 Gone).
func TestCancelReceiverMidStream(t *testing.T) {
	a, b := trustedPair(t)
	events := b.srv.hub.subscribe()
	defer b.srv.hub.unsubscribe(events)
	sr := startSlowRelay(t, a, b, []transfer.BrowserFile{
		{Name: "big/one.bin", Size: 64 << 10}, {Name: "big/two.bin", Size: 256 << 20},
	})
	in := b.waitMoving("in")
	if in.Name != "big" || in.PeerFP != a.id.Fingerprint() || in.Files != 2 {
		t.Fatalf("incoming transfer view: %+v", in)
	}
	b.cancel(in.ID)

	waitEvent(t, events, "cancelled", in.ID)
	e := b.waitHistory(in.ID)
	if e.Result != history.ResultCancelled || e.Dir != history.DirIn || e.PeerName != "alice" || e.Files != 2 {
		t.Fatalf("receiver history: %+v", e)
	}
	b.assertNoPartials()
	if n := len(b.transfers()); n != 0 {
		t.Fatalf("%d transfers still listed", n)
	}

	r := sr.wait(t)
	if r.err != nil || r.code != http.StatusGone {
		t.Fatalf("relay after the receiver cancelled: %d %q %v", r.code, r.msg, r.err)
	}
	out := a.waitHistory("")
	if out.Result != history.ResultCancelled || out.Dir != history.DirOut || !strings.Contains(out.Error, "receiver") {
		t.Fatalf("sender history: %+v", out)
	}
}

// TestCancelReceiverSingleFile: the same for a kind:file transfer, whose
// .rocket-part file must be removed.
func TestCancelReceiverSingleFile(t *testing.T) {
	a, b := trustedPair(t)
	sr := startSlowRelay(t, a, b, []transfer.BrowserFile{{Name: "movie.mkv", Size: 256 << 20}})
	in := b.waitMoving("in")
	if !hasPart(b) {
		t.Fatal("no .rocket-part file while receiving")
	}
	b.cancel(in.ID)
	if e := b.waitHistory(in.ID); e.Result != history.ResultCancelled {
		t.Fatalf("receiver history: %+v", e)
	}
	b.assertNoPartials()
	if r := sr.wait(t); r.code != http.StatusGone {
		t.Fatalf("relay: %d %q %v", r.code, r.msg, r.err)
	}
}

func hasPart(h *testHost) bool {
	for _, n := range h.downloads() {
		if strings.HasSuffix(n, transfer.PartSuffix) {
			return true
		}
	}
	return false
}

// TestCancelSenderMidStream: the sender cancels while the bytes are moving.
// Its history says cancelled; the receiver sees the body cut off, removes its
// partial files and records the transfer as not completed.
func TestCancelSenderMidStream(t *testing.T) {
	a, b := trustedPair(t)
	events := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(events)
	sr := startSlowRelay(t, a, b, []transfer.BrowserFile{
		{Name: "big/one.bin", Size: 64 << 10}, {Name: "big/two.bin", Size: 256 << 20},
	})
	b.waitMoving("in")
	out := a.waitMoving("out")
	a.cancel(out.ID)

	waitEvent(t, events, "cancelled", out.ID)
	e := a.waitHistory(out.ID)
	if e.Result != history.ResultCancelled || e.Dir != history.DirOut || e.Name != "big" || e.Files != 2 {
		t.Fatalf("sender history: %+v", e)
	}
	if n := len(a.transfers()); n != 0 {
		t.Fatalf("%d transfers still listed", n)
	}
	sr.wait(t) // the browser's request ends one way or another

	in := b.waitHistory("")
	if in.Result == history.ResultOK || in.Dir != history.DirIn {
		t.Fatalf("receiver history: %+v", in)
	}
	b.assertNoPartials()
}

// TestCancelWithdrawsOffer: cancelling a send that is still waiting for the
// receiver's decision withdraws the offer: the receiver's prompt goes away
// (offer_gone) and both histories say cancelled.
func TestCancelWithdrawsOffer(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	events := b.srv.hub.subscribe()
	defer b.srv.hub.unsubscribe(events)
	src := writeTemp(t, "note.txt", []byte("hello"))
	code, body := a.call("POST", "/api/send", map[string]any{"peer": b.addr(), "paths": []string{src}}, true)
	if code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	var sent struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.Unmarshal(body, &sent)
	offer := b.waitIncoming(1)[0]
	list := a.transfers()
	if len(list) != 1 || list[0].ID != sent.TransferID || list[0].State != transfer.StateWaiting {
		t.Fatalf("sender transfers: %+v", list)
	}
	a.cancel(sent.TransferID)

	waitEvent(t, events, "offer_gone", offer.TransferID)
	if e := a.waitHistory(sent.TransferID); e.Result != history.ResultCancelled {
		t.Fatalf("sender history: %+v", e)
	}
	if e := b.waitHistory(offer.TransferID); e.Result != history.ResultCancelled || e.Name != "note.txt" || e.PeerFP != a.id.Fingerprint() {
		t.Fatalf("receiver history: %+v", e)
	}
	if n := len(b.waitIncomingNow()); n != 0 {
		t.Fatalf("%d offers still pending", n)
	}
}

// TestCancelPendingOfferDeclines: the receiver cancelling an offer it has not
// answered yet declines it.
func TestCancelPendingOfferDeclines(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	src := writeTemp(t, "note.txt", []byte("hello"))
	code, body := a.call("POST", "/api/send", map[string]any{"peer": b.addr(), "paths": []string{src}}, true)
	if code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	var sent struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.Unmarshal(body, &sent)
	offer := b.waitIncoming(1)[0]
	b.cancel(offer.TransferID)
	if e := b.waitHistory(offer.TransferID); e.Result != history.ResultDeclined {
		t.Fatalf("receiver history: %+v", e)
	}
	if e := a.waitHistory(sent.TransferID); e.Result != history.ResultDeclined {
		t.Fatalf("sender history: %+v", e)
	}
	if code, _ := b.call("POST", "/api/cancel", map[string]any{"transfer_id": "nope"}, true); code != 404 {
		t.Fatalf("cancel of an unknown transfer: %d", code)
	}
}

// TestHistoryRecordsCompletedTransfer: both ends write an ok entry with the
// same counts; ?limit= and clear work over the API.
func TestHistoryRecordsCompletedTransfer(t *testing.T) {
	a, b := trustedPair(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "pics", "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "pics", "a.jpg"), []byte("aaaa"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "pics", "sub", "b.jpg"), []byte("bb"), 0o644)
	for range 2 {
		code, body := a.call("POST", "/api/send", map[string]any{"peer": b.addr(), "paths": []string{filepath.Join(dir, "pics")}}, true)
		if code != 200 {
			t.Fatalf("send: %d %s", code, body)
		}
		var sent struct {
			TransferID string `json:"transfer_id"`
		}
		_ = json.Unmarshal(body, &sent)
		out := a.waitHistory(sent.TransferID)
		if out.Result != history.ResultOK || out.Files != 2 || out.Bytes != 6 || out.Name != "pics" || out.PeerName != b.addr() {
			t.Fatalf("sender entry: %+v", out)
		}
		if out.Ended.Before(out.Started) {
			t.Fatalf("ended before started: %+v", out)
		}
	}
	in := b.waitHistory("")
	if in.Result != history.ResultOK || in.Dir != history.DirIn || in.Files != 2 || in.Bytes != 6 || in.PeerFP != a.id.Fingerprint() {
		t.Fatalf("receiver entry: %+v", in)
	}
	if got := len(a.history("?limit=1")); got != 1 {
		t.Fatalf("limit=1 returned %d", got)
	}
	if code, _ := a.call("GET", "/api/history?limit=x", nil, false); code != 400 {
		t.Fatalf("bad limit: %d", code)
	}
	if code, b := a.call("POST", "/api/history/clear", nil, true); code != 200 {
		t.Fatalf("clear: %d %s", code, b)
	}
	if got := len(a.history("")); got != 0 {
		t.Fatalf("after clear: %d entries", got)
	}
}

// TestSettingsRoundTripAPI: PUT then GET /api/settings, the download folder
// change applies to the next transfer, and auto_accept_trusted=false makes a
// trusted sender go through the prompt again.
func TestSettingsRoundTripAPI(t *testing.T) {
	a, b := trustedPair(t)
	var v settingsView
	code, body := b.call("GET", "/api/settings", nil, false)
	if err := json.Unmarshal(body, &v); code != 200 || err != nil {
		t.Fatalf("get: %d %s", code, body)
	}
	if v.DeviceName != "bob" || v.DownloadDir != b.cfg.DownloadDir || !v.AutoAcceptTrusted || v.RestartRequired {
		t.Fatalf("initial settings: %+v", v)
	}

	newDL := filepath.Join(t.TempDir(), "inbox")
	code, body = b.call("PUT", "/api/settings", map[string]any{"download_dir": newDL, "auto_accept_trusted": false, "device_name": "bob's pc"}, true)
	if code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	_ = json.Unmarshal(body, &v)
	want := settingsView{DeviceName: "bob's pc", DownloadDir: newDL, AutoAcceptTrusted: false, RestartRequired: true}
	if v != want {
		t.Fatalf("after put: %+v, want %+v", v, want)
	}
	code, body = b.call("GET", "/api/settings", nil, false)
	_ = json.Unmarshal(body, &v)
	if code != 200 || v != want {
		t.Fatalf("get after put: %+v", v)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.DataDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	// A partial PUT keeps the other fields.
	code, body = b.call("PUT", "/api/settings", map[string]any{"device_name": ""}, true)
	_ = json.Unmarshal(body, &v)
	if code != 200 || v.DeviceName != "bob" || v.DownloadDir != newDL || v.AutoAcceptTrusted || v.RestartRequired {
		t.Fatalf("partial put: %d %+v", code, v)
	}
	for _, bad := range []map[string]any{{"download_dir": "relative"}, {"device_name": strings.Repeat("n", 65)}} {
		if code, _ := b.call("PUT", "/api/settings", bad, true); code != 400 {
			t.Errorf("PUT %v: %d, want 400", bad, code)
		}
	}
	if code, _ := b.call("PUT", "/api/settings", map[string]any{"auto_accept_trusted": false}, false); code != 403 {
		t.Fatalf("PUT without CSRF header: %d", code)
	}

	// Alice is trusted, but auto-accept is off: she gets a prompt.
	src := writeTemp(t, "doc.txt", []byte("12345"))
	if code, body := a.call("POST", "/api/send", map[string]any{"peer": b.addr(), "paths": []string{src}}, true); code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	o := b.waitIncoming(1)[0]
	if code, body := b.call("POST", "/api/accept", map[string]any{"id": o.ID, "accept": true}, true); code != 200 {
		t.Fatalf("accept: %d %s", code, body)
	}
	waitFile(t, filepath.Join(newDL, "doc.txt"), 5)
}

// TestTrustedListAndRevoke covers GET /api/trusted and POST
// /api/trusted/revoke; a revoked sender is prompted again.
func TestTrustedListAndRevoke(t *testing.T) {
	a, b := trustedPair(t)
	code, body := b.call("GET", "/api/trusted", nil, false)
	var list []struct {
		Fingerprint string `json:"fingerprint"`
		Name        string `json:"name"`
		Pub         string `json:"pub"`
	}
	if err := json.Unmarshal(body, &list); code != 200 || err != nil || len(list) != 1 || list[0].Fingerprint != a.id.Fingerprint() || list[0].Pub != a.id.PublicKeyB64() {
		t.Fatalf("trusted: %d %s", code, body)
	}
	if code, _ := b.call("POST", "/api/trusted/revoke", map[string]any{"fingerprint": "0000000000000000"}, true); code != 404 {
		t.Fatalf("revoke unknown: %d", code)
	}
	if code, body := b.call("POST", "/api/trusted/revoke", map[string]any{"fingerprint": a.id.Fingerprint()}, true); code != 200 {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if b.srv.trust.IsTrusted(a.id.Fingerprint(), a.id.PublicKeyB64()) {
		t.Fatal("still trusted after revoke")
	}
	code, body = b.call("GET", "/api/trusted", nil, false)
	if code != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("trusted after revoke: %d %s", code, body)
	}
}
