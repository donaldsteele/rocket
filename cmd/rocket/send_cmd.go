package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// runSend sends files and folders to a peer named on the command line, all
// in one transfer: a host:port, or a discovered peer's name or fingerprint
// prefix (it briefly listens for beacons to resolve those).
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
	plan, err := transfer.PlanPaths(fs.Args()[1:])
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
	label := target
	if transfer.IsHostPort(target) {
		t = transfer.Target{Addr: target}
	} else {
		go func() { _ = rs.reg.Run(ctx) }()
		peer, ok := resolvePeer(ctx, rs.reg, target, *wait)
		if !ok {
			fmt.Fprintf(os.Stderr, "peer %q not found on the network\n", target)
			return 1
		}
		t = transfer.Target{Addr: peer.Addr, FP: peer.Fingerprint}
		label = fmt.Sprintf("%s (%s)", peer.Name, peer.Addr)
	}

	what := transfer.Label(plan.Manifest)
	if plan.Manifest.Kind == protocol.KindMulti {
		what = fmt.Sprintf("%s (%d files, %d bytes)", what, transfer.Files(plan.Manifest), plan.Manifest.Total)
	}
	fmt.Printf("sending %s to %s; waiting for the receiver to accept...\n", what, label)
	err = transfer.SendPlan(ctx, rs.id, t, plan, func(p transfer.Progress) {
		line := fmt.Sprintf("  %3d%%  %d/%d bytes", pct(p.Sent, p.Total), p.Sent, p.Total)
		if p.Files > 1 {
			line += fmt.Sprintf("  file %d/%d %s", p.FileIndex, p.Files, p.File)
		}
		if r := []rune(line); len(r) > 79 {
			line = string(r[:76]) + "..."
		}
		fmt.Printf("\r%-79s", line)
	})
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("done")
	return 0
}

func resolvePeer(ctx context.Context, reg *discovery.Registry, target string, wait time.Duration) (discovery.Peer, bool) {
	deadline := time.Now().Add(wait)
	for {
		if p, ok := reg.Find(target); ok {
			return p, true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return discovery.Peer{}, false
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
