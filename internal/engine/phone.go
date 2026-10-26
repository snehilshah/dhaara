package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/snehilshah/dhaara/internal/adb"
	"github.com/snehilshah/dhaara/internal/exclude"
	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/term"
)

// "dhaara phone" makes a folder on an Android phone follow Drive, over adb (USB,
// or wireless debugging). It follows the same rules as pull (mirror.go), but
// file data comes from this laptop's own copy of the folder, so nothing is
// downloaded: a file is copied to the phone only if the laptop's copy is
// identical to Drive's. It never changes Drive or the laptop.
//
// On the phone every removal is checked first: the file is deleted only if
// its MD5, worked out on the phone right before, is the one on the removed
// list. Copies go to a hidden ".dhaara-push.part" file and are renamed into
// place only after their MD5 checks out on the phone.

// phoneDevice is what runPhone needs from the phone. adbDevice is the real
// one; tests use a folder.
type phoneDevice interface {
	Check(ctx context.Context) error
	List(ctx context.Context) (map[string]adb.File, error)
	MD5(ctx context.Context, rels []string) (map[string]string, error)
	Remove(ctx context.Context, rels []string, md5s map[string]string) (map[string]bool, error)
	Move(ctx context.Context, moves []adb.Move) (map[string]bool, error)
	Push(ctx context.Context, p adb.Push) error
	RemoveEmptyDirs(ctx context.Context, rels []string) error
}

// newPhoneDevice is replaced in tests.
var newPhoneDevice = func(cfg Config) (phoneDevice, error) {
	adbPath := cfg.ADB
	if adbPath == "" {
		if p, err := exec.LookPath("adb"); err == nil {
			adbPath = p
		} else if home, _ := os.UserHomeDir(); home != "" {
			if p := filepath.Join(home, "Android", "Sdk", "platform-tools", "adb"); fileExists(p) {
				adbPath = p
			}
		}
	}
	if adbPath == "" {
		return nil, gdrive.Fatalf("adb not found. Install it (sudo apt install adb) or pass --adb /path/to/adb")
	}
	return newADBDevice(adbPath, cfg.PhoneDir), nil
}

