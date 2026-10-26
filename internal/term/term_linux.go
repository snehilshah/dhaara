//go:build linux

package term

import (
	"os"
	"syscall"
	"unsafe"
)

// termWidth returns the terminal's column count, or 100 if unknown.
func termWidth(f *os.File) int {
	if f == nil {
		return 100
	}
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 {
		return 100
	}
	return int(ws.Col)
}
