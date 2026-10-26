package engine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/platform"
	"github.com/snehilshah/dhaara/internal/term"
)

// Sync keeps a local folder and its Drive folder the same, in both
// directions. To tell "deleted here" from "added on Drive", it keeps a record
// of every file that was the same on both sides at the end of the last sync
// (synced.json in the folder's state directory). For each path:
//
//	here     Drive    record      what sync does
//	A        A        any         nothing
//	A        -        A           deleted on Drive: move the local file to the trash
//	A        -        -/other     new here: upload
//	-        A        A           deleted here: move Drive's copy to Drive's trash
//	-        A        -/other     new on Drive: download
//	A'       A        A           changed here: upload, old Drive copy to trash
//	A        A'       A           changed on Drive: download over the local file
//	A        B        -/other     both changed: conflict, nothing is touched
//
// A file added here with the same contents as one deleted here is a move or
// rename: Drive's copy is moved instead of uploading it again (and the same
// the other way). Everything sync takes out of the Drive folder goes on the
// removed list first (see removed.go), so pull and phone can follow.
//
// With no record yet (the first sync), the record starts from the files that
// earlier dhaara uploads of this folder put on Drive, if any; otherwise
// nothing is removed anywhere, except local files that the removed list
// says were taken off Drive on purpose.

type syncEntry struct {
	MD5  string `json:"md5"`
	Size int64  `json:"size"`
}

// SyncSummary describes what a sync run did.
type SyncSummary struct {
	InSync, Uploaded, Downloaded, MovedOnDrive, MovedHere, TrashedOnDrive, TrashedHere int
	Conflicts, Failed, Excluded                                                        int
}

type driveMove struct {
	from remoteFile
	to   *LocalFile
}

type localMove struct {
	from string // local relative path
	to   remoteFile
}

type syncPlan struct {
	upload     []*LocalFile // new here, or changed here (ReplaceIDs set)
	replaced   []remoteFile // Drive copies those uploads replace
	driveMoves []driveMove
	driveTrash []remoteFile
	download   []remoteFile // new on Drive, or changed there
	localMoves []localMove
	localTrash []string
	conflicts  []string
	inSync     int
}

func (p *syncPlan) empty() bool {
	return len(p.upload)+len(p.driveMoves)+len(p.driveTrash)+len(p.download)+len(p.localMoves)+len(p.localTrash) == 0
}

// planSync decides what to do. It changes nothing.
func planSync(base map[string]syncEntry, local map[string]*LocalFile, localMD5 map[string]string,
	remote map[string]remoteFile, removed *removedList) syncPlan {
	var p syncPlan
	paths := map[string]bool{}
	for k := range local {
		paths[k] = true
	}
	for k := range remote {
		paths[k] = true
	}
	sorted := make([]string, 0, len(paths))
	for k := range paths {
		sorted = append(sorted, k)
	}
	slices.Sort(sorted)

	var newHere []*LocalFile  // candidates for a move on Drive
	var newThere []remoteFile // candidates for a move here
	for _, rel := range sorted {
		l, hasL := local[rel]
		r, hasR := remote[rel]
		b, hasB := base[rel]
		var lm string
		if hasL {
			lm = localMD5[rel]
		}
		switch {
		case hasL && hasR:
			switch {
			case lm == r.MD5 && l.Size == r.Size:
				p.inSync++
			case hasB && b.MD5 == r.MD5:
				l.ReplaceIDs = []string{r.ID}
				p.upload = append(p.upload, l)
				p.replaced = append(p.replaced, r)
			case (hasB && b.MD5 == lm) || (!hasB && removed.Has(rel, lm)):
				p.download = append(p.download, r)
			default:
				p.conflicts = append(p.conflicts, rel)
			}
		case hasL:
			if (hasB && b.MD5 == lm) || (!hasB && removed.Has(rel, lm)) {
				p.localTrash = append(p.localTrash, rel)
			} else {
				newHere = append(newHere, l)
			}
		case hasR:
			if hasB && b.MD5 == r.MD5 {
				p.driveTrash = append(p.driveTrash, r)
			} else {
				newThere = append(newThere, r)
			}
		}
	}

	// Moves and renames: a new file whose contents match one being removed.
	type key struct {
		md5  string
		size int64
	}
	gone := map[key][]int{}
	for i, r := range p.driveTrash {
		k := key{r.MD5, r.Size}
		gone[k] = append(gone[k], i)
	}
	usedDrive := map[int]bool{}
	for _, l := range newHere {
		k := key{localMD5[l.Rel], l.Size}
		if ids := gone[k]; len(ids) > 0 && k.md5 != "" {
			gone[k] = ids[1:]
			usedDrive[ids[0]] = true
			p.driveMoves = append(p.driveMoves, driveMove{from: p.driveTrash[ids[0]], to: l})
			continue
		}
		p.upload = append(p.upload, l)
	}
	p.driveTrash = dropIndexes(p.driveTrash, usedDrive)

	goneHere := map[key][]int{}
	for i, rel := range p.localTrash {
		k := key{localMD5[rel], local[rel].Size}
		goneHere[k] = append(goneHere[k], i)
	}
	usedHere := map[int]bool{}
	for _, r := range newThere {
		k := key{r.MD5, r.Size}
		if ids := goneHere[k]; len(ids) > 0 && k.md5 != "" {
			goneHere[k] = ids[1:]
			usedHere[ids[0]] = true
			p.localMoves = append(p.localMoves, localMove{from: p.localTrash[ids[0]], to: r})
			continue
		}
		p.download = append(p.download, r)
	}
	p.localTrash = dropIndexes(p.localTrash, usedHere)
	return p
}

