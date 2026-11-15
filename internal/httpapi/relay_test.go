package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// pageFile is one file the page holds: its name as the browser gives it
// (webkitRelativePath for a folder pick) and its content.
type pageFile struct {
	name string
	data []byte
}

// relayPage plays the browser page (web/app.js) sending picked files: it
// holds their content and drives /api/relay/offer, /upload and /result the
// way the page does, recording what it did.
type relayPage struct {
	t     *testing.T
	h     *testHost
	files []pageFile
	// dropAt makes the next upload stop after that many content bytes, as
	// when the tab is closed or reloaded (-1: never).
	dropAt int64
	// stay makes the page stop after an interrupted answer instead of
	// offering again (it went away between requests).
	stay bool

	id      string
	answers []relayAnswer
	froms   []int   // the first file of every upload
	upBytes []int64 // content bytes of every upload
}

var errTabClosed = errors.New("the test page went away")

func newPage(t *testing.T, h *testHost, files []pageFile) *relayPage {
	return &relayPage{t: t, h: h, files: files, dropAt: -1}
}

func (pg *relayPage) list() []transfer.BrowserFile {
	out := make([]transfer.BrowserFile, len(pg.files))
	for i, f := range pg.files {
		out[i] = transfer.BrowserFile{Name: f.name, Size: int64(len(f.data))}
	}
	return out
}

// manifest is the manifest the server builds from the page's list.
func (pg *relayPage) manifest() protocol.Manifest {
	m, err := transfer.BrowserManifest(pg.list(), true)
	if err != nil {
		pg.t.Fatal(err)
	}
	return m
}

// offer calls POST /api/relay/offer.
func (pg *relayPage) offer(body map[string]any) (int, relayAnswer, string) {
	pg.t.Helper()
	code, b := pg.h.call("POST", "/api/relay/offer", body, true)
	var a relayAnswer
	if code == 200 {
		if err := json.Unmarshal(b, &a); err != nil {
			pg.t.Fatalf("offer answer %s: %v", b, err)
		}
	}
	return code, a, string(b)
}

// upload makes upload seq: the files from file from on, as multipart. It
// returns the answer, from GET /api/relay/result when the upload request
// itself failed (the server may stop reading early). gone reports that the
// page dropped the upload (dropAt).
func (pg *relayPage) upload(seq, from int) (a relayAnswer, gone bool) {
	pg.t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	drop := pg.dropAt
	pg.dropAt = -1
	var n int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, f := range pg.files[from:] {
			w, err := mw.CreateFormFile("file", f.name)
			if err != nil {
				return
			}
			for off := 0; off < len(f.data); off += 32 << 10 {
				chunk := f.data[off:min(off+32<<10, len(f.data))]
				if drop >= 0 && n+int64(len(chunk)) > drop {
					pw.CloseWithError(errTabClosed)
					return
				}
				if _, err := w.Write(chunk); err != nil {
					return // the server stopped reading
				}
				n += int64(len(chunk))
			}
		}
		_ = mw.Close()
		_ = pw.Close()
	}()
	u := fmt.Sprintf("%s/api/relay/upload?transfer_id=%s&seq=%d", pg.h.ts.URL, pg.id, seq)
	req, _ := http.NewRequest("POST", u, pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(CSRFHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	pr.CloseWithError(io.ErrClosedPipe)
	<-done
	pg.froms = append(pg.froms, from)
	pg.upBytes = append(pg.upBytes, n)
	if drop >= 0 && err != nil {
		return relayAnswer{}, true
	}
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			pg.t.Fatalf("upload %d: %d %s", seq, resp.StatusCode, b)
		}
		if err := json.Unmarshal(b, &a); err == nil {
			pg.checkResult(seq, a)
			return a, false
		}
	}
	return pg.result(seq), false
}

