//go:build !linux

package term

import "os"

func termWidth(*os.File) int { return 100 }

// IsTerminal reports whether f looks like an interactive terminal.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