func dropIndexes[T any](items []T, drop map[int]bool) []T {
	var out []T
	for i, it := range items {
		if !drop[i] {
			out = append(out, it)
		}
	}
	return out
}

// Sync makes the local folder and the Drive folder the same, both ways.
func Sync(ctx context.Context, cfg Config, ui *term.Console) (SyncSummary, error) {
	var sum SyncSummary
	src, err := filepath.Abs(cfg.Source)
	if err != nil {
		return sum, err
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return sum, gdrive.Fatalf("not a folder: %s", src)
	}
	dest := cfg.RemoteName
	if dest == "" {
		dest = filepath.Base(src)
	}

	dirs, files, exLocal, err := scanFolder(src, cfg.Exclude, ui)
	if err != nil {
		return sum, err
	}
	key := sha256.Sum256([]byte(src + "\x00" + dest)) // the same state as upload, so its checksums are reused
	st, err := openState(filepath.Join(cfg.StateDir, hex.EncodeToString(key[:8])))
	if err != nil {
		return sum, gdrive.Fatalf("cannot create state folder: %v", err)
	}
	defer st.Flush()

	d, about, err := connect(ctx, cfg, ui)
	if err != nil {
		return sum, err
	}
	ui.Printf("Reading the Drive folder %q...", dest)
	rootID, _, err := gdrive.ResolveFolder(ctx, d, dest, false)
	if err != nil {
		return sum, err
	}
	remote, _, exRemote, err := readRemote(ctx, d, rootID, cfg.Exclude, ui)
	if err != nil {
		return sum, err
	}
	sum.Excluded = len(exLocal) + len(exRemote)
	removed, err := loadRemoved(ctx, d, dest, rootID)
	if err != nil {
		return sum, err
	}

	basePath := filepath.Join(st.dir, "synced.json")
	base := map[string]syncEntry{}
	_, statErr := os.Stat(basePath)
	firstSync := errors.Is(statErr, os.ErrNotExist)
	if firstSync {
		// Saved straight away, even on a dry run: it must come from the
		// checksums as the last upload left them, before any are re-read.
		base = seedBase(st, remote)
		saveBase(basePath, base)
		if len(base) > 0 {
			ui.Printf("First sync of this folder: starting from the %d files earlier uploads put on Drive.", len(base))
		} else {
			ui.Println("First sync of this folder: files only on one side are copied to the other; nothing is removed.")
		}
	} else {
		loadJSON(basePath, &base)
	}

	// After seeding: reading a changed file updates its remembered checksum.
	ui.Println("Checking local files...")
	local := map[string]*LocalFile{}
	localMD5 := map[string]string{}
	for _, f := range files {
		m, err := st.MD5(f)
		if err != nil {
			return sum, err
		}
		local[f.Rel], localMD5[f.Rel] = f, m
	}

	plan := planSync(base, local, localMD5, remote, removed)
	sum.InSync, sum.Conflicts = plan.inSync, len(plan.conflicts)
	printSyncPlan(ui, plan, cfg.DryRun)
	if err := checkMassRemoval(len(plan.driveTrash), len(remote), "on Drive", cfg); err != nil {
		return sum, err
	}
	if err := checkMassRemoval(len(plan.localTrash), len(local), "here", cfg); err != nil {
		return sum, err
	}

	if cfg.DryRun {
		ui.Println("Dry run: nothing was changed.")
		return sum, nil
	}
	if plan.empty() {
		finishSync(ui, plan)
		if sum.Conflicts > 0 {
			return sum, ErrProblems
		}
		return sum, nil
	}

	var upBytes, downBytes int64
	for _, f := range plan.upload {
		upBytes += f.Size
	}
	for _, r := range plan.download {
		downBytes += r.Size
	}
	limit, hasLimit := parseInt(about.StorageQuota.Limit)
	usage, _ := parseInt(about.StorageQuota.Usage)
	if hasLimit && upBytes > limit-usage {
		return sum, gdrive.Fatalf("not enough Google Drive space: this needs %s but only %s is free", term.Size(upBytes), term.Size(max(0, limit-usage)))
	}
	if free := freeSpace(src); free >= 0 && downBytes > free {
		return sum, gdrive.Fatalf("not enough disk space: this needs %s but only %s is free", term.Size(downBytes), term.Size(free))
	}

	var failed []string
	fail := func(rel string, err error) {
		slog.Error("sync step failed", "file", rel, "err", err)
		ui.Printf("FAILED  %s: %v", rel, err)
		failed = append(failed, rel)
	}

	// 1. Moved or deleted on Drive: follow here first, so folders emptied
	// here aren't made again on Drive.
	var emptied []string
	for _, m := range plan.localMoves {
		from, to := filepath.Join(src, m.from), filepath.Join(src, m.to.Rel)
		if err := moveLocal(from, to); err != nil {
			fail(m.to.Rel, err)
			continue
		}
		st.Remember(&LocalFile{Rel: m.to.Rel, Size: local[m.from].Size, MtimeNs: local[m.from].MtimeNs}, m.to.MD5)
		emptied = append(emptied, parentDirs(m.from)...)
		sum.MovedHere++
		slog.Info("moved here", "from", m.from, "to", m.to.Rel)
	}
	for _, rel := range plan.localTrash {
		if err := cfg.Trash(filepath.Join(src, rel)); err != nil {
			fail(rel, err)
			continue
		}
		emptied = append(emptied, parentDirs(rel)...)
		sum.TrashedHere++
		slog.Info("moved to the trash here", "file", rel)
	}

	removeEmptyDirs(src, emptied)
	if dirs, _, _, err = scanFolder(src, cfg.Exclude, nil); err != nil {
		return sum, err
	}

	// 2. Tell other devices first: the removed list is saved before anything
	// leaves the Drive folder.
	for _, m := range plan.driveMoves {
		removed.Add(m.from.Rel, m.from.MD5, m.from.Size, "moved")
		removed.Forget(m.to.Rel, m.from.MD5)
	}
	for _, r := range plan.driveTrash {
		removed.Add(r.Rel, r.MD5, r.Size, "deleted")
	}
	for _, r := range plan.replaced {
		removed.Add(r.Rel, r.MD5, r.Size, "replaced")
	}
	for _, f := range plan.upload {
		removed.Forget(f.Rel, localMD5[f.Rel])
	}
	if err := removed.Save(ctx); err != nil {
		return sum, err
	}

	// 3. Folders for everything that goes up.
	t, err := loadTree(ctx, d, dest, dirs, true)
	if err != nil {
		return sum, err
	}

	// 4. Moves and renames on Drive (nothing is uploaded again).
	for _, m := range plan.driveMoves {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if err := d.Move(ctx, m.from.ID, m.to.Name, m.from.Parent, t.ids[m.to.RelDir]); err != nil {
			if isFatal(err) {
				return sum, err
			}
			fail(m.to.Rel, err)
			continue
		}
		sum.MovedOnDrive++
		slog.Info("moved on Drive", "from", m.from.Rel, "to", m.to.Rel)
	}

	// 5. Deleted here: Drive's copy goes to Drive's trash.
	for _, r := range plan.driveTrash {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if err := d.Trash(ctx, r.ID); err != nil {
			if isFatal(err) {
				return sum, err
			}
			fail(r.Rel, err)
			continue
		}
		sum.TrashedOnDrive++
		slog.Info("moved to Drive's trash", "file", r.Rel)
	}

	// 6. Uploads (a changed file's old copy is trashed once the new one checks out).
	if len(plan.upload) > 0 {
		for _, f := range plan.upload {
			f.ParentID = t.ids[f.RelDir]
		}
		ui.Printf("Uploading %d files (%s)...", len(plan.upload), term.Size(upBytes))
		uploaded, failures, _, err := uploadAll(ctx, cfg, d, st, plan.upload, ui)
		sum.Uploaded = uploaded
		failed = append(failed, failures...)
		if err != nil {
			return sum, err
		}
	}

	// 7. Downloads.
	if len(plan.download) > 0 {
		for _, r := range plan.download {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(src, r.Rel)), 0o755); err != nil {
				return sum, &gdrive.LocalError{Err: err}
			}
		}
		ui.Printf("Downloading %d files (%s)...", len(plan.download), term.Size(downBytes))
		downloaded, failures, _, err := downloadAll(ctx, cfg, d, st, src, plan.download, ui)
		sum.Downloaded = downloaded
		failed = append(failed, failures...)
		if err != nil {
			return sum, err
		}
	}

	// 8. Read both sides again: tidy up, check, and save the new record.
	ui.Println("Checking both sides again...")
	_, after, _, err := scanFolder(src, cfg.Exclude, nil)
	if err != nil {
		return sum, err
	}
	remoteAfter, treeAfter, _, err := readRemote(ctx, d, t.ids[""], cfg.Exclude, nil)
	if err != nil {
		return sum, err
	}
	if err := tidyDrive(ctx, d, treeAfter); err != nil {
		return sum, err
	}
	localAfter := map[string]string{}
	for _, f := range after {
		m, err := st.MD5(f)
		if err != nil {
			return sum, err
		}
		localAfter[f.Rel] = m
	}
	var differ []string
	for rel := range unionKeys(localAfter, remoteAfter, base) {
		lm, hasL := localAfter[rel]
		r, hasR := remoteAfter[rel]
		switch {
		case hasL && hasR && lm == r.MD5:
			base[rel] = syncEntry{lm, r.Size}
		case !hasL && !hasR:
			delete(base, rel)
		case !slices.Contains(plan.conflicts, rel):
			differ = append(differ, rel) // keep its old record entry
		}
	}
	saveBase(basePath, base)

	sum.Failed = len(failed)
	finishSync(ui, plan)
	ui.Printf("Done: %d uploaded, %d downloaded, %d moved on Drive, %d moved here, %d to Drive's trash, %d to the trash here.",
		sum.Uploaded, sum.Downloaded, sum.MovedOnDrive, sum.MovedHere, sum.TrashedOnDrive, sum.TrashedHere)
	if len(differ) > 0 {
		slices.Sort(differ)
		ui.Printf("NOT IN SYNC: %d files still differ between here and Drive:", len(differ))
		for i, rel := range differ {
			if i == 50 {
				ui.Printf("  ... and %d more (see the log file)", len(differ)-50)
				break
			}
			ui.Printf("  %s", rel)
		}
		slog.Error("not in sync", "files", differ)
		ui.Println("Run the same command again to retry them.")
		return sum, ErrProblems
	}
	ui.Printf("VERIFIED: here and Drive hold the same %d files.", len(localAfter)-len(plan.conflicts))
	if sum.Failed > 0 || sum.Conflicts > 0 {
		return sum, ErrProblems
	}
	return sum, nil
}

