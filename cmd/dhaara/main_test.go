package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDrivePathFlags(t *testing.T) {
	cfg, err := parseFlags([]string{"phone", "Work/Reports", "--exclude-file", "/nonexistent"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if cfg.RemoteName != "Work/Reports" || cfg.PhoneDir != "/sdcard/Reports" || cfg.Source != filepath.Join(home, "Reports") {
		t.Errorf("phone defaults: %+v", cfg)
	}
	if _, err := parseFlags([]string{"pull", "Work/../x", "--exclude-file", "/nonexistent"}, io.Discard); err == nil {
		t.Error("an unsafe Drive path was accepted")
	}
}
