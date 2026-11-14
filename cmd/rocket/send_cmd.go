package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// runSend sends a file to a peer named on the command line: a host:port, or a
// discovered peer's name or fingerprint prefix (it briefly listens for
// beacons to resolve those).
func runSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	cf := addCommonFlags(fs)
	wait := fs.Duration("wait", 4*time.Second, "how long to look for a peer named by name or fingerprint")
	if fs.Parse(args) != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: rocket send [flags] <peer|host:port> <file>")
		return 2
	}
	if fs.NArg() > 2 {
		fmt.Fprintln(os.Stderr, "error: this version sends one file at a time")
		return 2
	}
	target, path := fs.Arg(0), fs.Arg(1)
	if st, err := os.Stat(path); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	} else if !st.Mode().IsRegular() {
		fmt.Fprintf(os.Stderr, "error: %s is not a regular file\n", path)
		return 1
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

	fmt.Printf("sending %s to %s; waiting for the receiver to accept...\n", filepath.Base(path), label)
	err = transfer.Send(ctx, rs.id, t, path, func(sent, total int64) {
		fmt.Printf("\r  %3d%%  %d/%d bytes", pct(sent, total), sent, total)
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
