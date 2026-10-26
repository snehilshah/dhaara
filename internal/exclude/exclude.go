// Package exclude matches paths against exclusion patterns.
package exclude

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Exclusion patterns, like a simple .gitignore. One pattern per line in
// ~/.config/dhaara/exclude (blank lines and lines starting with # are
// ignored), plus any --exclude options.
//
//   - No "/" in the pattern: matches a file or folder name at any depth.
//     "*.tmp" leaves out every .tmp file; "node_modules" leaves out every
//     folder (or file) called node_modules, with everything inside.
//   - With a "/": matches the path from the top of the folder. "build/"
//     or "/build" leaves out only the top-level build folder;
//     "projects/*/cache" leaves out a cache folder two levels down.
//
// Patterns use shell wildcards: * (anything but /), ? (one character) and
// [abc]. Excluded files are never uploaded, downloaded, copied to the phone
// or removed anywhere: dhaara acts as if they weren't there.

// Match reports whether rel (a relative path, "/"-separated) or any
// folder above it matches one of the patterns.
func Match(rel string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, p := range patterns {
		pat := strings.Trim(p, "/")
		if !strings.Contains(p, "/") { // a name, at any depth
			for _, part := range parts {
				if ok, _ := filepath.Match(pat, part); ok {
					return true
				}
			}
			continue
		}
		for i := range parts {
			if ok, _ := filepath.Match(pat, strings.Join(parts[:i+1], "/")); ok {
				return true
			}
		}
	}
	return false
}

// Check rejects patterns the matcher can't read, such as "[".
func Check(patterns []string) error {
	for _, p := range patterns {
		if strings.Trim(p, "/") == "" {
			return fmt.Errorf("exclude pattern %q matches nothing", p)
		}
		if _, err := filepath.Match(strings.Trim(p, "/"), ""); err != nil {
			return fmt.Errorf("bad exclude pattern %q: %v", p, err)
		}
	}
	return nil
}

// ReadFile reads one pattern per line; a missing file means none.
func ReadFile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}
