package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/platform"
	"github.com/snehilshah/dhaara/internal/term"
)

// Like apt's update and upgrade: sync, pull and phone only read both sides
// and save what they would change as a pending plan; nothing is changed.
// "dhaara pending" shows the plans and "dhaara apply" carries them out.
//
// apply reads both sides again and works the plan out afresh, then does
// only the steps that are identical to the saved ones: the same file, the
// same Drive copy (ID) and the same contents (MD5). A step whose files
// changed since the plan was made is skipped, and anything new waits for
// the next sync, so apply never does something that wasn't reviewed.

// planStep is one change in a plan. Key identifies it exactly (operation,
// paths, Drive ID, MD5s); Line is what the user reads.
type planStep struct {
	Key     string `json:"key"`
	Line    string `json:"line"`
	Removal bool   `json:"removal,omitempty"`
}

type savedPlan struct {
	Kind       string     `json:"kind"` // sync, pull, phone
	Created    time.Time  `json:"created"`
	Source     string     `json:"source"` // local folder
	RemoteName string     `json:"remote"` // Drive folder path
	PhoneDir   string     `json:"phone_dir,omitempty"`
	Steps      []planStep `json:"steps"`
	Conflicts  []string   `json:"conflicts,omitempty"`
	Notes      []string   `json:"notes,omitempty"` // things the plan can't do, for the user

	path string
}

func (p *savedPlan) keys() map[string]bool {
	m := map[string]bool{}
	for _, s := range p.Steps {
		m[s.Key] = true
	}
	return m
}

func (p *savedPlan) removals() int {
	n := 0
	for _, s := range p.Steps {
		if s.Removal {
			n++
		}
	}
	return n
}

func (p *savedPlan) title() string {
	switch p.Kind {
	case "sync":
		return fmt.Sprintf("sync  %s <-> Drive %s", p.Source, p.RemoteName)
	case "pull":
		return fmt.Sprintf("pull  Drive %s -> %s", p.RemoteName, p.Source)
	default:
		return fmt.Sprintf("phone Drive %s -> phone %s (files from %s)", p.RemoteName, p.PhoneDir, p.Source)
	}
}

func plansDir(cfg Config) string { return filepath.Join(cfg.StateDir, "plans") }

func planPath(cfg Config, kind, source, remote, phoneDir string) string {
	k := sha256.Sum256([]byte(kind + "\x00" + source + "\x00" + remote + "\x00" + phoneDir))
	return filepath.Join(plansDir(cfg), kind+"-"+hex.EncodeToString(k[:8])+".json")
}

// savePlan stores the plan (replacing an older one for the same folders) and
// tells the user what is pending. An empty plan removes any old one.
func savePlan(cfg Config, ui *term.Console, p *savedPlan) error {
	p.Created = time.Now().UTC().Truncate(time.Second)
	path := planPath(cfg, p.Kind, p.Source, p.RemoteName, p.PhoneDir)
	if len(p.Steps) == 0 {
		os.Remove(path)
		if len(p.Conflicts) > 0 {
			ui.Println("Nothing to apply, but the conflicts above need you.")
			return ErrProblems
		}
		ui.Println("Up to date: nothing to apply.")
		return nil
	}
	if err := platform.WritePrivateJSON(path, p); err != nil {
		return gdrive.Fatalf("cannot save the plan: %v", err)
	}
	ui.Printf("Pending: %s.", stepCounts(p.Steps))
	ui.Println("Nothing was changed. Review with: dhaara pending    Apply with: dhaara apply")
	if len(p.Conflicts) > 0 {
		return ErrProblems // the plan is saved, but the conflicts need you
	}
	return nil
}

func stepCounts(steps []planStep) string {
	counts := map[string]int{}
	var order []string
	for _, s := range steps {
		op, _, _ := strings.Cut(s.Line, " ")
		if s.Removal {
			op = "remove"
		}
		if counts[op] == 0 {
			order = append(order, op)
		}
		counts[op]++
	}
	var parts []string
	for _, op := range order {
		parts = append(parts, fmt.Sprintf("%s %d", op, counts[op]))
	}
	return strings.Join(parts, ", ")
}