// scanFolder lists a local folder without excluded paths. ui may be nil.
func scanFolder(src string, patterns []string, ui *term.Console) (dirs []string, files []*LocalFile, dropped []string, err error) {
	dirs, files, skipped, err := scan(src)
	if err != nil {
		return nil, nil, nil, gdrive.Fatalf("cannot read %s: %v", src, err)
	}
	files, dropped = excludeItems(files, func(f *LocalFile) string { return f.Rel }, patterns)
	dirs, _ = excludeItems(dirs, func(s string) string { return s }, patterns)
	if ui != nil {
		for _, p := range skipped {
			ui.Printf("Skipping (not a regular file): %s", p)
		}
		var total int64
		for _, f := range files {
			total += f.Size
		}
		ui.Printf("Local folder: %s  (%d files, %s, %d subfolders)", src, len(files), term.Size(total), len(dirs))
		printExcluded(ui, "here", dropped)
	}
	return dirs, files, dropped, nil
}

// readRemote lists the Drive folder without excluded paths. A missing folder
// (rootID "") is empty. ui may be nil.
func readRemote(ctx context.Context, d *gdrive.Drive, rootID string, patterns []string, ui *term.Console) (map[string]remoteFile, *remoteTree, []string, error) {
	out := map[string]remoteFile{}
	if rootID == "" {
		return out, &remoteTree{dirIDs: map[string]string{}, byRel: map[string]remoteFile{}}, nil, nil
	}
	t, err := listTree(ctx, d, rootID)
	if err != nil {
		return nil, nil, nil, err
	}
	var dropped []string
	t.files, dropped = excludeItems(t.files, func(r remoteFile) string { return r.Rel }, patterns)
	for _, r := range t.files {
		out[r.Rel] = r
	}
	if ui != nil {
		for _, s := range t.skipped {
			ui.Printf("Skipping on Drive: %s", s)
		}
		var total int64
		for _, r := range t.files {
			total += r.Size
		}
		ui.Printf("On Drive: %d files, %s.", len(t.files), term.Size(total))
		printExcluded(ui, "on Drive", dropped)
	}
	return out, t, dropped, nil
}