// result reads GET /api/relay/result for upload seq.
func (pg *relayPage) result(seq int) relayAnswer {
	pg.t.Helper()
	for {
		code, b := pg.h.call("GET", fmt.Sprintf("/api/relay/result?transfer_id=%s&seq=%d", pg.id, seq), nil, false)
		var a relayAnswer
		if code != 200 || json.Unmarshal(b, &a) != nil {
			pg.t.Fatalf("result %d: %d %s", seq, code, b)
		}
		if a.Result != "pending" {
			return a
		}
	}
}

// checkResult: the result route answers what the upload answered.
func (pg *relayPage) checkResult(seq int, a relayAnswer) {
	pg.t.Helper()
	if got := pg.result(seq); got.Result != a.Result || got.Seq != a.Seq {
		pg.t.Fatalf("result route says %+v, the upload said %+v", got, a)
	}
}

// run drives a send from its first offer (body) to its end, like the page:
// upload when accepted (again on restart), offer again at once when
// interrupted and retryable (the server waits out the delay). It returns
// the last answer.
func (pg *relayPage) run(body map[string]any) relayAnswer {
	pg.t.Helper()
	code, a, msg := pg.offer(body)
	if code != 200 {
		pg.t.Fatalf("offer: %d %s", code, msg)
	}
	for {
		pg.answers = append(pg.answers, a)
		if a.TransferID != "" {
			pg.id = a.TransferID
		}
		switch a.Result {
		case "accepted", "restart":
			var gone bool
			if a, gone = pg.upload(a.Seq, a.From); gone {
				return relayAnswer{Result: "gone", TransferID: pg.id}
			}
		case string(history.ResultInterrupted):
			if !a.Retryable || pg.stay {
				return a
			}
			if code, a, msg = pg.offer(map[string]any{"transfer_id": pg.id}); code != 200 {
				pg.t.Fatalf("retry offer: %d %s", code, msg)
			}
		default:
			return a
		}
	}
}

// start is the first offer of a new send to peer.
func (pg *relayPage) start(peer string) map[string]any {
	return map[string]any{"peer": peer, "files": pg.list()}
}

// sawRetry checks that the send broke and retried: an interrupted,
// retryable answer for retry 1 of attempts.
func (pg *relayPage) sawRetry(attempts int) {
	pg.t.Helper()
	for _, a := range pg.answers {
		if a.Result == string(history.ResultInterrupted) && a.Retryable {
			if a.Attempt != 1 || a.Attempts != attempts || a.DelayMS <= 0 || a.Error == "" {
				pg.t.Fatalf("retry answer %+v", a)
			}
			return
		}
	}
	pg.t.Fatalf("no retry in %+v", pg.answers)
}

// TestRelayResumeMidFile drops the peer connection in the middle of a
// file picked in the page: the page offers again by itself, the receiver
// (which asked the person the first time) accepts the retry without a
// prompt and offers its resume point, and only the missing bytes cross the
// wire. The result has the source's SHA-256.
func TestRelayResumeMidFile(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	fastRetries(a, 5)
	data := randomBytes(1<<20 + 12345)
	const cut = 300 << 10
	p := newCutProxy(t, b.ts.URL, cut)
	pg := newPage(t, a, []pageFile{{"film.bin", data}})
	go func() {
		b.call("POST", "/api/accept", map[string]any{"id": b.waitIncoming(1)[0].ID, "accept": true}, true)
	}()
	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)

	if end := pg.run(pg.start(p.addr())); end.Result != "sent" {
		t.Fatalf("send ended with %+v (answers %+v)", end, pg.answers)
	}
	pg.sawRetry(5)
	retry := waitEventData(t, aEv, "retrying", pg.id)
	if retry["attempt"] != 1 || retry["attempts"] != 5 {
		t.Fatalf("retrying event %v", retry)
	}
	if got := sumFile(t, filepath.Join(b.cfg.DownloadDir, "film.bin")); got != sum(data) {
		t.Fatal("received file differs from the source")
	}
	checkOnlyMissingSent(t, p, pg.manifest(), cut, 0)
	if len(pg.froms) != 2 || pg.froms[1] != 0 {
		t.Fatalf("uploads began with files %v", pg.froms)
	}
	b.waitNoKept()
	if out, _ := a.resumable(); len(out) != 0 {
		t.Fatalf("sender kept %+v", out)
	}
	if h := a.history(""); len(h) != 1 || h[0].Result != history.ResultOK || h[0].ID != pg.id || !h[0].Encrypted {
		t.Fatalf("sender history %+v", h)
	}
	if n := len(a.transfers()); n != 0 {
		t.Fatalf("%d transfers still listed", n)
	}
}

