package engine

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/term"
)

// pullRemovalsAndMoves is the part of "dhaara pull" that plain download
// doesn't do: it follows the removed list (moves, renames, removals, and old
// versions to replace) before the usual download runs. It fills replaceOK
// with the paths the download may overwrite. done is true after a dry run.
func pullRemovalsAndMoves(ctx context.Context, cfg Config, ui *term.Console, d *gdrive.Drive, st *State, target, rootID string,
	files []remoteFile, replaceOK map[string]bool) (done bool, err error) {
	removed, err := loadRemoved(ctx, d, cfg.RemoteName, rootID)
	if err != nil {
		return false, err
	}
	remote := map[string]remoteFile{}
	for _, rf := range files {
		remote[rf.Rel] = rf
	}
	local := map[string]mirrorLocal{}
	byRel := map[string]*LocalFile{}
	if fi, err := os.Stat(target); err == nil && fi.IsDir() {
		_, lf, _, err := scanFolder(target, cfg.Exclude, nil)
		if err != nil {
			return false, err
		}
		for _, f := range lf {
			local[f.Rel] = mirrorLocal{f.Size, f.MtimeNs}
			byRel[f.Rel] = f
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, &gdrive.LocalError{Err: err}
	}
	md5s := map[string]string{}
	for _, rel := range mirrorNeeds(remote, local, removed) {
		if md5s[rel], err = st.MD5(byRel[rel]); err != nil {
			return false, err
		}
	}
	plan := planMirror(remote, local, md5s, removed, cfg.Overwrite)
	printMirrorPlan(ui, plan, cfg.DryRun, "download", "here")
	if err := checkMassRemoval(len(plan.remove), len(local), "here", cfg); err != nil {
		return false, err
	}
	if cfg.DryRun {
		ui.Println("Dry run: nothing was downloaded or changed.")
		return true, nil
	}

	var emptied []string
	for _, m := range plan.moves {
		if err := moveLocal(filepath.Join(target, m.From), filepath.Join(target, m.To.Rel)); err != nil {
			ui.Printf("FAILED  moving %s: %v (it will be downloaded instead)", m.From, err)
			slog.Error("move failed", "from", m.From, "to", m.To.Rel, "err", err)
			continue
		}
		f := byRel[m.From]
		st.Remember(&LocalFile{Rel: m.To.Rel, Size: f.Size, MtimeNs: f.MtimeNs}, md5s[m.From])
		emptied = append(emptied, parentDirs(m.From)...)
		slog.Info("moved", "from", m.From, "to", m.To.Rel)
	}
	for _, rel := range plan.remove {
		if err := cfg.Trash(filepath.Join(target, rel)); err != nil {
			ui.Printf("FAILED  removing %s: %v", rel, err)
			slog.Error("remove failed", "file", rel, "err", err)
			continue
		}
		emptied = append(emptied, parentDirs(rel)...)
		slog.Info("moved to the trash", "file", rel)
	}
	removeEmptyDirs(target, emptied)
	if n := len(plan.moves) + len(plan.remove); n > 0 {
		ui.Printf("Followed Drive: %d moved, %d moved to the trash.", len(plan.moves), len(plan.remove))
	}
	for rel := range plan.replace {
		replaceOK[rel] = true
	}
	return false, nil
}
