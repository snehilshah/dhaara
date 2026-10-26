//go:build linux

package platform

import "syscall"

// FreeSpace returns the bytes available to you on the disk holding path,
// or -1 if unknown. A variable so tests can pretend the disk is full.
var FreeSpace = func(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