// folderPage is a picked folder whose second file is large.
func folderPage(t *testing.T, a *testHost) *relayPage {
	return newPage(t, a, []pageFile{
		{"album/a.bin", randomBytes(100 << 10)},
		{"album/inner/b.bin", randomBytes(700 << 10)},
		{"album/inner/c.bin", randomBytes(50 << 10)},
		{"album/inner/zero.tx", nil},
	})
}

func entryOf(t *testing.T, m protocol.Manifest, path string) int {
	for i, e := range m.Entries {
		if e.Path == path {
			return i
		}
	}
	t.Fatalf("no entry %s", path)
	return -1
}

func pageTree(pg *relayPage, prefix string) map[string]string {
	want := map[string]string{}
	for _, f := range pg.files {
		rel := strings.TrimPrefix(f.name, prefix)
		want[rel] = string(f.data)
		if i := strings.LastIndex(rel, "/"); i > 0 {
			want[rel[:i+1]] = ""
		}
	}
	return want
}

// TestRelayResumeMidFolder: the same for a picked folder, cut in its
// second file. The page's second upload begins at that file (the first is
// not uploaded again), the wire carries only the missing bytes, and the
// tree arrives identical.
func TestRelayResumeMidFolder(t *testing.T) {
	a, b := trustedPair(t)
	fastRetries(a, 5)
	const cut = 400 << 10
	p := newCutProxy(t, b.ts.URL, cut)
	pg := folderPage(t, a)
	if end := pg.run(pg.start(p.addr())); end.Result != "sent" {
		t.Fatalf("send ended with %+v (answers %+v)", end, pg.answers)
	}
	pg.sawRetry(5)
	m := pg.manifest()
	checkOnlyMissingSent(t, p, m, cut, uint32(entryOf(t, m, "album/inner/b.bin")))
	if len(pg.froms) != 2 || pg.froms[1] != 1 {
		t.Fatalf("uploads began with files %v, want the second at file 1", pg.froms)
	}
	if want := int64(len(pg.files[1].data) + len(pg.files[2].data)); pg.upBytes[1] != want {
		t.Fatalf("the page uploaded %d bytes the second time, want %d (from b.bin on)", pg.upBytes[1], want)
	}
	sameTree(t, pageTree(pg, "album/"), snapshot(t, filepath.Join(b.cfg.DownloadDir, "album")))
	if len(b.downloads()) != 1 {
		t.Fatalf("downloads %v", b.downloads())
	}
	b.waitNoKept()
}

// interruptRelay sends the page's files from a to b through a proxy that
// drops the first upload after cut bytes, with no retries: the send ends
// interrupted, kept in a's outbox with the picked files, and b keeps what
// arrived.
func interruptRelay(t *testing.T, a, b *testHost, pg *relayPage, cut int64) *cutProxy {
	t.Helper()
	a.srv.retryDelays = []time.Duration{}
	p := newCutProxy(t, b.ts.URL, cut)
	end := pg.run(pg.start(p.addr()))
	if end.Result != string(history.ResultInterrupted) || end.Retryable {
		t.Fatalf("send ended with %+v", end)
	}
	b.waitKept(1)
	return p
}

