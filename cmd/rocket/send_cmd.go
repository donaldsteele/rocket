package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// runSend sends a file to a peer named on the command line. It briefly listens
// for beacons to resolve the peer by name or fingerprint.
func runSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	wait := fs.Duration("wait", 4*time.Second, "how long to look for the peer")
	if fs.Parse(args) != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: rocket send <peer-name-or-fingerprint> <file>")
		return 2
	}
	target, path := fs.Arg(0), fs.Arg(1)
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	rs, err := setup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Minute)
	defer cancel()
	go func() { _ = rs.reg.Run(ctx) }()

	peer, ok := resolvePeer(rs.reg, target, *wait)
	if !ok {
		fmt.Fprintf(os.Stderr, "peer %q not found on the network\n", target)
		return 1
	}

	fmt.Printf("sending %s to %s (%s)...\n", filepath.Base(path), peer.Name, peer.Addr)
	offer := transfer.Offer{SenderName: rs.id.Name, SenderFP: rs.id.Fingerprint()}
	err = transfer.Send(ctx, peer.Addr, offer, path, func(sent, total int64) {
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

func resolvePeer(reg *discovery.Registry, target string, wait time.Duration) (discovery.Peer, bool) {
	deadline := time.Now().Add(wait)
	for {
		for _, p := range reg.Peers() {
			if strings.EqualFold(p.Name, target) || strings.HasPrefix(p.Fingerprint, strings.ToLower(target)) {
				return p, true
			}
		}
		if time.Now().After(deadline) {
			return discovery.Peer{}, false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func pct(a, b int64) int {
	if b <= 0 {
		return 0
	}
	return int(a * 100 / b)
}
