// Command rocket-tray is the desktop tray companion for rocket.
//
// It shows whether rocket is running on this computer, how many peers it can
// see and whether incoming offers are waiting, and it can open the rocket
// window, start rocket, or quit it. It talks only to rocket's local API on
// http://127.0.0.1:<port>; it never opens a port of its own.
//
// Usage:
//
//	rocket-tray [--port 29005]
//	rocket-tray --status        print one status line and exit (0 = running)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// version is set at build time with -ldflags "-X main.version=1.0.0".
var version = "dev"

// defaultPort is rocket's default HTTP port.
const defaultPort = 29005

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args and either prints one status line (--status) or runs the
// tray until the person picks "Exit tray".
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rocket-tray", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", portFromEnv(os.Getenv), "rocket's HTTP port on this computer (env ROCKET_PORT)")
	status := fs.Bool("status", false, "print rocket's status once and exit (exit code 0 when running)")
	showVersion := fs.Bool("version", false, "print the tray version and exit")
	if fs.Parse(args) != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "rocket-tray: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "rocket-tray %s\n", version)
		return 0
	}
	if *port <= 0 || *port > 65535 {
		fmt.Fprintf(stderr, "rocket-tray: invalid --port %d\n", *port)
		return 2
	}

	client := NewClient(*port)
	if *status {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		st := client.Status(ctx)
		fmt.Fprintln(stdout, viewFor(st, false, "").Tooltip)
		if !st.Running {
			return 1
		}
		return 0
	}

	runTray(newApp(client, *port))
	return 0
}

// portFromEnv returns ROCKET_PORT when it holds a valid port, else the default.
// It matches how rocket itself picks its port, so a tray started in the same
// environment finds the same rocket.
func portFromEnv(getenv func(string) string) int {
	if v := getenv("ROCKET_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			return p
		}
	}
	return defaultPort
}
