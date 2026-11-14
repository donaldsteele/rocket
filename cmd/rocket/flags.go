package main

import (
	"flag"

	"github.com/donaldsteele/rocket/internal/config"
)

// commonFlags are accepted by every command that runs a rocket host.
type commonFlags struct {
	port        int
	beaconPort  int
	dataDir     string
	downloadDir string
	name        string
	noMDNS      bool
}

// addCommonFlags registers --port, --beacon-port, --data-dir, --download-dir,
// --name and --no-mdns on fs. Unset flags fall back to ROCKET_* environment
// variables, then to the defaults (see internal/config).
func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.IntVar(&c.port, "port", 0, "HTTP port for the UI, API and drop page (default 29005)")
	fs.IntVar(&c.beaconPort, "beacon-port", 0, "UDP port for discovery beacons (default 29006)")
	fs.StringVar(&c.dataDir, "data-dir", "", "folder for the identity key and trust store (default: user config dir/rocket)")
	fs.StringVar(&c.downloadDir, "download-dir", "", "folder received files are saved to (default: Downloads/rocket)")
	fs.StringVar(&c.name, "name", "", "device name shown to peers (default: hostname)")
	fs.BoolVar(&c.noMDNS, "no-mdns", false, "do not use mDNS (_rocket._tcp.local); the UDP beacon still runs")
	return c
}

func (c *commonFlags) overrides() config.Overrides {
	return config.Overrides{
		DataDir: c.dataDir, DownloadDir: c.downloadDir, Name: c.name,
		HTTPPort: c.port, BeaconPort: c.beaconPort,
	}
}
