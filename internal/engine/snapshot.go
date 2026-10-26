package engine

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/term"
)

// A snapshot is a full copy of a Drive folder, made on Drive with its copy
// command: no file data passes through this machine and nothing is written
// to its disk. Snapshots go in "<folder> snapshots/<date>", next to the
// folder, and are only made when asked. The copy counts against Drive storage.
//
// While copying, the snapshot folder's name ends in " (unfinished)". A
// stopped snapshot continues on the next run, and the name loses the
// marker only once every file has been checked.

const unfinishedSuffix = " (unfinished)"

func snapshotsFolderName(remoteName string) string { return remoteName + " snapshots" }

// Snapshot copies the Drive folder, on Drive, into "<folder> snapshots/<date>".
func Snapshot(ctx context.Context, cfg Config, ui *term.Console) error {
	d, about, err := connect(ctx, cfg, ui)
	if err != nil {
		return err
	}
	name := cfg.RemoteName
	ui.Printf("Reading the Drive folder %q...", name)
	rootID, parentID, err := gdrive.ResolveFolder(ctx, d, name, false)
	if err != nil {
		return err
	}
	if rootID == "" {
		return gdrive.Fatalf("no folder named %q found in your Drive. dhaara can only see folders it uploaded itself", name)
	}
	src, err := listTree(ctx, d, rootID)
	if err != nil {
		return err
	}
	for _, s := range src.skipped {
		ui.Printf("Not copied: %s", s)
	}
	var total int64
	for _, rf := range src.files {
		total += rf.Size
	}
	ui.Printf("On Drive: %d files, %s, %d subfolders.", len(src.files), term.Size(total), len(src.dirs))

	snapsName := snapshotsFolderName(name)
	snapsID, err := gdrive.EnsureFolder(ctx, d, parentID, filepath.Base(snapsName), !cfg.DryRun)
	if err != nil {
		return err
	}
	var existing []gdrive.File
	if snapsID != "" {
		if existing, err = d.ListChildren(ctx, snapsID); err != nil {
			return err
		}
	}
	workID, final := "", ""
	taken := map[string]bool{}
	for _, k := range existing {
		if !k.IsFolder() {
			continue
		}
		taken[strings.TrimSuffix(k.Name, unfinishedSuffix)] = true
		if workID == "" && strings.HasSuffix(k.Name, unfinishedSuffix) {
			workID, final = k.ID, strings.TrimSuffix(k.Name, unfinishedSuffix)
		}
	}
	if workID == "" {
		date := time.Now().Format("2006-01-02")
		final = date
		for n := 2; taken[final]; n++ {
			final = fmt.Sprintf("%s (%d)", date, n)
		}
	} else {
		ui.Printf("Continuing the unfinished snapshot %q.", final)
	}
	where := snapsName + "/" + final

	if cfg.DryRun {
		ui.Printf("Would copy %d files (%s) on Drive into %q. Nothing would be downloaded.", len(src.files), term.Size(total), where)
		ui.Println("Dry run: nothing was changed.")
		return nil
	}
	limit, hasLimit := parseInt(about.StorageQuota.Limit)
	usage, _ := parseInt(about.StorageQuota.Usage)
	if hasLimit && workID == "" && total > limit-usage {
		return gdrive.Fatalf("not enough Google Drive space for a snapshot: it needs %s but only %s is free",
			term.Size(total), term.Size(max(0, limit-usage)))
	}
	if workID == "" {
		if workID, err = d.CreateFolder(ctx, final+unfinishedSuffix, snapsID); err != nil {
			return err
		}
	}

	// The same folders, and what's already there from a stopped run.
	dst, err := listTree(ctx, d, workID)
	if err != nil {
		return err
	}
	for _, dir := range src.dirs { // parents first
		if dst.dirIDs[dir] != "" {
			continue
		}
		parent := dst.dirIDs[relDir(dir)]
		id, err := d.CreateFolder(ctx, filepath.Base(dir), parent)
		if err != nil {
			return err
		}
		dst.dirIDs[dir] = id
	}
	var todo []remoteFile
	for _, rf := range src.files {
		if have, ok := dst.byRel[rf.Rel]; ok && have.MD5 == rf.MD5 && have.Size == rf.Size {
			continue
		}
		todo = append(todo, rf)
	}
	if done := len(src.files) - len(todo); done > 0 {
		ui.Printf("Already copied: %d files.", done)
	}

	if len(todo) > 0 {
		ui.Printf("Copying %d files on Drive (%s)...", len(todo), term.Size(total))
		failed, err := copyAll(ctx, cfg, d, ui, todo, dst.dirIDs)
		if err != nil {
			return err
		}
		if len(failed) > 0 {
			ui.Printf("%d files could not be copied (listed above). Run the same command again to continue this snapshot.", len(failed))
			return ErrProblems
		}
	}

	// Check the copy against the original, file by file.
	ui.Println("Checking the snapshot against the original (size + MD5)...")
	check, err := listTree(ctx, d, workID)
	if err != nil {
		return err
	}
	for _, dup := range check.dups {
		if keep, ok := check.byRel[dup.Rel]; ok && keep.MD5 == dup.MD5 && keep.Size == dup.Size {
			if err := d.Trash(ctx, dup.ID); err != nil {
				return err
			}
		}
	}
	var bad []string
	for _, rf := range src.files {
		if have, ok := check.byRel[rf.Rel]; !ok || have.MD5 != rf.MD5 || have.Size != rf.Size {
			bad = append(bad, rf.Rel)
		}
	}
	if len(bad) > 0 {
		ui.Printf("NOT VERIFIED: %d files are missing or different in the snapshot:", len(bad))
		for i, rel := range bad {
			if i == 50 {
				ui.Printf("  ... and %d more", len(bad)-50)
				break
			}
			ui.Printf("  %s", rel)
		}
		ui.Println("Run the same command again to continue this snapshot.")
		return ErrProblems
	}
	if err := d.Move(ctx, workID, final, snapsID, snapsID); err != nil {
		return err
	}
	ui.Printf("VERIFIED: snapshot saved as %q (%d files, %s). Nothing was downloaded.", where, len(src.files), term.Size(total))
	return nil
}