// TestRelayPrefixMismatchRestarts: the file changed inside the part the
// receiver kept. The resumed upload's prefix does not hash to the
// receiver's resume point, so the server answers "restart", the page
// uploads everything, and the peer body starts at (0,0): the result is the
// new content, in one copy.
func TestRelayPrefixMismatchRestarts(t *testing.T) {
	a, b := trustedPair(t)
	pg := newPage(t, a, []pageFile{{"notes.bin", randomBytes(500 << 10)}})
	p := interruptRelay(t, a, b, pg, 200<<10)
	out, _ := a.resumable()
	if len(out) != 1 || out[0].ID != pg.id || len(out[0].Picked) != 1 || out[0].Picked[0].Name != "notes.bin" || out[0].RemoteID == "" {
		t.Fatalf("outbox %+v", out)
	}
	pg.files[0].data[1000] ^= 0xff

	old := pg.id
	pg.answers, pg.froms = nil, nil
	if end := pg.run(map[string]any{"resume": old, "files": pg.list()}); end.Result != "sent" {
		t.Fatalf("resume ended with %+v (answers %+v)", end, pg.answers)
	}
	if len(pg.answers) < 2 || pg.answers[0].Result != "accepted" || !pg.answers[0].Resume || pg.answers[1].Result != "restart" {
		t.Fatalf("answers %+v, want accepted with a resume point, then restart", pg.answers)
	}
	if pg.id == old {
		t.Fatal("the resumed send kept its old id")
	}
	m := pg.manifest()
	sent, lengths, resumes := p.uploads()
	if len(resumes) != 2 || resumes[1] == nil || len(sent) != 2 {
		t.Fatalf("uploads %v, replies %v", sent, resumes)
	}
	if full := transfer.SealedBodySize(m, transfer.Start{}); lengths[1] != full || sent[1] != full {
		t.Fatalf("second upload %d/%d bytes, want the whole body (%d)", sent[1], lengths[1], full)
	}
	sameTree(t, map[string]string{"notes.bin": string(pg.files[0].data)}, snapshot(t, b.cfg.DownloadDir))
	b.waitNoKept()
	if out, _ := a.resumable(); len(out) != 0 {
		t.Fatalf("still resumable %+v", out)
	}
}

