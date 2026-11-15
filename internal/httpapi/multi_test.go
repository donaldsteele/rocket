package httpapi

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// snapshot maps every file and folder under root (relative "/" path, folders
// with a trailing "/") to its content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, want, got map[string]string) {
	t.Helper()
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		w, wok := want[k]
		g, gok := got[k]
		if !wok || !gok || w != g {
			t.Errorf("%q: want present=%v (%d bytes), got present=%v (%d bytes)", k, wok, len(w), gok, len(g))
		}
	}
}

// TestTwoServerFolderTransfer sends a nested folder (an empty folder, unicode
// names, a 0-byte file) plus a loose file between two hosts in one offer with
// one accept, and compares every byte on arrival.
func TestTwoServerFolderTransfer(t *testing.T) {
	a, b := newTestHost(t, "alice"), newTestHost(t, "bob")
	events := b.srv.hub.subscribe()
	defer b.srv.hub.unsubscribe(events)
	aEvents := a.srv.hub.subscribe()
	defer a.srv.hub.unsubscribe(aEvents)

	big := make([]byte, 700<<10)
	_, _ = rand.Read(big)
	src := t.TempDir()
	folder := filepath.Join(src, "Holiday 2024")
	files := map[string]string{
		"big.bin":               string(big),
		"zero.txt":              "",
		"ünïcødé 文件.txt":        "unicode name",
		"sub/deeper/notes.md":   "# notes\n",
		"sub/deeper/日本語/写真.jpg": "jpeg-ish",
		"sub/very long name with spaces and more spaces and more spaces and more spaces and still more spaces.txt": "long",
	}
	for rel, data := range files {
		p := filepath.Join(folder, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(folder, "empty", "also empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	loose := writeTemp(t, "loose.txt", []byte("loose file"))

	code, body := a.call("POST", "/api/send", map[string]any{"peer": b.addr(), "paths": []string{folder, loose}}, true)
	if code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}

	o := b.waitIncoming(1)[0]
	if o.Kind != protocol.KindMulti || o.Files != len(files)+1 || o.Name != "Holiday 2024" || len(o.Names) == 0 {
		t.Fatalf("offer view: %+v", o)
	}
	if want := int64(len(big) + len("unicode name") + len("# notes\n") + len("jpeg-ish") + len("long") + len("loose file")); o.Size != want {
		t.Fatalf("offer size %d, want %d", o.Size, want)
	}
	if code, body := b.call("POST", "/api/accept", map[string]any{"id": o.ID, "accept": true}, true); code != 200 {
		t.Fatalf("accept: %d %s", code, body)
	}

	done := waitEventData(t, events, "done", o.TransferID)
	if done["files"] != len(files)+1 {
		t.Fatalf("done event: %v", done)
	}
	sentEv := waitEventData(t, aEvents, "sent", "")
	if sentEv["files"] != len(files)+1 {
		t.Fatalf("sent event: %v", sentEv)
	}

	sameTree(t, snapshot(t, folder), snapshot(t, filepath.Join(b.cfg.DownloadDir, "Holiday 2024")))
	if got, _ := os.ReadFile(filepath.Join(b.cfg.DownloadDir, "loose.txt")); string(got) != "loose file" {
		t.Fatalf("loose file: %q", got)
	}
	for k := range snapshot(t, b.cfg.DownloadDir) {
		if strings.HasSuffix(k, transfer.PartSuffix) {
			t.Fatalf("part file left: %s", k)
		}
	}
}

// TestFolderProgressEvents checks per-file progress fields on both ends.
func TestFolderProgressEvents(t *testing.T) {
	a, b := trustedPair(t)
	events := b.srv.hub.subscribe()
	defer b.srv.hub.unsubscribe(events)
	src := filepath.Join(t.TempDir(), "d")
	_ = os.MkdirAll(src, 0o755)
	for _, n := range []string{"1.txt", "2.txt", "3.txt"} {
		_ = os.WriteFile(filepath.Join(src, n), bytes.Repeat([]byte(n), 1000), 0o644)
	}
	code, body := a.call("POST", "/api/send", map[string]any{"peer": b.addr(), "paths": []string{src}}, true)
	if code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	var tid string
	var last map[string]any
	timeout := time.After(10 * time.Second)
	for tid == "" || last == nil {
		select {
		case ev := <-events:
			m, _ := ev.Data.(map[string]any)
			switch ev.Type {
			case "progress":
				last = m
			case "done":
				tid, _ = m["transfer_id"].(string)
			}
		case <-timeout:
			t.Fatal("no progress/done events")
		}
	}
	if last["name"] != "d" || last["files"] != 3 || last["file_index"] != 3 || last["file"] != "d/3.txt" || last["sent"] != int64(3*5000) {
		t.Fatalf("final progress event: %v", last)
	}
	if last["transfer_id"] != tid {
		t.Fatalf("progress transfer_id %v, done %v", last["transfer_id"], tid)
	}
}

// TestRelayMultiFile drives a browser-style relay: one multipart body (the
// manifest field, then files with folder-relative names) on Alice's loopback
// API, streamed on to Bob as a tar without staging.
func TestRelayMultiFile(t *testing.T) {
	a, b := trustedPair(t)
	files := []struct{ name, data string }{
		{"album/a.txt", "first"}, {"album/inner/b.txt", "second"}, {"album/zero", ""},
	}
	var list []transfer.BrowserFile
	for _, f := range files {
		list = append(list, transfer.BrowserFile{Name: f.name, Size: int64(len(f.data))})
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mf, _ := mw.CreateFormField("manifest")
	_ = json.NewEncoder(mf).Encode(map[string]any{"files": list})
	for _, f := range files {
		w, _ := mw.CreateFormFile("file", f.name)
		_, _ = io.WriteString(w, f.data)
	}
	_ = mw.Close()

	req, _ := http.NewRequest("POST", a.ts.URL+"/api/relay?peer="+b.addr(), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(CSRFHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("relay: %d %s", resp.StatusCode, msg)
	}
	sameTree(t, map[string]string{"a.txt": "first", "inner/": "", "inner/b.txt": "second", "zero": ""},
		snapshot(t, filepath.Join(b.cfg.DownloadDir, "album")))
}

// TestDropMultiFileSingleOffer covers several files in one browser drop with
// repeated names, which are numbered rather than rejected.
func TestDropMultiFileSingleOffer(t *testing.T) {
	b := newTestHost(t, "bob")
	files := []dropFile{{Name: "x.txt", Size: 1}, {Name: "y.txt", Size: 2}, {Name: "x.txt", Size: 3}}
	// From this computer's own UI (CSRF header) the prompt is skipped.
	code, body := b.call("POST", "/drop/offer", map[string]any{"files": files}, true)
	var reply protocol.OfferReply
	_ = json.Unmarshal(body, &reply)
	if code != 200 || !reply.Accepted {
		t.Fatalf("drop offer: %d %s", code, body)
	}
	code, msg := dropUpload(t, http.DefaultClient, b, reply.Token, []part{{"x.txt", "1"}, {"y.txt", "22"}, {"x.txt", "333"}})
	if code != 200 {
		t.Fatalf("drop upload: %d %s", code, msg)
	}
	sameTree(t, map[string]string{"x.txt": "1", "y.txt": "22", "x (1).txt": "333"}, snapshot(t, b.cfg.DownloadDir))
}

// waitEventData waits for an event of type typ (and transfer_id, unless
// empty) and returns its data.
func waitEventData(t *testing.T, ch chan Event, typ, transferID string) map[string]any {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev := <-ch:
			m, _ := ev.Data.(map[string]any)
			if ev.Type == "error" {
				t.Fatalf("error event: %v", m)
			}
			if ev.Type == typ && (transferID == "" || m["transfer_id"] == transferID) {
				return m
			}
		case <-timeout:
			t.Fatalf("no %q event for %s", typ, transferID)
		}
	}
}

// TestRelayAbortMidStream: the second relayed part is not the announced file,
// so the sender aborts mid-body; the relay reports the real cause and the
// receiver removes what it had written. The first file is larger than one
// encrypted frame (64 KiB), so the receiver is already writing when the
// body stops.
func TestRelayAbortMidStream(t *testing.T) {
	a, b := trustedPair(t)
	events := b.srv.hub.subscribe()
	defer b.srv.hub.unsubscribe(events)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mf, _ := mw.CreateFormField("manifest")
	_ = json.NewEncoder(mf).Encode(map[string]any{"files": []transfer.BrowserFile{
		{Name: "set/one.txt", Size: 100 << 10}, {Name: "set/two.txt", Size: 3},
	}})
	w, _ := mw.CreateFormFile("file", "set/one.txt")
	_, _ = w.Write(bytes.Repeat([]byte("1"), 100<<10))
	w, _ = mw.CreateFormFile("file", "set/evil.exe")
	_, _ = io.WriteString(w, "222")
	_ = mw.Close()

	req, _ := http.NewRequest("POST", a.ts.URL+"/api/relay?peer="+b.addr(), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(CSRFHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(msg), "two.txt") {
		t.Fatalf("relay with a wrong part: %d %s", resp.StatusCode, msg)
	}
	// The receiver sees the body cut off when the sender drops the
	// connection, which it cannot tell from the sender cancelling: it
	// reports the transfer cancelled by the peer and cleans up. (Under
	// protocol 1 the tar reader hid the cause and reported an error.)
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == "error" {
				t.Fatalf("error event %v", ev.Data)
			}
			if ev.Type != "cancelled" {
				continue
			}
			if m, _ := ev.Data.(map[string]any); m["by"] != "peer" || m["dir"] != "in" {
				t.Fatalf("cancelled event %v", m)
			}
			if d := b.downloads(); len(d) != 0 {
				t.Fatalf("left in downloads: %v", d)
			}
			return
		case <-timeout:
			t.Fatal("receiver reported nothing")
		}
	}
}
