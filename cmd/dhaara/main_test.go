package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parse(t *testing.T, args ...string) (command, error) {
	t.Helper()
	return parseFlags(append(args, "--exclude-file", "/nonexistent"), io.Discard)
}

func TestCommands(t *testing.T) {
	home, _ := os.UserHomeDir()
	c, err := parse(t, "add", "music", "~/UpdateMusic", "drive:UpdateMusic", "--exclude", "*.txt", "--follow")
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Mode != "add" || c.name != "music" || !c.add.Follow ||
		strings.Join(c.args, "|") != filepath.Join(home, "UpdateMusic")+"|drive:UpdateMusic" ||
		strings.Join(c.add.Exclude, "|") != "*.txt" || len(c.cfg.Exclude) != 0 {
		t.Errorf("add: %+v", c)
	}
	if c, err := parse(t, "status", "-v", "music"); err != nil || !c.verbose || c.name != "music" {
		t.Errorf("status -v music: %+v %v", c, err)
	}
	if c, err := parse(t, "apply", "--yes"); err != nil || !c.cfg.Yes || c.name != "" {
		t.Errorf("apply --yes: %+v %v", c, err)
	}
	if c, err := parse(t, "download", "drive:Work/Reports"); err != nil || c.cfg.RemoteName != "Work/Reports" || c.cfg.Source != "Reports" {
		t.Errorf("download: %+v %v", c, err)
	}
}

func TestBadCommands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"sync", "~/Reports"}, "replaced by pairings"},
		{[]string{"pending"}, "dhaara status"},
		{[]string{"~/Reports"}, "dhaara upload ~/Reports"},
		{[]string{"stauts"}, "unknown command"},
		{[]string{"status", "--yes"}, "--yes is only for apply"},
		{[]string{"apply", "a", "b"}, "usage"},
		{[]string{"download", "Work/../x"}, "bad Drive folder path"},
		{[]string{"add", "music"}, "usage"},
	} {
		_, err := parse(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: want an error with %q, got %v", tc.args, tc.want, err)
		}
	}
}
