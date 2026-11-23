//go:build !unix

package platform

import "errors"

var ErrLocked = errors.New("locked by another process")

// Lock does nothing where file locks aren't available.
func Lock(path, info string) (release func(), holder string, err error) {
	return func() {}, "", nil
}
