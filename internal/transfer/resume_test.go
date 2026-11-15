package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/protocol/securestream"
)

func hexSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// resumeManifest: d/, d/a.txt (5), d/b.txt (10), d/e/, c.txt (3).
func resumeManifest() protocol.Manifest {
	return protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d", Mode: protocol.ModeDir | 0o755},
		{Path: "d/a.txt", Size: 5, Mode: 0o644},
		{Path: "d/b.txt", Size: 10, Mode: 0o644},
		{Path: "d/e", Mode: protocol.ModeDir | 0o755},
		{Path: "c.txt", Size: 3, Mode: 0o644},
	}, Total: 18}
}

var errNet = fmt.Errorf("%w: test network", ErrConnLost)

// TestExtractorKeepAndResume: a write cut in the middle of a file keeps the
// folder, the finished file and the part file; the resumed extractor takes
// the rest of the bytes and produces the whole tree.
func TestExtractorKeepAndResume(t *testing.T) {
	dl := t.TempDir()
	m := resumeManifest()
	x := NewExtractor(dl, m, nil)
	if err := x.Write(0, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if err := x.Write(1, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	cutAfter4 := io.MultiReader(strings.NewReader("0123"), iotest.ErrReader(errNet))
	if err := x.Write(2, cutAfter4); !errors.Is(err, errNet) {
		t.Fatalf("cut write: %v", err)
	}
	st, pt, err := x.Keep()
	if err != nil {
		t.Fatal(err)
	}
	if st.Next != 2 || st.Offset != 4 || st.Sent != 9 || st.Files != 1 || st.Part == "" {
		t.Fatalf("state %+v", st)
	}
	if pt != (protocol.ResumePoint{Entry: 2, Offset: 4, SHA256: hexSum("0123")}) {
		t.Fatalf("point %+v", pt)
	}
	x.Abort() // after Keep, Abort must not remove what was kept
	if err := CheckExtractorState(st); err != nil {
		t.Fatal(err)
	}

	var last Progress
	y, err := ResumeExtractor(st, m, func(p Progress) { last = p })
	if err != nil {
		t.Fatal(err)
	}
	res, err := ReceiveFrom(y, m, st.Next, Start{Entry: 2, Offset: 4}, strings.NewReader("456789xyz"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Bytes != 18 || last.Sent != 18 || last.FileIndex != 3 {
		t.Fatalf("result %+v, progress %+v", res, last)
	}
	got := map[string]string{}
	for _, p := range []string{"d/a.txt", "d/b.txt", "c.txt"} {
		b, err := os.ReadFile(filepath.Join(dl, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		got[p] = string(b)
	}
	if got["d/a.txt"] != "hello" || got["d/b.txt"] != "0123456789" || got["c.txt"] != "xyz" {
		t.Fatalf("tree %v", got)
	}
	if fi, err := os.Stat(filepath.Join(dl, "d", "e")); err != nil || !fi.IsDir() {
		t.Fatalf("folder d/e: %v", err)
	}
	assertNoPartsTree(t, dl)
}

// TestExtractorKeepPoints: the resume point between entries is the next
// file at 0; after the last file it is that file at its end.
func TestExtractorKeepPoints(t *testing.T) {
	m := resumeManifest()
	dl := t.TempDir()
	x := NewExtractor(dl, m, nil)
	for i, s := range []string{"", "hello", "0123456789"} {
		if err := x.Write(i, strings.NewReader(s)); err != nil {
			t.Fatal(err)
		}
	}
	_, pt, err := x.Keep()
	if err != nil || pt != (protocol.ResumePoint{Entry: 4, Offset: 0, SHA256: hexSum("")}) {
		t.Fatalf("between entries: %+v %v", pt, err)
	}

	dl = t.TempDir()
	x = NewExtractor(dl, m, nil)
	for i, s := range []string{"", "hello", "0123456789", "", "xyz"} {
		if err := x.Write(i, strings.NewReader(s)); err != nil {
			t.Fatal(err)
		}
	}
	st, pt, err := x.Keep()
	if err != nil || pt != (protocol.ResumePoint{Entry: 4, Offset: 3, SHA256: hexSum("xyz")}) || st.Next != 5 {
		t.Fatalf("after the last file: %+v %+v %v", pt, st, err)
	}
	// Continuing there takes no bytes and only checks the end.
	y, err := ResumeExtractor(st, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := ReceiveFrom(y, m, st.Next, Start{Entry: 4, Offset: 3}, strings.NewReader("")); err != nil || res.Files != 3 {
		t.Fatalf("finish: %+v %v", res, err)
	}
}

// TestReceiveFromStartChecks: the start must fit what the writer holds.
func TestReceiveFromStartChecks(t *testing.T) {
	m := resumeManifest()
	for _, c := range []struct {
		from int
		s    Start
	}{
		{2, Start{Entry: 3}},             // a folder
		{2, Start{Entry: 4}},             // skips file 2
		{2, Start{Entry: 2, Offset: 11}}, // past the end of the file
		{3, Start{Entry: 2, Offset: 4}},  // behind from, not at the end
		{9, Start{}},
	} {
		x := NewExtractor(t.TempDir(), m, nil)
		if _, err := ReceiveFrom(x, m, c.from, c.s, strings.NewReader("")); !errors.Is(err, ErrStart) {
			t.Errorf("from %d start %+v: %v", c.from, c.s, err)
		}
	}
}

// TestKeptStateChecks: a state whose files are gone, or that names paths
// outside its folder, is refused; Discard removes everything it names.
func TestKeptStateChecks(t *testing.T) {
	dl := t.TempDir()
	m := resumeManifest()
	x := NewExtractor(dl, m, nil)
	_ = x.Write(0, strings.NewReader(""))
	_ = x.Write(1, strings.NewReader("hello"))
	_ = x.Write(2, io.MultiReader(strings.NewReader("01"), iotest.ErrReader(errNet)))
	st, _, err := x.Keep()
	if err != nil {
		t.Fatal(err)
	}
	outside := st
	outside.Created = append([]string{filepath.Join(t.TempDir(), "elsewhere")}, st.Created...)
	if err := CheckExtractorState(outside); err == nil {
		t.Fatal("a path outside the folder passed")
	}
	shorter := st
	shorter.Offset = 3
	if err := CheckExtractorState(shorter); err == nil {
		t.Fatal("a part file shorter than the offset passed")
	}
	DiscardExtractor(outside) // must not touch the outside path; removes the rest
	if left, _ := os.ReadDir(dl); len(left) != 0 {
		t.Fatalf("left after discard: %v", left)
	}
	if err := CheckExtractorState(st); err == nil {
		t.Fatal("a discarded state passed")
	}
	if _, err := ResumeExtractor(st, m, nil); err == nil {
		t.Fatal("resumed a discarded state")
	}
}

func TestIsNetwork(t *testing.T) {
	yes := []error{
		ErrConnLost, ErrInterrupted, io.ErrUnexpectedEOF, io.EOF,
		fmt.Errorf("x: %w", securestream.ErrTruncated),
		&net.OpError{Op: "read", Err: syscall.ECONNRESET},
		&url.Error{Op: "Post", URL: "http://x", Err: io.EOF},
		&url.Error{Op: "Post", URL: "http://x", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}},
		os.ErrDeadlineExceeded,
	}
	no := []error{
		nil, ErrCancelled, context.Canceled, context.DeadlineExceeded, ErrSize, ErrStart, ErrDeclined,
		securestream.ErrAuth, os.ErrNotExist,
		fmt.Errorf("%w by the receiver", ErrCancelled),
		&url.Error{Op: "Post", URL: "http://x", Err: context.Canceled},
		&url.Error{Op: "Post", URL: "http://x", Err: errors.New("http: ContentLength=5 with Body length 3")},
	}
	for _, err := range yes {
		if !IsNetwork(err) {
			t.Errorf("IsNetwork(%v) = false", err)
		}
	}
	for _, err := range no {
		if IsNetwork(err) {
			t.Errorf("IsNetwork(%v) = true", err)
		}
	}
}
