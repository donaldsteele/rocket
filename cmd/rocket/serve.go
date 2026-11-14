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
	cfg   *config.Config
	id    *node.Identity
	trust *peers.Store
	reg   *discovery.Registry
}

// setup loads config (with flag overrides), identity, trust store and the
// discovery registry.
func setup(cf *commonFlags) (*runtimeState, error) {
	cfg, err := config.Load(cf.overrides())
	if err != nil {
		return nil, err
	}
	id, err := node.Load(cfg.DataDir, cfg.Name)
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
	return &runtimeState{cfg: cfg, id: id, trust: trust, reg: reg}, nil
}

// serve starts discovery and the HTTP server, blocking until the UI quits, a
// signal arrives, or the context is cancelled. openBrowser controls whether the
// local UI is opened.
func (rs *runtimeState) serve(openBrowser bool) int {
	srv, err := httpapi.New(rs.cfg, rs.id, rs.trust, rs.reg, web.FS())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	srv.Version, srv.OS = buildinfo.Version, runtime.GOOS

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	addr := fmt.Sprintf(":%d", rs.cfg.HTTPPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}

	go func() {
		if err := rs.reg.Run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "discovery:", err)
		}
	}()

	fmt.Printf("rocket %s\n", buildinfo.Version)
	fmt.Printf("  name:        %s\n", rs.id.Name)
	fmt.Printf("  fingerprint: %s\n", rs.id.Fingerprint())
	fmt.Printf("  received to: %s\n", rs.cfg.DownloadDir)
	fmt.Printf("  open:        http://127.0.0.1:%d/\n", rs.cfg.HTTPPort)
	for _, u := range lanURLs(rs.cfg.HTTPPort) {
		fmt.Printf("  drop page:   %s\n", u)
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
	case <-srv.Quit():
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = hs.Shutdown(shutCtx)
	return 0
}

// lanURLs lists the addresses other devices can open the drop page on.
func lanURLs(port int) []string {
	var urls []string
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
