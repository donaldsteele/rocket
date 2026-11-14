package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/donaldsteele/rocket/internal/buildinfo"
	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/httpapi"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/web"
)

// runtimeState bundles everything a running rocket host needs.
type runtimeState struct {
	cfg *config.Config
	id  *node.Identity
	reg *discovery.Registry
	srv *httpapi.Server
}

// setup loads config, identity, trust store and the discovery registry.
func setup() (*runtimeState, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	id, err := node.Load(cfg.DataDir, "")
	if err != nil {
		return nil, err
	}
	trust, err := peers.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	reg := discovery.New(discovery.Self{
		Name: id.Name, Fingerprint: id.Fingerprint(),
		HTTPPort: cfg.HTTPPort, Version: buildinfo.Version, OS: runtime.GOOS,
	}, cfg.BeaconPort)
	srv := httpapi.New(cfg, id, trust, reg, web.FS())
	return &runtimeState{cfg: cfg, id: id, reg: reg, srv: srv}, nil
}

// serve starts discovery and the HTTP server, blocking until the UI quits, a
// signal arrives, or the context is cancelled. openBrowser controls whether the
// local UI is opened.
func (rs *runtimeState) serve(openBrowser bool) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	go func() {
		if err := rs.reg.Run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "discovery:", err)
		}
	}()

	addr := fmt.Sprintf(":%d", rs.cfg.HTTPPort)
	hs := &http.Server{Addr: addr, Handler: rs.srv.Handler()}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}

	fmt.Printf("rocket %s\n", buildinfo.Version)
	fmt.Printf("  name:        %s\n", rs.id.Name)
	fmt.Printf("  fingerprint: %s\n", rs.id.Fingerprint())
	fmt.Printf("  received to: %s\n", rs.cfg.DownloadDir)
	for _, u := range localURLs(rs.cfg.HTTPPort) {
		fmt.Printf("  open:        %s\n", u)
	}

	go func() {
		if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "http:", err)
		}
	}()

	if openBrowser {
		openInBrowser(fmt.Sprintf("http://127.0.0.1:%d/", rs.cfg.HTTPPort))
	}

	select {
	case <-ctx.Done():
	case <-rs.srv.Quit():
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = hs.Shutdown(shutCtx)
	return 0
}

// localURLs lists the addresses the UI can be opened on.
func localURLs(port int) []string {
	urls := []string{fmt.Sprintf("http://127.0.0.1:%d/", port)}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip4 := ipn.IP.To4(); ip4 != nil {
					urls = append(urls, fmt.Sprintf("http://%s:%d/", ip4, port))
				}
			}
		}
	}
	return urls
}
