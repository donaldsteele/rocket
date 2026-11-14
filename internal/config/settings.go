package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// SettingsFile is the settings file inside the data folder.
const SettingsFile = "settings.json"

// MaxDeviceName is the longest device name accepted, in runes.
const MaxDeviceName = 64

// Settings are the person's preferences, saved in settings.json. Empty
// DeviceName and DownloadDir mean "use the default" (the hostname and
// Downloads/rocket). Command-line flags and ROCKET_* variables override the
// saved values at start-up (see Load).
type Settings struct {
	DeviceName  string `json:"device_name"`
	DownloadDir string `json:"download_dir"`
	// AutoAcceptTrusted skips the accept prompt for trusted senders (whose
	// key matches the pinned key) and remembered browsers. Default true.
	AutoAcceptTrusted bool `json:"auto_accept_trusted"`
}

// DefaultSettings is what a fresh install uses.
func DefaultSettings() Settings { return Settings{AutoAcceptTrusted: true} }

// ReadSettings reads settings.json from dataDir. A missing file yields
// DefaultSettings; a missing field keeps its default.
func ReadSettings(dataDir string) (Settings, error) {
	s := DefaultSettings()
	b, err := os.ReadFile(filepath.Join(dataDir, SettingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return DefaultSettings(), fmt.Errorf("%s: %w", SettingsFile, err)
	}
	return s, nil
}

// Normalize checks and tidies s: the device name is trimmed, must be at most
// MaxDeviceName runes and have no control characters; the download folder,
// when set, must be an absolute path and is cleaned.
func (s Settings) Normalize() (Settings, error) {
	s.DeviceName = strings.TrimSpace(s.DeviceName)
	if utf8.RuneCountInString(s.DeviceName) > MaxDeviceName {
		return s, fmt.Errorf("device name is longer than %d characters", MaxDeviceName)
	}
	for _, r := range s.DeviceName {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return s, errors.New("device name contains control characters")
		}
	}
	s.DownloadDir = strings.TrimSpace(s.DownloadDir)
	if s.DownloadDir != "" {
		if !filepath.IsAbs(s.DownloadDir) {
			return s, errors.New("download folder must be an absolute path")
		}
		s.DownloadDir = filepath.Clean(s.DownloadDir)
	}
	return s, nil
}

// SettingsStore holds the saved settings, safe for concurrent use.
type SettingsStore struct {
	path string
	mu   sync.Mutex
	cur  Settings
}

// OpenSettings loads settings.json from dataDir. A missing or unreadable
// file yields the defaults (a damaged file must not stop rocket starting; the
// next Put replaces it); the error, if any, is returned alongside for logging.
func OpenSettings(dataDir string) (*SettingsStore, error) {
	s, err := ReadSettings(dataDir)
	return &SettingsStore{path: filepath.Join(dataDir, SettingsFile), cur: s}, err
}

// Get returns the saved settings.
func (st *SettingsStore) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cur
}

// Put normalizes s, saves it atomically (mode 0600) and returns what was
// saved.
func (st *SettingsStore) Put(s Settings) (Settings, error) {
	s, err := s.Normalize()
	if err != nil {
		return s, err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return s, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return s, err
	}
	if err := os.Rename(tmp, st.path); err != nil {
		os.Remove(tmp)
		return s, err
	}
	st.cur = s
	return s, nil
}
