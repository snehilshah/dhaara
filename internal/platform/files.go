// Package platform holds the operating-system bits: keeping the computer
// awake, free disk space, the desktop trash, and private files.
package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// WritePrivateJSON writes atomically, readable only by you (it can hold
// sign-in data and upload addresses).
func WritePrivateJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
