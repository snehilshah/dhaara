//go:build !linux

package platform

var FreeSpace = func(string) int64 { return -1 }
