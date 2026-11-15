//go:build !windows

package rocketcore

import (
	"os"
	"syscall"
	"testing"
)

// dupFd returns a duplicate of f's descriptor that the caller owns, as
// Kotlin's ParcelFileDescriptor.detachFd() hands one over.
func dupFd(t *testing.T, f *os.File) int {
	t.Helper()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return fd
}
