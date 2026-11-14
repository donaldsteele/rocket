package main

import (
	"os/exec"
	"syscall"
)

// createNoWindow keeps the console program rocket.exe from opening a console
// window when the tray starts it.
const createNoWindow = 0x08000000

// detach starts rocket without a console window and in its own process group,
// so closing the tray does not take it down.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow | syscall.CREATE_NEW_PROCESS_GROUP,
	}
}