func relDir(rel string) string {
	if d := filepath.Dir(rel); d != "." {
		return d
	}
	return ""
}

// copyAll copies files on Drive with a few workers, checking each copy's MD5.
func copyAll(ctx context.Context, cfg Config, d *gdrive.Drive, ui *term.Console, todo []remoteFile, dirIDs map[string]string) ([]string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var copied atomic.Int64
	var mu sync.Mutex
	var failed []string
	jobs := make(chan remoteFile)
	var wg sync.WaitGroup
	for range cfg.Workers {
		wg.Go(func() {
			for rf := range jobs {
				err := copyOne(ctx, d, rf, dirIDs[relDir(rf.Rel)])
				switch {
				case err == nil:
					copied.Add(1)
				case ctx.Err() != nil:
				case isFatal(err):
					cancel(err)
				default:
					slog.Error("copy failed", "file", rf.Rel, "err", err)
					ui.Printf("FAILED  %s: %v", rf.Rel, err)
					mu.Lock()
					failed = append(failed, rf.Rel)
					mu.Unlock()
				}
			}
		})
	}
	quit := make(chan struct{})
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		last := time.Now()
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
				line := fmt.Sprintf("Copied %d of %d files", copied.Load(), len(todo))
				if ui.IsTTY() {
					ui.Status(line)
				} else if time.Since(last) >= time.Minute {
					ui.Println(line)
					last = time.Now()
				}
			}
		}
	}()
feed:
	for _, rf := range todo {
		select {
		case jobs <- rf:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	close(quit)
	ui.ClearStatus()
	ui.Printf("Copied %d files.", copied.Load())
	return failed, context.Cause(ctx)
}

func copyOne(ctx context.Context, d *gdrive.Drive, rf remoteFile, parent string) error {
	modified := ""
	if !rf.Modified.IsZero() {
		modified = rf.Modified.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		got, err := d.Copy(ctx, rf.ID, filepath.Base(rf.Rel), parent, modified)
		if err != nil {
			return err // d.Copy already retried network and server errors
		}
		if got.MD5 == "" && rf.Size > 0 {
			if got, err = d.GetFile(ctx, got.ID); err != nil {
				return err
			}
		}
		if got.MD5 == rf.MD5 && got.SizeBytes() == rf.Size {
			return nil
		}
		lastErr = fmt.Errorf("Drive's copy did not match (md5 %s, expected %s)", got.MD5, rf.MD5)
		if err := d.Trash(ctx, got.ID); err != nil {
			return err
		}
	}
	return lastErr
}
