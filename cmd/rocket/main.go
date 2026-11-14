// Command rocket is a LAN file transfer tool: discover peers on the local
// network and send files to them (or receive via a browser drop page), with
// trust-on-first-use approval.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/donaldsteele/rocket/internal/buildinfo"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rocket [ui | send <peer|host:port> <path>... | recv | mcp | version] [flags]")
	fmt.Fprintln(os.Stderr, "  ui       start the web UI (default) and announce this host")
	fmt.Fprintln(os.Stderr, "  send     send files and folders to a peer by name, fingerprint or host:port")
	fmt.Fprintln(os.Stderr, "  recv     run headless: announce and accept, no browser")
	fmt.Fprintln(os.Stderr, "  mcp      serve the MCP interface for an AI agent over stdio")
	fmt.Fprintln(os.Stderr, "  version  print the version")
	fmt.Fprintln(os.Stderr, "flags (ui, send, recv, mcp): --port, --beacon-port, --data-dir, --download-dir, --name")
	fmt.Fprintln(os.Stderr, "run 'rocket <command> -h' for the flags of one command")
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		return runUI(nil)
	}
	switch args[0] {
	case "ui":
		return runUI(args[1:])
	case "send":
		return runSend(args[1:])
	case "recv":
		return runRecv(args[1:])
	case "mcp":
		return runMCP(args[1:])
	case "version":
		fmt.Printf("rocket %s\n", buildinfo.Version)
		return 0
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		// Bare flags mean the default command: rocket --port 29100
		if strings.HasPrefix(args[0], "-") {
			return runUI(args)
		}
		usage()
		return 2
	}
}
