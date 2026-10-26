// Package adb runs file operations on an Android phone through adb. Commands
// run in the phone's own shell (toybox); file names go to it on standard
// input, one per line, never inside a command, so no name can be mistaken
// for a command.
package adb

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/snehilshah/dhaara/internal/gdrive"
)

// File is one file on the phone.
type File struct {
	Size    int64
	MtimeNs int64
}

// Move renames From to To (paths inside Root).
type Move struct{ From, To string }

// Push copies Local to Rel on the phone; MD5 is checked on the phone.
type Push struct {
	Local, Rel, MD5 string
}

// Size is the local file's size (0 if it can't be read).
func (p Push) Size() int64 {
	if fi, err := os.Stat(p.Local); err == nil {
		return fi.Size()
	}
	return 0
}

// Device is one phone folder reached through adb.
type Device struct {
	ADB      string // the adb program
	Root     string // folder on the phone, like /sdcard/Reports
	PartName string // name for a copy in progress, like ".app-push.part"
	// IsPart reports whether a file name is a leftover copy in progress;
	// List leaves those out.
	IsPart func(name string) bool
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (a *Device) shell(ctx context.Context, script, stdin string) (string, error) {
	cmd := exec.CommandContext(ctx, a.ADB, "shell", "-T", script)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("adb shell: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (a *Device) Check(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, a.ADB, "get-state").CombinedOutput()
	state := strings.TrimSpace(string(out))
	if err != nil || state != "device" {
		msg := state
		if msg == "" && err != nil {
			msg = err.Error()
		}
		return gdrive.Fatalf("no phone ready over adb (%s). Unlock the phone and accept \"Allow USB debugging\". "+
			"Over Wi-Fi: turn on Wireless debugging, then adb pair and adb connect. "+
			"With several devices, pick one with ANDROID_SERIAL=<serial>", msg)
	}
	return nil
}

func (a *Device) List(ctx context.Context) (map[string]File, error) {
	script := "cd " + shellQuote(a.Root) + " 2>/dev/null || exit 0; find . -type f -exec stat -c '%s %Y %n' {} +"
	out, err := a.shell(ctx, script, "")
	if err != nil {
		return nil, err
	}
	files := map[string]File{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 3)
		if len(f) != 3 || !strings.HasPrefix(f[2], "./") {
			continue
		}
		size, err1 := strconv.ParseInt(f[0], 10, 64)
		mtime, err2 := strconv.ParseInt(f[1], 10, 64)
		rel := strings.TrimPrefix(f[2], "./")
		if err1 != nil || err2 != nil || !filepath.IsLocal(rel) {
			continue
		}
		if a.IsPart != nil && a.IsPart(path.Base(rel)) {
			continue
		}
		files[rel] = File{Size: size, MtimeNs: mtime * 1e9}
	}
	return files, sc.Err()
}

// lines runs a per-line script and returns one output line per input line.
func (a *Device) lines(ctx context.Context, body string, in []string) ([]string, error) {
	script := "cd " + shellQuote(a.Root) + " || exit 1; tab=$(printf '\\t'); " + body
	out, err := a.shell(ctx, script, strings.Join(in, "\n")+"\n")
	if err != nil {
		return nil, err
	}
	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(got) != len(in) {
		return nil, fmt.Errorf("unexpected reply from the phone (%d lines for %d files)", len(got), len(in))
	}
	return got, nil
}

func (a *Device) MD5(ctx context.Context, rels []string) (map[string]string, error) {
	got, err := a.lines(ctx, `while IFS= read -r f; do h=$(md5sum "./$f" 2>/dev/null) && h=${h#\\} && echo "${h%% *}" || echo ERR; done`, rels)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, rel := range rels {
		if len(got[i]) == 32 {
			out[rel] = got[i]
		}
	}
	return out, nil
}

func (a *Device) Remove(ctx context.Context, rels []string, md5s map[string]string) (map[string]bool, error) {
	in := make([]string, len(rels))
	for i, rel := range rels {
		in[i] = md5s[rel] + "\t" + rel
	}
	got, err := a.lines(ctx, `while IFS="$tab" read -r m f; do h=$(md5sum "./$f" 2>/dev/null); h=${h#\\}; `+
		`if [ "${h%% *}" = "$m" ] && rm -f "./$f"; then echo OK; else echo NO; fi; done`, in)
	if err != nil {
		return nil, err
	}
	ok := map[string]bool{}
	for i, rel := range rels {
		ok[rel] = got[i] == "OK"
	}
	return ok, nil
}

func (a *Device) Move(ctx context.Context, moves []Move) (map[string]bool, error) {
	in := make([]string, len(moves))
	for i, m := range moves {
		in[i] = m.From + "\t" + m.To
	}
	got, err := a.lines(ctx, `while IFS="$tab" read -r a b; do `+
		`if [ ! -e "./$b" ] && mkdir -p "$(dirname "./$b")" && mv "./$a" "./$b"; then echo OK; else echo NO; fi; done`, in)
	if err != nil {
		return nil, err
	}
	ok := map[string]bool{}
	for i, m := range moves {
		ok[m.From] = got[i] == "OK"
	}
	return ok, nil
}

func (a *Device) Push(ctx context.Context, p Push) error {
	part := path.Join(path.Dir(p.Rel), a.PartName)
	out, err := exec.CommandContext(ctx, a.ADB, "push", p.Local, path.Join(a.Root, part)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb push: %v: %s", err, strings.TrimSpace(string(out)))
	}
	got, err := a.lines(ctx, `while IFS="$tab" read -r m p t; do h=$(md5sum "./$p" 2>/dev/null); h=${h#\\}; `+
		`if [ "${h%% *}" = "$m" ] && mv -f "./$p" "./$t"; then echo OK; else rm -f "./$p"; echo NO; fi; done`,
		[]string{p.MD5 + "\t" + part + "\t" + p.Rel})
	if err != nil {
		return err
	}
	if got[0] != "OK" {
		return fmt.Errorf("the copy on the phone did not match (MD5); it was removed")
	}
	return nil
}

func (a *Device) RemoveEmptyDirs(ctx context.Context, rels []string) error {
	if len(rels) == 0 {
		return nil
	}
	rels = slices.Clone(rels)
	slices.SortFunc(rels, func(a, b string) int { return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b)) })
	rels = slices.Compact(rels) // deepest first, so parents empty out
	_, err := a.lines(ctx, `while IFS= read -r d; do rmdir "./$d" 2>/dev/null; echo .; done`, rels)
	return err
}