// loadPlans reads the pending plans, optionally only those for one folder.
func loadPlans(cfg Config, source string) ([]*savedPlan, error) {
	entries, err := os.ReadDir(plansDir(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*savedPlan
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(plansDir(cfg), e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		p := &savedPlan{path: path}
		if err := json.Unmarshal(raw, p); err != nil {
			return nil, fmt.Errorf("unreadable plan %s: %v", path, err)
		}
		if source == "" || p.Source == source {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b *savedPlan) int { return a.Created.Compare(b.Created) })
	return out, nil
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func sourceFilter(cfg Config) (string, error) {
	if cfg.Source == "" {
		return "", nil
	}
	return filepath.Abs(cfg.Source)
}

// Pending shows every pending plan in full. It changes nothing.
func Pending(ctx context.Context, cfg Config, ui *term.Console) error {
	src, err := sourceFilter(cfg)
	if err != nil {
		return err
	}
	plans, err := loadPlans(cfg, src)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		ui.Println("Nothing pending. Run dhaara sync, pull or phone to check for changes.")
		return nil
	}
	for i, p := range plans {
		if i > 0 {
			ui.Println("")
		}
		ui.Printf("%s  (checked %s)", p.title(), ago(p.Created))
		ui.Printf("  %s", stepCounts(p.Steps))
		for _, s := range p.Steps {
			ui.Printf("    %s", s.Line)
		}
		for _, c := range p.Conflicts {
			ui.Printf("    conflict, left alone: %s", c)
		}
		for _, n := range p.Notes {
			ui.Printf("    %s", n)
		}
	}
	ui.Println("")
	ui.Println("Apply with: dhaara apply")
	return nil
}

// Apply carries out pending plans, after showing each and asking (unless
// cfg.Yes). Each plan is removed once applied; a stopped one stays, and
// running apply again continues it.
func Apply(ctx context.Context, cfg Config, ui *term.Console) error {
	src, err := sourceFilter(cfg)
	if err != nil {
		return err
	}
	plans, err := loadPlans(cfg, src)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		ui.Println("Nothing to apply. Run dhaara sync, pull or phone first.")
		return nil
	}
	var problems bool
	for _, p := range plans {
		ui.Printf("%s  (checked %s)", p.title(), ago(p.Created))
		ui.Printf("  %s", stepCounts(p.Steps))
		shown := 0
		for _, s := range p.Steps { // removals first: they matter most
			if s.Removal && shown < 100 {
				ui.Printf("    %s", s.Line)
				shown++
			}
		}
		for _, s := range p.Steps {
			if !s.Removal && shown < 100 {
				ui.Printf("    %s", s.Line)
				shown++
			}
		}
		if len(p.Steps) > shown {
			ui.Printf("    ... and %d more (see dhaara pending)", len(p.Steps)-shown)
		}
		if !cfg.Yes {
			if cfg.Confirm == nil {
				return gdrive.Fatalf("nothing was applied: there is no terminal to ask in. Add --yes to apply without asking")
			}
			if !cfg.Confirm("Apply these changes? [y/N] ") {
				ui.Println("Skipped; the plan is still pending.")
				continue
			}
		}

		run := cfg
		run.Mode, run.Source, run.RemoteName, run.PhoneDir = p.Kind, p.Source, p.RemoteName, p.PhoneDir
		run.applying = p
		switch p.Kind {
		case "sync":
			_, err = Sync(ctx, run, ui)
		case "pull":
			run.Download, run.Pull = true, true
			_, err = Download(ctx, run, ui)
		case "phone":
			err = Phone(ctx, run, ui)
		default:
			err = fmt.Errorf("unknown plan kind %q in %s", p.Kind, p.path)
		}
		if ctx.Err() != nil {
			return ctx.Err() // keep the plan: apply again to continue
		}
		var fe *gdrive.FatalError
		if errors.As(err, &fe) && !errors.Is(err, ErrProblems) {
			return err // nothing (or not everything) happened; keep the plan
		}
		os.Remove(p.path)
		if err != nil {
			problems = true
			ui.Printf("Finished with problems: %v", err)
		}
	}
	if problems {
		return ErrProblems
	}
	return nil
}

// matchSteps compares the plan apply just worked out (fresh) with the saved
// one. keep reports the fresh steps to carry out. It tells the user about
// saved steps that no longer hold and about changes that are new.
func matchSteps(ui *term.Console, saved *savedPlan, fresh []planStep) (keep map[string]bool) {
	want := saved.keys()
	keep = map[string]bool{}
	var newer int
	for _, s := range fresh {
		if want[s.Key] {
			keep[s.Key] = true
		} else {
			newer++
		}
	}
	var stale []string
	for _, s := range saved.Steps {
		if !keep[s.Key] {
			stale = append(stale, s.Line)
		}
	}
	if len(stale) > 0 {
		ui.Printf("SKIPPED: %d planned changes no longer match (the files changed since the check):", len(stale))
		for i, l := range stale {
			if i == 20 {
				ui.Printf("  ... and %d more", len(stale)-20)
				break
			}
			ui.Printf("  %s", l)
		}
	}
	if newer > 0 {
		ui.Printf("Not applied: %d changes appeared since the check. Run the check again to see them.", newer)
	}
	return keep
}

func stepKey(parts ...string) string { return strings.Join(parts, "\x00") }

// syncSteps describes a sync plan as steps.
func syncSteps(p syncPlan, localMD5 map[string]string) []planStep {
	var out []planStep
	for _, f := range p.upload {
		if len(f.ReplaceIDs) > 0 {
			out = append(out, planStep{Key: stepKey("replace-on-drive", f.Rel, localMD5[f.Rel], strings.Join(f.ReplaceIDs, ",")),
				Line: fmt.Sprintf("upload %s (changed, %s)", f.Rel, term.Size(f.Size))})
		} else {
			out = append(out, planStep{Key: stepKey("upload", f.Rel, localMD5[f.Rel]),
				Line: fmt.Sprintf("upload %s (new, %s)", f.Rel, term.Size(f.Size))})
		}
	}
	for _, m := range p.driveMoves {
		out = append(out, planStep{Key: stepKey("move-on-drive", m.from.Rel, m.to.Rel, m.from.ID, m.from.MD5),
			Line: fmt.Sprintf("move on Drive %s → %s", m.from.Rel, m.to.Rel)})
	}
	for _, r := range p.driveTrash {
		out = append(out, planStep{Key: stepKey("trash-on-drive", r.Rel, r.ID, r.MD5),
			Line: fmt.Sprintf("remove from Drive (to Drive's trash) %s", r.Rel), Removal: true})
	}
	for _, r := range p.download {
		out = append(out, planStep{Key: stepKey("download", r.Rel, r.ID, r.MD5, localMD5[r.Rel]),
			Line: fmt.Sprintf("download %s (%s)", r.Rel, term.Size(r.Size))})
	}
	for _, m := range p.localMoves {
		out = append(out, planStep{Key: stepKey("move-here", m.from, m.to.Rel, m.to.ID, m.to.MD5),
			Line: fmt.Sprintf("move here %s → %s", m.from, m.to.Rel)})
	}
	for _, rel := range p.localTrash {
		out = append(out, planStep{Key: stepKey("trash-here", rel, localMD5[rel]),
			Line: fmt.Sprintf("remove here (to the trash) %s", rel), Removal: true})
	}
	return out
}

// filterSync keeps only the steps in keep.
func filterSync(p syncPlan, localMD5 map[string]string, keep map[string]bool) syncPlan {
	steps := syncSteps(p, localMD5)
	ok := func(i int) bool { return keep[steps[i].Key] }
	out := syncPlan{conflicts: p.conflicts, inSync: p.inSync}
	i := 0
	keepRel := map[string]bool{}
	for _, f := range p.upload {
		if ok(i) {
			out.upload = append(out.upload, f)
			keepRel[f.Rel] = true
		}
		i++
	}
	for _, r := range p.replaced {
		if keepRel[r.Rel] {
			out.replaced = append(out.replaced, r)
		}
	}
	for _, m := range p.driveMoves {
		if ok(i) {
			out.driveMoves = append(out.driveMoves, m)
		}
		i++
	}
	for _, r := range p.driveTrash {
		if ok(i) {
			out.driveTrash = append(out.driveTrash, r)
		}
		i++
	}
	for _, r := range p.download {
		if ok(i) {
			out.download = append(out.download, r)
		}
		i++
	}
	for _, m := range p.localMoves {
		if ok(i) {
			out.localMoves = append(out.localMoves, m)
		}
		i++
	}
	for _, rel := range p.localTrash {
		if ok(i) {
			out.localTrash = append(out.localTrash, rel)
		}
		i++
	}
	return out
}

// mirrorSteps describes a pull or phone plan as steps. fetchVerb is
// "download" or "copy to the phone"; where is "here" or "on the phone".
func mirrorSteps(p mirrorPlan, md5 map[string]string, fetchVerb, where string) []planStep {
	var out []planStep
	for _, m := range p.moves {
		out = append(out, planStep{Key: stepKey("move", m.From, m.To.Rel, md5[m.From]),
			Line: fmt.Sprintf("move %s %s → %s", where, m.From, m.To.Rel)})
	}
	for _, rel := range p.remove {
		out = append(out, planStep{Key: stepKey("remove", rel, md5[rel]),
			Line: fmt.Sprintf("remove %s %s", where, rel), Removal: true})
	}
	for _, r := range p.fetch {
		what, old := "new", ""
		if p.replace[r.Rel] {
			what, old = "replaces the old version", md5[r.Rel]
		}
		out = append(out, planStep{Key: stepKey("fetch", r.Rel, r.MD5, old),
			Line: fmt.Sprintf("%s %s (%s, %s)", fetchVerb, r.Rel, what, term.Size(r.Size))})
	}
	return out
}

func filterMirror(p mirrorPlan, md5 map[string]string, keep map[string]bool) mirrorPlan {
	steps := mirrorSteps(p, md5, "", "")
	ok := func(i int) bool { return keep[steps[i].Key] }
	out := mirrorPlan{replace: map[string]bool{}, conflicts: p.conflicts, same: p.same}
	i := 0
	for _, m := range p.moves {
		if ok(i) {
			out.moves = append(out.moves, m)
		}
		i++
	}
	for _, rel := range p.remove {
		if ok(i) {
			out.remove = append(out.remove, rel)
		}
		i++
	}
	for _, r := range p.fetch {
		if ok(i) {
			out.fetch = append(out.fetch, r)
			if p.replace[r.Rel] {
				out.replace[r.Rel] = true
			}
		}
		i++
	}
	return out
}
