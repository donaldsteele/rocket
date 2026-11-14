package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Get(); got != DefaultSettings() || !got.AutoAcceptTrusted {
		t.Fatalf("fresh settings %+v", got)
	}
	dl := filepath.Join(dir, "inbox")
	saved, err := st.Put(Settings{DeviceName: "  den pc  ", DownloadDir: dl + string(filepath.Separator), AutoAcceptTrusted: false})
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{DeviceName: "den pc", DownloadDir: dl, AutoAcceptTrusted: false}
	if saved != want || st.Get() != want {
		t.Fatalf("saved %+v, want %+v", saved, want)
	}
	again, err := OpenSettings(dir)
	if err != nil || again.Get() != want {
		t.Fatalf("reopened: %+v %v", again.Get(), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, SettingsFile)); err != nil || fi.Size() == 0 {
		t.Fatalf("settings file: %v", err)
	}

	for _, bad := range []Settings{
		{DeviceName: strings.Repeat("x", MaxDeviceName+1)},
		{DeviceName: "bad\nname"},
		{DownloadDir: "relative/dir"},
	} {
		if _, err := st.Put(bad); err == nil {
			t.Errorf("Put(%+v) accepted", bad)
		}
	}
	if st.Get() != want {
		t.Fatalf("a rejected Put changed the settings: %+v", st.Get())
	}
}

// TestSettingsMissingFieldDefaults: auto_accept_trusted defaults to true when
// the file does not mention it, and a damaged file falls back to defaults.
func TestSettingsMissingFieldDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, SettingsFile)
	_ = os.WriteFile(p, []byte(`{"device_name":"x"}`), 0o600)
	s, err := ReadSettings(dir)
	if err != nil || !s.AutoAcceptTrusted || s.DeviceName != "x" {
		t.Fatalf("got %+v %v", s, err)
	}
	_ = os.WriteFile(p, []byte(`{nope`), 0o600)
	st, err := OpenSettings(dir)
	if err == nil || st == nil || st.Get() != DefaultSettings() {
		t.Fatalf("damaged file: %+v %v", st, err)
	}
}

// TestLoadUsesSavedSettings: settings.json fills the name and download folder
// unless a flag or environment variable sets them.
func TestLoadUsesSavedSettings(t *testing.T) {
	t.Setenv(EnvName, "")
	t.Setenv(EnvDownloadDir, "")
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	st, _ := OpenSettings(mkdir(t, data))
	saved := filepath.Join(dir, "saved-dl")
	if _, err := st.Put(Settings{DeviceName: "saved-name", DownloadDir: saved, AutoAcceptTrusted: true}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(Overrides{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "saved-name" || c.DownloadDir != saved {
		t.Fatalf("got %+v", c)
	}
	flagDL := filepath.Join(dir, "flag-dl")
	c, err = Load(Overrides{DataDir: data, DownloadDir: flagDL, Name: "flag-name"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "flag-name" || c.DownloadDir != flagDL {
		t.Fatalf("flags must win: %+v", c)
	}
}

func mkdir(t *testing.T, d string) string {
	t.Helper()
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}