// TestRelayResumeAfterReload: the tab is closed in the middle of the
// upload. The send ends interrupted at once (nobody holds the files to
// retry) and the receiver keeps what arrived. After the reload, Resume
// needs the same files again: a different pick is refused with a message
// that says what differs, POST /api/resume refuses (no paths), and the same
// pick sends only what is missing, to identical SHA-256.
func TestRelayResumeAfterReload(t *testing.T) {
	a, b := trustedPair(t)
	fastRetries(a, 5)
	p := newCutProxy(t, b.ts.URL)
	pg := folderPage(t, a)
	pg.dropAt = 400 << 10
	if end := pg.run(pg.start(p.addr())); end.Result != "gone" {
		t.Fatalf("send ended with %+v", end)
	}
	if e := a.waitHistory(pg.id); e.Result != history.ResultInterrupted {
		t.Fatalf("sender history %+v", e)
	}
	b.waitKept(1)
	out, _ := a.resumable()
	if len(out) != 1 || out[0].ID != pg.id || len(out[0].Picked) != 4 || len(out[0].Paths) != 0 {
		t.Fatalf("outbox %+v", out)
	}
	if sent, _, _ := p.uploads(); len(sent) != 1 {
		t.Fatalf("the server retried without the page: %v", sent)
	}

	id := pg.id
	list := pg.list()
	bad := map[string][]transfer.BrowserFile{
		"You picked 3 files": list[:3],
		"File 2 is":          {list[0], {Name: "album/inner/x.bin", Size: list[1].Size}, list[2], list[3]},
		"so it changed":      {list[0], {Name: list[1].Name, Size: list[1].Size + 1}, list[2], list[3]},
	}
	for want, files := range bad {
		if code, _, msg := pg.offer(map[string]any{"resume": id, "files": files}); code != http.StatusConflict || !strings.Contains(msg, want) {
			t.Errorf("resume with other files: %d %q, want 409 %q", code, msg, want)
		}
	}
	if code, b := a.call("POST", "/api/resume", map[string]any{"transfer_id": id}, true); code != http.StatusConflict || !strings.Contains(string(b), "pick") {
		t.Fatalf("POST /api/resume of a page send: %d %s", code, b)
	}
	if code, _, _ := pg.offer(map[string]any{"resume": strings.Repeat("0", 32), "files": list}); code != http.StatusNotFound {
		t.Fatalf("resume of nothing: %d", code)
	}
	if out, _ := a.resumable(); len(out) != 1 {
		t.Fatalf("a refused resume dropped the record: %+v", out)
	}

	if end := pg.run(map[string]any{"resume": id, "files": list}); end.Result != "sent" {
		t.Fatalf("resume ended with %+v (answers %+v)", end, pg.answers)
	}
	m := pg.manifest()
	sent, lengths, resumes := p.uploads()
	if len(resumes) != 2 || resumes[1] == nil || len(sent) != 2 {
		t.Fatalf("uploads %v, replies %v", sent, resumes)
	}
	start := transfer.Start{Entry: resumes[1].Entry, Offset: resumes[1].Offset}
	if want := transfer.SealedBodySize(m, start); lengths[1] != want || sent[1] != want || want >= transfer.SealedBodySize(m, transfer.Start{}) {
		t.Fatalf("second upload %d/%d bytes, want %d (from %+v)", sent[1], lengths[1], want, start)
	}
	if k := pg.froms[len(pg.froms)-1]; k == 0 && start.Entry > 1 {
		t.Fatalf("the page uploaded from file 0 for a resume at entry %d", start.Entry)
	}
	sameTree(t, pageTree(pg, "album/"), snapshot(t, filepath.Join(b.cfg.DownloadDir, "album")))
	b.waitNoKept()
	if out, _ := a.resumable(); len(out) != 0 {
		t.Fatalf("still resumable %+v", out)
	}
}

