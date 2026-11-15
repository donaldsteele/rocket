package rocketcore

import (
	"os"
	"syscall"
	"testing"
)

// dupFd returns a duplicate of f's handle that the caller owns, as Kotlin's
// ParcelFileDescriptor.detachFd() hands one over.
func dupFd(t *testing.T, f *os.File) int {
	t.Helper()
	p, err := syscall.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	var h syscall.Handle
	if err := syscall.DuplicateHandle(p, syscall.Handle(f.Fd()), p, &h, 0, false, syscall.DUPLICATE_SAME_ACCESS); err != nil {
		t.Fatal(err)
	}
	return int(h)
}
