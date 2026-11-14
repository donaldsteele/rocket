package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
)

// errNoRocket means no rocket binary was found next to the tray or on PATH.
var errNoRocket = errors.New("rocket not found")

// rocketName is the rocket binary's file name on goos.
func rocketName(goos string) string {
	if goos == "windows" {
		return "rocket.exe"
	}
	return "rocket"
}

// findRocket locates the rocket binary: first next to the tray executable
// (the release zips ship them side by side), then on PATH. The lookups are
// parameters so tests can run them without touching the real system.
func findRocket(exe, goos string, isFile func(string) bool, lookPath func(string) (string, error)) (string, error) {
	name := rocketName(goos)
	if exe != "" {
		sibling := filepath.Join(filepath.Dir(exe), name)
		if isFile(sibling) {
			return sibling, nil
		}
	}
	if p, err := lookPath(name); err == nil {
		return p, nil
	}
	return "", errNoRocket
}

// rocketArgs is the command line the tray starts rocket with: the local UI
// server without opening a browser, on the port the tray watches.
func rocketArgs(port int) []string {
	return []string{"ui", "--no-browser", "--port", strconv.Itoa(port)}
}

// startRocket launches rocket in the background, detached from the tray so it
// keeps running after the tray exits. Its output is discarded; the tray
// learns whether it came up by polling the API.
func startRocket(port int) error {
	exe, _ := os.Executable()
	bin, err := findRocket(exe, runtime.GOOS, isRegularFile, exec.LookPath)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, rocketArgs(port)...)
	cmd.Dir = filepath.Dir(bin)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap it if it exits while the tray runs
	return nil
}

func isRegularFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
