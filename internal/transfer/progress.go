package transfer

import (
	"io"
	"time"
)

// Progress reports how far a transfer has got.
type Progress struct {
	Sent      int64  // bytes so far, over the whole transfer
	Total     int64  // bytes in the whole transfer
	File      string // manifest path of the file in flight
	FileIndex int    // 1-based index of that file among the transfer's files
	Files     int    // number of files in the transfer (folders not counted)
}

// ProgressFn receives progress reports, throttled to about 20 a second, plus
// a final report when the last byte has moved.
type ProgressFn func(Progress)

// reportEvery throttles progress callbacks.
const reportEvery = 50 * time.Millisecond

// tracker turns byte counts and file starts into throttled ProgressFn calls.
// It is used from one goroutine at a time.
type tracker struct {
	fn   ProgressFn
	p    Progress
	last time.Time
	sent Progress // last state reported
}

func newTracker(fn ProgressFn, total int64, files int) *tracker {
	return &tracker{fn: fn, p: Progress{Total: total, Files: files}}
}

// file marks the start of the next file.
func (t *tracker) file(path string) {
	t.p.File = path
	t.p.FileIndex++
	t.report(false)
}

func (t *tracker) add(n int) {
	if n <= 0 {
		return
	}
	t.p.Sent += int64(n)
	t.report(t.p.Sent == t.p.Total)
}

// finish reports the final state unless it was just reported.
func (t *tracker) finish() { t.report(true) }

func (t *tracker) report(force bool) {
	if t.fn == nil || t.p == t.sent {
		return
	}
	if !force && time.Since(t.last) < reportEvery {
		return
	}
	t.last = time.Now()
	t.sent = t.p
	t.fn(t.p)
}

type trackWriter struct {
	w io.Writer
	t *tracker
}

func (w trackWriter) Write(b []byte) (int, error) {
	n, err := w.w.Write(b)
	w.t.add(n)
	return n, err
}
