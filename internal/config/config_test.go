package config

import (
	"path/filepath"
	"testing"
)

func TestLoadOverrides(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(EnvPort, "29150")
	t.Setenv(EnvName, "from-env")
	c, err := Load(Overrides{
		DataDir:     filepath.Join(tmp, "data"),
		DownloadDir: filepath.Join(tmp, "dl"),
		BeaconPort:  29151,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPPort != 29150 || c.BeaconPort != 29151 || c.Name != "from-env" {
		t.Fatalf("got %+v", c)
	}
	if c.DataDir != filepath.Join(tmp, "data") || c.DownloadDir != filepath.Join(tmp, "dl") {
		t.Fatalf("dirs %+v", c)
	}
	// Flags win over the environment.
	c, err = Load(Overrides{DataDir: c.DataDir, DownloadDir: c.DownloadDir, HTTPPort: 29152, Name: "flag"})
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPPort != 29152 || c.Name != "flag" || c.BeaconPort != DefaultBeaconPort {
		t.Fatalf("got %+v", c)
	}
	t.Setenv(EnvPort, "nope")
	if _, err := Load(Overrides{DataDir: c.DataDir, DownloadDir: c.DownloadDir}); err == nil {
		t.Fatal("bad port env accepted")
	}
	if _, err := Load(Overrides{DataDir: c.DataDir, DownloadDir: c.DownloadDir, HTTPPort: 70000}); err == nil {
		t.Fatal("out of range port accepted")
	}
}
