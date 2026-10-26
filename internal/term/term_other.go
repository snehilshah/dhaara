//go:build !linux

package term

import "os"

func termWidth(*os.File) int { return 100 }
