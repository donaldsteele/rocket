// Command rocket is a LAN file transfer tool: discover peers on the local
// network and send files to them (or receive via a browser drop page), with
// trust-on-first-use approval.
package main

import (
	"fmt"
	"os"

	"github.com/donaldsteele/rocket/internal/buildinfo"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rocket [ui | send <peer> <file> | recv | mcp | version]")
	fmt.Fprintln(os.Stderr, "  ui       start the web UI (default) and announce this host")
	fmt.Fprintln(os.Stderr, "  send     send a file to a discovered peer by name or fingerprint")
	fmt.Fprintln(os.Stderr, "  recv     run headless: announce and accept, no browser")
	fmt.Fprintln(os.Stderr, "  mcp      serve the MCP interface for an AI agent over stdio")
	fmt.Fprintln(os.Stderr, "  version  print the version")
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
		usage()
		return 2
	}
}
