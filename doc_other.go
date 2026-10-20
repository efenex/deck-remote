//go:build !darwin

package main

import (
	"fmt"
	"os"
	"syscall"
)

// Non-darwin fallback: O_NOFOLLOW covers only the final component (no
// O_NOFOLLOW_ANY), so the intermediate-symlink TOCTOU is narrowed by the
// canonical-path re-check below but not closed. Linux case-sensitive
// filesystems have no case-folding bypass.
func openDocFile(p string) (*os.File, error) {
	fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), p), nil
}

func fdPath(f *os.File) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}
