package exclude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExcludePatterns(t *testing.T) {
	cases := []struct {
		pattern, rel string
		want         bool
	}{
		{"*.txt", "duplicates_report.txt", true},
		{"*.txt", "hindi/notes.txt", true},
		{"*.txt", "hindi/song.mp3", false},
		{"lyrics", "lyrics/a.lrc", true},
		{"lyrics", "hindi/lyrics/a.lrc", true},
		{"lyrics", "hindi/lyrics.mp3", false},
		{"ringtone/", "ringtone/a.mp3", true},
		{"/ringtone", "ringtone/a.mp3", true},
		{"ringtone/", "hindi/ringtone/a.mp3", false},
		{"hindi/*/temp", "hindi/zotify/temp/a.mp3", true},
		{"hindi/*/temp", "hindi/temp/a.mp3", false},
		{"yt-py/ANIMAL*", "yt-py/ANIMAL：x.mp3", true},
	}
	for _, c := range cases {
		if got := Match(c.rel, []string{c.pattern}); got != c.want {
			t.Errorf("excluded(%q, %q) = %v, want %v", c.rel, c.pattern, got, c.want)
		}
	}
	if err := Check([]string{"["}); err == nil {
		t.Error("a bad pattern was accepted")
	}
	p := filepath.Join(t.TempDir(), "exclude")
	if err := os.WriteFile(p, []byte("# comment\n\n*.txt\n  ringtone/  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(p)
	if err != nil || strings.Join(got, ",") != "*.txt,ringtone/" {
		t.Errorf("readExcludeFile: %q %v", got, err)
	}
}