// newADBDevice makes the real phone, naming copies in progress like
// unfinished downloads so a later run (and List) knows to skip them.
func newADBDevice(adbPath, root string) *adb.Device {
	return &adb.Device{ADB: adbPath, Root: root, PartName: partPrefix + "push" + partSuffix,
		IsPart: func(name string) bool {
			return strings.HasPrefix(name, partPrefix) && strings.HasSuffix(name, partSuffix)
		}}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Phone makes the phone folder cfg.PhoneDir follow the Drive folder over
// adb, copying files from the local folder cfg.Source. The check saves a
// plan and changes nothing; Apply carries it out.
func Phone(ctx context.Context, cfg Config, ui *term.Console) error {
	from, err := filepath.Abs(cfg.Source)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(from); err != nil || !fi.IsDir() {
		return gdrive.Fatalf("not a folder: %s (the laptop's copy, where files for the phone come from)", from)
	}
	dev, err := newPhoneDevice(cfg)
	if err != nil {
		return err
	}
	if err := dev.Check(ctx); err != nil {
		return gdrive.Fatalf("%v", err)
	}

	d, _, err := connect(ctx, cfg, ui)
	if err != nil {
		return err
	}
	ui.Printf("Reading the Drive folder %q...", cfg.RemoteName)
	rootID, _, err := gdrive.ResolveFolder(ctx, d, cfg.RemoteName, false)
	if err != nil {
		return err
	}
	if rootID == "" {
		return gdrive.Fatalf("no folder named %q found in your Drive. dhaara can only see folders it uploaded itself", cfg.RemoteName)
	}
	remote, _, _, err := readRemote(ctx, d, rootID, cfg.Exclude, ui)
	if err != nil {
		return err
	}
	removed, err := loadRemoved(ctx, d, cfg.RemoteName, rootID)
	if err != nil {
		return err
	}

	ui.Printf("Reading the phone's folder %s...", cfg.PhoneDir)
	onPhone, err := dev.List(ctx)
	if err != nil {
		return err
	}
	local := map[string]mirrorLocal{}
	var excludedPhone []string
	for rel, f := range onPhone {
		if exclude.Match(rel, cfg.Exclude) {
			excludedPhone = append(excludedPhone, rel)
			continue
		}
		local[rel] = mirrorLocal{f.Size, f.MtimeNs}
	}
	ui.Printf("On the phone: %d files.", len(local))
	printExcluded(ui, "on the phone", excludedPhone)

	key := sha256.Sum256([]byte("phone\x00" + cfg.PhoneDir + "\x00" + cfg.RemoteName))
	pst, err := openState(filepath.Join(cfg.StateDir, "phone-"+hex.EncodeToString(key[:8])))
	if err != nil {
		return gdrive.Fatalf("cannot create state folder: %v", err)
	}
	defer pst.Flush()

	// Checksums on the phone, remembered by size and time so later runs only
	// read new or changed files.
	md5s := map[string]string{}
	var unknown []string
	for _, rel := range mirrorNeeds(remote, local, removed) {
		l := local[rel]
		if m, ok := pst.Cached(rel, l.Size, l.MtimeNs); ok {
			md5s[rel] = m
		} else {
			unknown = append(unknown, rel)
		}
	}
	if len(unknown) > 0 {
		ui.Printf("Checking %d files on the phone (MD5)...", len(unknown))
		for start := 0; start < len(unknown); start += 200 {
			batch := unknown[start:min(start+200, len(unknown))]
			got, err := dev.MD5(ctx, batch)
			if err != nil {
				return err
			}
			for rel, m := range got {
				md5s[rel] = m
				l := local[rel]
				pst.Remember(&LocalFile{Rel: rel, Size: l.Size, MtimeNs: l.MtimeNs}, m)
			}
			if ui.IsTTY() {
				ui.Status(fmt.Sprintf("Checked %d of %d", min(start+200, len(unknown)), len(unknown)))
			}
		}
		ui.ClearStatus()
	}

	plan := planMirror(remote, local, md5s, removed, false)

	// Files for the phone come from the laptop's copy, which must match Drive.
	lkey := sha256.Sum256([]byte(from + "\x00" + cfg.RemoteName))
	lst, err := openState(filepath.Join(cfg.StateDir, hex.EncodeToString(lkey[:8])))
	if err != nil {
		return gdrive.Fatalf("cannot create state folder: %v", err)
	}
	defer lst.Flush()
	var notReady []string
	var ready []remoteFile
	for _, r := range plan.fetch {
		p := filepath.Join(from, r.Rel)
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != r.Size {
			notReady = append(notReady, r.Rel)
			continue
		}
		m, err := lst.MD5(&LocalFile{Rel: r.Rel, Path: p, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()})
		if err != nil || m != r.MD5 {
			notReady = append(notReady, r.Rel)
			continue
		}
		ready = append(ready, r)
	}
	plan.fetch = ready
	var notes []string
	for _, rel := range notReady {
		notes = append(notes, fmt.Sprintf("can't copy (missing or different in %s; run dhaara sync there first): %s", from, rel))
	}

	steps := mirrorSteps(plan, md5s, "copy to the phone", "on the phone")
	if cfg.applying == nil { // the check: save the plan, change nothing
		printMirrorPlan(ui, plan, "copy to the phone", "on the phone")
		if len(notReady) > 0 {
			ui.Printf("Can't copy: %d files on Drive are missing or different in %s (run dhaara sync there first):", len(notReady), from)
			for i, rel := range notReady {
				if i == 20 {
					ui.Printf("  ... and %d more", len(notReady)-20)
					break
				}
				ui.Printf("  %s", rel)
			}
		}
		warnMassRemoval(ui, len(plan.remove), len(local), "on the phone")
		err := savePlan(cfg, ui, &savedPlan{Kind: "phone", Source: from, RemoteName: cfg.RemoteName, PhoneDir: cfg.PhoneDir,
			Steps: steps, Conflicts: plan.conflicts, Notes: notes})
		if err == nil && len(notReady) > 0 {
			err = ErrProblems
		}
		return err
	}
	plan = filterMirror(plan, md5s, matchSteps(ui, cfg.applying, steps))
	if err := checkMassRemoval(len(plan.remove), len(local), "on the phone", cfg); err != nil {
		return err
	}
	var pushes []adb.Push
	for _, r := range plan.fetch {
		pushes = append(pushes, adb.Push{Local: filepath.Join(from, r.Rel), Rel: r.Rel, MD5: r.MD5})
	}

	var problems int
	var emptied []string
	if len(plan.moves) > 0 {
		var mv []adb.Move
		for _, m := range plan.moves {
			mv = append(mv, adb.Move{From: m.From, To: m.To.Rel})
		}
		ok, err := dev.Move(ctx, mv)
		if err != nil {
			return err
		}
		for _, m := range mv {
			if !ok[m.From] {
				problems++
				ui.Printf("FAILED  moving %s on the phone", m.From)
				continue
			}
			emptied = append(emptied, parentDirs(m.From)...)
			l := local[m.From]
			pst.Remember(&LocalFile{Rel: m.To, Size: l.Size, MtimeNs: l.MtimeNs}, md5s[m.From])
		}
	}
	if len(plan.remove) > 0 {
		ok, err := dev.Remove(ctx, plan.remove, md5s)
		if err != nil {
			return err
		}
		for _, rel := range plan.remove {
			if !ok[rel] {
				problems++
				ui.Printf("NOT REMOVED  %s (it changed on the phone since it was checked)", rel)
				continue
			}
			emptied = append(emptied, parentDirs(rel)...)
			slog.Info("removed on the phone", "file", rel)
		}
	}
	if err := dev.RemoveEmptyDirs(ctx, emptied); err != nil {
		slog.Warn("cannot remove empty folders on the phone", "err", err)
	}

	var total, done int64
	for _, p := range pushes {
		total += p.Size()
	}
	started := time.Now()
	for i, p := range pushes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ui.IsTTY() {
			ui.Status(fmt.Sprintf("Copying to the phone: %d of %d files, %s of %s", i, len(pushes), term.Size(done), term.Size(total)))
		}
		if err := dev.Push(ctx, p); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			problems++
			ui.ClearStatus()
			ui.Printf("FAILED  %s: %v", p.Rel, err)
			slog.Error("push failed", "file", p.Rel, "err", err)
			continue
		}
		done += p.Size()
		slog.Info("copied to the phone", "file", p.Rel)
	}
	ui.ClearStatus()
	ui.Printf("Phone: %d moved, %d removed, %d copied (%s in %s).", len(plan.moves), len(plan.remove),
		len(pushes), term.Size(done), term.Duration(time.Since(started)))
	if problems > 0 {
		return ErrProblems
	}
	ui.Println("The phone's folder matches Drive. Apps that index files (a music player, a gallery) see the changes on their next scan.")
	return nil
}

// Cached returns a remembered MD5 for a file of this size and time.
func (s *State) Cached(rel string, size, mtimeNs int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.md5s[rel]
	if ok && e.Size == size && e.MtimeNs == mtimeNs {
		return e.MD5, true
	}
	return "", false
}
