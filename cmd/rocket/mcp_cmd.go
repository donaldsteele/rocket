package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/donaldsteele/rocket/internal/buildinfo"
	"github.com/donaldsteele/rocket/internal/mcpserver"
)

// nopWriteCloser keeps stdout open when the transport closes.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// runMCP serves the MCP interface over stdio. stdout carries only protocol
// frames; diagnostics go to stderr. It runs its own discovery so an agent can
// list peers and send files without a UI open.
func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	if fs.Parse(args) != nil {
		return 2
	}
	log.SetOutput(os.Stderr)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	rs, err := setup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { _ = rs.reg.Run(ctx) }()

	svc := &mcpserver.Service{ID: rs.id, Reg: rs.reg}
	srv := mcpserver.New(svc, mcpserver.Options{Version: buildinfo.Version, Logger: logger})

	transport := &mcp.IOTransport{Reader: io.NopCloser(os.Stdin), Writer: nopWriteCloser{os.Stdout}}
	if err := srv.Run(ctx, transport); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		return 1
	}
	return 0
}
