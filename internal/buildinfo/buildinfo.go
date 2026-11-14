// Package buildinfo carries the program version, set at build time with
// -ldflags "-X github.com/donaldsteele/rocket/internal/buildinfo.Version=...".
package buildinfo

// Version is the release string. "dev" in local builds.
var Version = "dev"

// Name is the program name shown in the UI and discovery beacons.
const Name = "rocket"
