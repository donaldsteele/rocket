package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/donaldsteele/rocket/internal/receiver"
)

// TestSendPrinterShowsRetries: rocket send shows each reconnect on its own
// line, then that the transfer continued.
func TestSendPrinterShowsRetries(t *testing.T) {
	var b strings.Builder
	p := &sendPrinter{w: &b}
	progress := func(sent int64) receiver.Event {
		return receiver.Event{Type: "progress", Data: map[string]any{
			"transfer_id": "x", "name": "photos", "sent": sent, "total": int64(1000), "dir": "out",
			"file": "photos/a.jpg", "file_index": 2, "files": 3,
		}}
	}
	p.event(progress(400))
	p.event(receiver.Event{Type: "retrying", Data: map[string]any{
		"transfer_id": "x", "name": "photos", "dir": "out", "attempt": 1, "attempts": 5,
		"delay_ms": int64(2000), "error": "connection reset",
	}})
	p.event(progress(700))
	p.event(receiver.Event{Type: "sent", Data: map[string]any{"transfer_id": "x"}})
	p.endLine()
	got := b.String()
	for _, want := range []string{
		"  40%  400/1000 bytes  file 2/3 photos/a.jpg",
		"\nconnection lost (connection reset); reconnecting 1/5 in 2s...\n",
		"reconnected; continuing where it stopped\n",
		"  70%  700/1000 bytes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%q", want, got)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("progress line not ended: %q", got)
	}
	if strings.Count(got, "reconnected") != 1 {
		t.Errorf("reconnected shown more than once: %q", got)
	}
}

func TestAbsPaths(t *testing.T) {
	got := absPaths([]string{"a.txt", filepath.Join("dir", "b")})
	for _, p := range got {
		if !filepath.IsAbs(p) {
			t.Errorf("%q is not absolute", p)
		}
	}
}
