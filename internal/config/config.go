// Package config resolves the per-user data directory, the folder received
// files are written to, the device name and the ports, applying command-line
// and environment overrides.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Default ports. The HTTP port serves the UI, the transfer API and the drop
// page. The beacon port is the UDP broadcast channel used for peer discovery.
const (
	DefaultHTTPPort   = 29005
	DefaultBeaconPort = 29006
)

// Environment variables consulted when the matching flag is not given.
const (
	EnvDataDir     = "ROCKET_DATA_DIR"
	EnvDownloadDir = "ROCKET_DOWNLOAD_DIR"
	EnvName        = "ROCKET_NAME"
	EnvPort        = "ROCKET_PORT"
	EnvBeaconPort  = "ROCKET_BEACON_PORT"
)

// Config holds resolved runtime paths and ports.
type Config struct {
	DataDir     string // keys, peer trust store, drop tokens
	DownloadDir string // received files land here (Downloads/rocket)
	Name        string // device name; empty means the hostname
	HTTPPort    int
	BeaconPort  int
}

// Overrides are values from command-line flags. Zero values mean "not set":
// the environment variable is used, then the default.
type Overrides struct {
	DataDir     string
	DownloadDir string
	Name        string
	HTTPPort    int
	BeaconPort  int
}

// Load resolves the configuration for this user and OS (flags, then
// environment, then defaults) and creates the data and download folders.
func Load(o Overrides) (*Config, error) {
	c := &Config{
		DataDir:     pick(o.DataDir, os.Getenv(EnvDataDir)),
		DownloadDir: pick(o.DownloadDir, os.Getenv(EnvDownloadDir)),
		Name:        pick(o.Name, os.Getenv(EnvName)),
	}
	var err error
	if c.HTTPPort, err = port(o.HTTPPort, EnvPort, DefaultHTTPPort); err != nil {
		return nil, err
	}
	if c.BeaconPort, err = port(o.BeaconPort, EnvBeaconPort, DefaultBeaconPort); err != nil {
		return nil, err
	}
	if c.DataDir == "" {
		if c.DataDir, err = dataDir(); err != nil {
			return nil, err
		}
	}
	if c.DownloadDir == "" {
		c.DownloadDir = filepath.Join(downloadsDir(), "rocket")
	}
	if c.DataDir, err = filepath.Abs(c.DataDir); err != nil {
		return nil, err
	}
	if c.DownloadDir, err = filepath.Abs(c.DownloadDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.DownloadDir, 0o755); err != nil {
		return nil, err
	}
	return c, nil
}

func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func port(flagVal int, env string, def int) (int, error) {
	p := flagVal
	if p == 0 {
		if s := os.Getenv(env); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				return 0, fmt.Errorf("%s: %q is not a port number", env, s)
			}
			p = n
		}
	}
	if p == 0 {
		p = def
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %d is out of range", p)
	}
	return p, nil
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
	return home
}