func printExcluded(ui *term.Console, where string, dropped []string) {
	if len(dropped) == 0 {
		return
	}
	ui.Printf("Excluded %s (left alone): %d files", where, len(dropped))
	for i, rel := range dropped {
		if i == 5 {
			ui.Printf("  ... and %d more", len(dropped)-5)
			break
		}
		ui.Printf("  %s", rel)
	}
}

// seedBase starts the record from earlier uploads: files whose checksum
// dhaara remembers for this folder and that are on Drive with that checksum.
func seedBase(st *State, remote map[string]remoteFile) map[string]syncEntry {
	st.mu.Lock()
	defer st.mu.Unlock()
	base := map[string]syncEntry{}
	for rel, e := range st.md5s {
		if r, ok := remote[rel]; ok && r.MD5 == e.MD5 && r.Size == e.Size {
			base[rel] = syncEntry{e.MD5, e.Size}
		}
	}
	return base
}

func saveBase(path string, base map[string]syncEntry) {
	if err := platform.WritePrivateJSON(path, base); err != nil {
		slog.Error("cannot save the sync record", "err", err)
	}
}

func printSyncPlan(ui *term.Console, p syncPlan, dry bool) {
	ui.Printf("In sync: %d files.", p.inSync)
	verb := func(s string) string {
		if dry {
			return "would " + s
		}
		return s
	}
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		ui.Printf("%s: %d", title, len(lines))
		for i, l := range lines {
			if i == 100 {
				ui.Printf("  ... and %d more", len(lines)-100)
				break
			}
			ui.Printf("  %s", l)
		}
	}
	var lines []string
	for _, f := range p.upload {
		what := "new"
		if len(f.ReplaceIDs) > 0 {
			what = "changed"
		}
		lines = append(lines, fmt.Sprintf("%s %s (%s, %s)", verb("upload"), f.Rel, what, term.Size(f.Size)))
	}
	section("Here → Drive", lines)
	lines = nil
	for _, m := range p.driveMoves {
		lines = append(lines, fmt.Sprintf("%s %s → %s", verb("move on Drive"), m.from.Rel, m.to.Rel))
	}
	section("Moved or renamed here", lines)
	lines = nil
	for _, r := range p.driveTrash {
		lines = append(lines, fmt.Sprintf("%s %s", verb("move to Drive's trash"), r.Rel))
	}
	section("Deleted here", lines)
	lines = nil
	for _, r := range p.download {
		lines = append(lines, fmt.Sprintf("%s %s (%s)", verb("download"), r.Rel, term.Size(r.Size)))
	}
	section("Drive → here", lines)
	lines = nil
	for _, m := range p.localMoves {
		lines = append(lines, fmt.Sprintf("%s %s → %s", verb("move here"), m.from, m.to.Rel))
	}
	section("Moved or renamed on Drive", lines)
	lines = nil
	for _, rel := range p.localTrash {
		lines = append(lines, fmt.Sprintf("%s %s", verb("move to the trash here"), rel))
	}
	section("Deleted on Drive", lines)
	if p.empty() && len(p.conflicts) == 0 {
		ui.Println("Nothing to do: here and Drive are the same.")
	}
}

