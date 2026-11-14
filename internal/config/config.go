// Package config resolves the per-user data directory, the folder received
// files are written to, and the default ports.
package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// Default ports. The HTTP port serves the UI, the transfer API and the drop
// page. The beacon port is the UDP broadcast channel used for peer discovery.
const (
	DefaultHTTPPort   = 29005
	DefaultBeaconPort = 29006
)

// Config holds resolved runtime paths and ports.
type Config struct {
	DataDir     string // keys, peer trust store, logs
	DownloadDir string // received files land here (Downloads/rocket)
	HTTPPort    int
	BeaconPort  int
}

// Load resolves the default configuration for this user and OS.
func Load() (*Config, error) {
	data, err := dataDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, err
	}
	dl := filepath.Join(downloadsDir(), "rocket")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		return nil, err
	}
	return &Config{
		DataDir:     data,
		DownloadDir: dl,
		HTTPPort:    DefaultHTTPPort,
		BeaconPort:  DefaultBeaconPort,
	}, nil
}

// dataDir returns the OS-appropriate application data directory for rocket.
func dataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "rocket"), nil
}

// downloadsDir returns the user's Downloads folder, falling back to the home
// directory when it cannot be determined.
func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	d := filepath.Join(home, "Downloads")
	if _, err := os.Stat(d); err == nil {
		return d
	}
	// Some locales/headless setups have no Downloads folder; use home.
	_ = runtime.GOOS
	return home
}