// TestRelayCancelDuringRetry: the person cancels while the page's send
// waits to reconnect, with the page's next offer waiting on the server or
// with no request of the page in flight. The send ends cancelled, the
// receiver is told and drops what it kept, and nothing is left to resume.
func TestRelayCancelDuringRetry(t *testing.T) {
	for _, stay := range []bool{false, true} {
		t.Run(map[bool]string{false: "offer waiting", true: "page idle"}[stay], func(t *testing.T) {
			a, b := trustedPair(t)
			a.srv.retryDelays = []time.Duration{time.Minute}
			p := newCutProxy(t, b.ts.URL, 300<<10)
			pg := newPage(t, a, []pageFile{{"big.bin", randomBytes(2 << 20)}})
			pg.stay = stay
			aEv := a.srv.hub.subscribe()
			defer a.srv.hub.unsubscribe(aEv)
			ended := make(chan relayAnswer, 1)
			go func() { ended <- pg.run(pg.start(p.addr())) }()

			ev := waitEventData(t, aEv, "retrying", "")
			id, _ := ev["transfer_id"].(string)
			if list := a.transfers(); len(list) != 1 || list[0].State != transfer.StateReconnecting || list[0].Attempt != 1 || list[0].Attempts != 1 {
				t.Fatalf("transfers while reconnecting %+v", list)
			}
			b.waitKept(1)
			if stay {
				if end := <-ended; !end.Retryable {
					t.Fatalf("page saw %+v", end)
				}
			}
			a.cancel(id)
			if typ, m := waitEnd(t, aEv, id); typ != "cancelled" {
				t.Fatalf("sender saw %s %v", typ, m)
			}
			if !stay {
				select {
				case end := <-ended:
					if end.Result != string(history.ResultCancelled) {
						t.Fatalf("page saw %+v", end)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the page's offer did not return")
				}
			}
			b.waitNoState()
			if out, _ := a.resumable(); len(out) != 0 {
				t.Fatalf("sender kept %v", out)
			}
			if e := a.waitHistory(id); e.Result != history.ResultCancelled {
				t.Fatalf("sender history %+v", e)
			}
			if sent, _, _ := p.uploads(); len(sent) != 1 {
				t.Fatalf("uploads after the cancel: %v", sent)
			}
			// The page's next request learns the end.
			if code, ans, msg := pg.offer(map[string]any{"transfer_id": id}); code != 200 || ans.Result != string(history.ResultCancelled) {
				t.Fatalf("offer after the cancel: %d %s", code, msg)
			}
		})
	}
}

// TestRelayPageGoneWhileReconnecting: the page does not come back after
// the retry delay (closed between requests). The server waits a while,
// then keeps the send for a resume.
func TestRelayPageGoneWhileReconnecting(t *testing.T) {
	a, b := trustedPair(t)
	fastRetries(a, 5)
	a.srv.relayWait = 300 * time.Millisecond
	p := newCutProxy(t, b.ts.URL, 200<<10)
	pg := newPage(t, a, []pageFile{{"clip.bin", randomBytes(600 << 10)}})
	pg.stay = true
	aEv := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEv)
	if end := pg.run(pg.start(p.addr())); !end.Retryable {
		t.Fatalf("page saw %+v", end)
	}
	if typ, m := waitEnd(t, aEv, pg.id); typ != "error" || m["result"] != history.ResultInterrupted {
		t.Fatalf("sender saw %s %v", typ, m)
	}
	if out, _ := a.resumable(); len(out) != 1 || out[0].ID != pg.id || len(out[0].Picked) != 1 {
		t.Fatalf("outbox %+v", out)
	}
	b.waitKept(1)
	// Discard tells the receiver, which drops what it kept.
	if code, body := a.call("POST", "/api/discard", map[string]any{"transfer_id": pg.id}, true); code != 200 {
		t.Fatalf("discard: %d %s", code, body)
	}
	b.waitNoState()
}

// TestRelayRoutesRefuseBadRequests: wrong peers, unknown sends and uploads
// out of turn are refused.
func TestRelayRoutesRefuseBadRequests(t *testing.T) {
	a, b := trustedPair(t)
	pg := newPage(t, a, []pageFile{{"x.txt", []byte("x")}})
	for _, peer := range []string{"bob", "abcd", ""} {
		if code, _, _ := pg.offer(map[string]any{"peer": peer, "files": pg.list()}); code != http.StatusBadRequest {
			t.Errorf("peer %q: %d, want 400", peer, code)
		}
	}
	if code, _, _ := pg.offer(map[string]any{"transfer_id": strings.Repeat("1", 32)}); code != http.StatusNotFound {
		t.Errorf("retry of nothing: %d", code)
	}
	if code, _ := a.call("POST", "/api/relay/offer", map[string]any{"peer": b.addr(), "files": pg.list()}, false); code != http.StatusForbidden {
		t.Errorf("offer without CSRF: %d", code)
	}
	code, ans, msg := pg.offer(pg.start(b.addr()))
	if code != 200 || ans.Result != "accepted" || ans.From != 0 || ans.Seq != 1 {
		t.Fatalf("offer: %d %s", code, msg)
	}
	pg.id = ans.TransferID
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/relay/upload?transfer_id=%s&seq=2", a.ts.URL, pg.id), bytes.NewReader(nil))
	req.Header.Set(CSRFHeader, "1")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("upload out of turn: %v %v", resp, err)
	}
	if res, gone := pg.upload(1, 0); gone || res.Result != "sent" {
		t.Fatalf("upload: %+v", res)
	}
	sameTree(t, map[string]string{"x.txt": "x"}, snapshot(t, b.cfg.DownloadDir))
}
