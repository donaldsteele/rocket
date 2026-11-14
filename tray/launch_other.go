//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach starts rocket in its own session, so closing the tray (or the
// terminal it was started from) does not take it down.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