func finishSync(ui *term.Console, p syncPlan) {
	if len(p.conflicts) == 0 {
		return
	}
	ui.Printf("CONFLICTS: %d files changed both here and on Drive; neither copy was touched:", len(p.conflicts))
	for _, rel := range p.conflicts {
		ui.Printf("  %s", rel)
	}
	ui.Println("Keep the version you want here, delete the other copy on Drive's website, then sync again.")
}

// tidyDrive trashes extra identical copies left by a retried upload whose
// reply was lost. It never trashes folders: with the drive.file permission
// dhaara can't see files that other apps put in a folder, and trashing the
// folder would take those too. Emptied folders stay on Drive; pull doesn't
// recreate folders that hold no files.
func tidyDrive(ctx context.Context, d *gdrive.Drive, t *remoteTree) error {
	for _, dup := range t.dups {
		if keep, ok := t.byRel[dup.Rel]; ok && keep.MD5 == dup.MD5 && keep.Size == dup.Size {
			if err := d.Trash(ctx, dup.ID); err != nil {
				return err
			}
			slog.Info("trashed duplicate copy", "file", dup.Rel, "id", dup.ID)
		}
	}
	return nil
}

func unionKeys[A, B, C any](a map[string]A, b map[string]B, c map[string]C) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	for k := range c {
		out[k] = true
	}
	return out
}

// moveLocal renames a file, creating folders as needed. It never overwrites.
func moveLocal(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%s already exists", to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// removeEmptyDirs removes the given folders (and only if empty), deepest first.
func removeEmptyDirs(root string, rels []string) {
	for _, rel := range removeOrder(rels) {
		os.Remove(filepath.Join(root, rel)) // fails, harmlessly, unless empty
	}
}

// checkMassRemoval stops a run that would remove more than a quarter of a
// side's files (at least 20), in case something went badly wrong, unless
// --allow-many-removals was given.
func checkMassRemoval(n, total int, where string, cfg Config) error {
	if cfg.AllowManyRemovals || cfg.DryRun || n < 20 || n*4 <= total {
		return nil
	}
	return gdrive.Fatalf("this would remove %d of the %d files %s. That's a lot, so nothing was changed. "+
		"Check the list above; if it's right, run again with --allow-many-removals", n, total, where)
}

func isFatal(err error) bool {
	var fe *gdrive.FatalError
	return errors.As(err, &fe) || errors.Is(err, context.Canceled)
}

// removeOrder sorts folders deepest first, without repeats.
func removeOrder(rels []string) []string {
	slices.SortFunc(rels, func(a, b string) int { return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b)) })
	return slices.Compact(rels)
}
