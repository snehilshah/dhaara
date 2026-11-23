//go:build unix

package platform

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// Lock takes an exclusive lock on the file at path without waiting, and
// writes info into it so whoever finds it locked can say who holds it.
// The lock goes when release is called or the process ends (even if it
// crashes). On ErrLocked, holder is the info the holder wrote.
func Lock(path, info string) (release func(), holder string, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, "", err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(path)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, strings.TrimSpace(string(b)), ErrLocked
		}
		return nil, "", err
	}
	f.Truncate(0)
	f.WriteAt([]byte(info+"\n"), 0)
	return func() {
		f.Truncate(0)
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, "", nil
}
