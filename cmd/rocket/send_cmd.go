package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/receiver"
	"github.com/donaldsteele/rocket/internal/resume"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// runSend sends files and folders to a peer named on the command line, all
// in one transfer: a host:port, or a discovered peer's name or fingerprint
// prefix (it briefly listens for beacons and mDNS to resolve those). When
// the connection breaks after the receiver accepted, it reconnects and
// continues where it stopped (up to 5 retries, shown as they happen); a send
// that gives up is kept for 24 hours like one from the UI, so the same
// command, or Resume in the UI, continues it.
func runSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	cf := addCommonFlags(fs)
	wait := fs.Duration("wait", 4*time.Second, "how long to look for a peer named by name or fingerprint")
	if fs.Parse(args) != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: rocket send [flags] <peer|host:port> <file|folder>...")
		return 2
	}
	target := fs.Arg(0)
	paths := absPaths(fs.Args()[1:])
	plan, err := transfer.PlanPaths(paths)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if n := len(plan.Skipped); n > 0 {
		fmt.Printf("note: skipped %d symlink(s) or special file(s)\n", n)
	}

	rs, err := setup(cf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	var t transfer.Target
	label, peerName := target, target
	if transfer.IsHostPort(target) {
		t = transfer.Target{Addr: target}
	} else {
		go func() { _ = rs.reg.Run(ctx) }()
		defer rs.startMDNS(ctx, false, io.Discard)()
		peer, err := resolvePeer(ctx, rs.reg, target, *wait)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		t = transfer.Target{Addr: peer.Addr, FP: peer.Fingerprint}
		label = fmt.Sprintf("%s (%s)", peer.Name, peer.Addr)
		peerName = peer.Name
	}

	what := transfer.Label(plan.Manifest)
	if plan.Manifest.Kind == protocol.KindMulti {
		what = fmt.Sprintf("%s (%d files, %d bytes)", what, transfer.Files(plan.Manifest), plan.Manifest.Total)
	}
	fmt.Printf("sending %s to %s, encrypted; waiting for the receiver to accept...\n", what, label)

	// The same books as a send from the UI: history, and the outbox of
	// interrupted sends, both in the data folder (best effort: a problem
	// with either never stops the send).
	hist, herr := history.Open(rs.cfg.DataDir)
	if herr != nil {
		hist = nil
	}
	outbox, oerr := resume.OpenOutbox(rs.cfg.DataDir)
	if oerr != nil {
		outbox = nil
	}
	out := &sendPrinter{w: os.Stdout}
	ledger := &receiver.Ledger{Transfers: transfer.NewManager(), History: hist, Events: out.event}
	tid := newTransferID()
	sctx := ledger.Transfers.Start(ctx, transfer.Info{
		ID: tid, Dir: history.DirOut, Name: transfer.Label(plan.Manifest), PeerName: peerName, PeerFP: t.FP,
		Files: transfer.Files(plan.Manifest), Total: plan.Manifest.Total, State: transfer.StateWaiting, Encrypted: true,
	}, nil)
	err = ledger.Send(sctx, rs.id, receiver.Outgoing{
		ID: tid, PeerName: peerName, Target: t, Manifest: plan.Manifest, Open: plan.Open,
		Resumable: true, Outbox: outbox, Paths: paths,
	})
	out.endLine()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, transfer.ErrInterrupted) {
			fmt.Fprintln(os.Stderr, "The receiver keeps what arrived for 24 hours. Run the same command again to continue where it stopped, or use Resume in rocket's Transfers view.")
		}
		return 1
	}
	fmt.Println("done")
	return 0
}

// absPaths makes the paths absolute, so that a send kept for a resume can
// be sent again from any working directory (the UI's Resume).
func absPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		if a, err := filepath.Abs(p); err == nil {
			out[i] = a
		} else {
			out[i] = p
		}
	}
	return out
}

func newTransferID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sendPrinter shows a send's events on a terminal: a progress line that
// redraws itself, and a line for every reconnect.
type sendPrinter struct {
	w           io.Writer
	mid         bool // a progress line is showing (no newline yet)
	reconnected bool // a retry was announced and no byte has moved since
}

func (p *sendPrinter) event(ev receiver.Event) {
	d, _ := ev.Data.(map[string]any)
	switch ev.Type {
	case "progress":
		if p.reconnected {
			p.reconnected = false
			p.endLine()
			fmt.Fprintln(p.w, "reconnected; continuing where it stopped")
		}
		line := fmt.Sprintf("  %3d%%  %d/%d bytes", pct(num(d["sent"]), num(d["total"])), num(d["sent"]), num(d["total"]))
		if files := num(d["files"]); files > 1 {
			file, _ := d["file"].(string)
			line += fmt.Sprintf("  file %d/%d %s", num(d["file_index"]), files, file)
		}
		if r := []rune(line); len(r) > 79 {
			line = string(r[:76]) + "..."
		}
		fmt.Fprintf(p.w, "\r%-79s", line)
		p.mid = true
	case "retrying":
		p.endLine()
		msg, _ := d["error"].(string)
		delay := time.Duration(num(d["delay_ms"])) * time.Millisecond
		fmt.Fprintf(p.w, "connection lost (%s); reconnecting %d/%d in %s...\n", msg, num(d["attempt"]), num(d["attempts"]), delay)
		p.reconnected = true
	}
}

// endLine ends a progress line that is showing.
func (p *sendPrinter) endLine() {
	if p.mid {
		fmt.Fprintln(p.w)
		p.mid = false
	}
}

// num reads an integer event field.
func num(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// resolvePeer waits up to wait for target to name exactly one discovered
// peer (discovery.Registry.Resolve). A target that matches several peers
// fails at once: picking one could send to the wrong host.
func resolvePeer(ctx context.Context, reg *discovery.Registry, target string, wait time.Duration) (discovery.Peer, error) {
	deadline := time.Now().Add(wait)
	for {
		p, err := reg.Resolve(target)
		if err == nil || errors.Is(err, discovery.ErrPeerAmbiguous) {
			return p, err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return discovery.Peer{}, err
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func pct(a, b int64) int {
	if b <= 0 {
		return 100
	}
	return int(a * 100 / b)
}
