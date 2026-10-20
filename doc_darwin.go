//go:build darwin

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// O_NOFOLLOW_ANY (macOS 11.3+): fail with ELOOP if ANY path component is a
// symlink, not just the last. Not exported by package syscall.
const oNofollowAny = 0x20000000

func openDocFile(p string) (*os.File, error) {
	fd, err := syscall.Open(p, syscall.O_RDONLY|oNofollowAny|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), p), nil
}

// fdPath is the kernel's spelling of the open file (on-disk case and Unicode
// form, no client-side folding). F_GETPATH, not F_GETPATH_NOFIRMLINK: the latter
// returns the /System/Volumes/Data/... form, which never matches a root.
func fdPath(f *os.File) (string, error) {
	var buf [1024]byte // PATH_MAX
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), uintptr(syscall.F_GETPATH), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", errno
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), nil
}
