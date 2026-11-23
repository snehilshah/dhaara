package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/platform"
	"github.com/snehilshah/dhaara/internal/term"
)

// A pairing names a folder here and a folder on Drive that are kept in
// step, plus any phones that follow Drive. It is set up once with "dhaara
// add" and stored in config.json, per device (never synced). Day to day,
// "dhaara status" checks every place of every pairing and saves one plan
// per pairing; "dhaara apply" carries it out, stage by stage: first here
// and Drive, then each phone.
//
// Roles: "sync" keeps here and Drive in step both ways (the laptop where
// the folder is curated). "follow" only takes Drive's changes (another
// laptop). Phones always follow Drive; their files come from the folder
// here, which must match Drive.

// ErrPending means changes are waiting to be applied (status --exit-code).
var ErrPending = errors.New("changes are pending")

type Pairing struct {
	Local   string        `json:"local"`
	Drive   string        `json:"drive"`
	DriveID string        `json:"drive_id,omitempty"` // pinned once the folder exists
	Role    string        `json:"role"`               // sync or follow
	Phones  []PhoneTarget `json:"phones,omitempty"`
	Exclude []string      `json:"exclude,omitempty"`
}

type PhoneTarget struct {
	Dir    string `json:"dir"`
	Serial string `json:"serial,omitempty"`
}

type pairingsFile struct {
	Version  int                 `json:"version"`
	Pairings map[string]*Pairing `json:"pairings"`
}

func configPath(cfg Config) string { return filepath.Join(cfg.ConfigDir, "config.json") }

func loadPairings(cfg Config) (*pairingsFile, error) {
	ps := &pairingsFile{Version: 1, Pairings: map[string]*Pairing{}}
	b, err := os.ReadFile(configPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return ps, nil
	}
	if err != nil {
		return nil, gdrive.Fatalf("cannot read %s: %v", configPath(cfg), err)
	}
	if err := json.Unmarshal(b, ps); err != nil {
		return nil, gdrive.Fatalf("cannot read %s: %v. Fix the file, or remove it and add the pairings again", configPath(cfg), err)
	}
	if ps.Version != 1 {
		return nil, gdrive.Fatalf("%s is from a newer dhaara (version %d); update dhaara", configPath(cfg), ps.Version)
	}
	if ps.Pairings == nil {
		ps.Pairings = map[string]*Pairing{}
	}
	return ps, nil
}

func (ps *pairingsFile) save(cfg Config) error {
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return gdrive.Fatalf("cannot create %s: %v", cfg.ConfigDir, err)
	}
	if err := platform.WritePrivateJSON(configPath(cfg), ps); err != nil {
		return gdrive.Fatalf("cannot save %s: %v", configPath(cfg), err)
	}
	return nil
}

// selectPairings returns the pairing names to work on: one, or all.
func (ps *pairingsFile) selectPairings(name string) ([]string, error) {
	if name != "" {
		if ps.Pairings[name] == nil {
			return nil, gdrive.Fatalf("no pairing called %q. %s", name, ps.known())
		}
		return []string{name}, nil
	}
	names := make([]string, 0, len(ps.Pairings))
	for n := range ps.Pairings {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func (ps *pairingsFile) known() string {
	if len(ps.Pairings) == 0 {
		return "There are none yet; add one with: dhaara add NAME FOLDER drive:FOLDER"
	}
	names, _ := ps.selectPairings("")
	return "Pairings: " + strings.Join(names, ", ")
}

var pairingName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// location is one argument of "dhaara add": a folder here, drive:PATH or
// phone:/PATH.
type location struct{ kind, path string }

func parseLocation(arg string) (location, error) {
	switch {
	case strings.HasPrefix(arg, "drive:"):
		p := strings.Trim(strings.TrimPrefix(arg, "drive:"), "/")
		if _, err := gdrive.SplitDrivePath(p); err != nil {
			return location{}, err
		}
		return location{"drive", p}, nil
	case strings.HasPrefix(arg, "phone:"):
		p := strings.TrimPrefix(arg, "phone:")
		if !strings.HasPrefix(p, "/") || filepath.Clean(p) != strings.TrimRight(p, "/") && p != "/" {
			return location{}, fmt.Errorf("a phone folder must be a full path, like phone:/sdcard/Music (got %q)", arg)
		}
		return location{"phone", filepath.Clean(p)}, nil
	default:
		p, err := filepath.Abs(arg)
		if err != nil {
			return location{}, err
		}
		return location{"local", p}, nil
	}
}

// AddOptions are the choices of "dhaara add".
type AddOptions struct {
	Follow  bool
	Exclude []string
	Serial  string // for a phone
}

// Add sets up a pairing, or adds phones to one.
func Add(ctx context.Context, cfg Config, ui *term.Console, name string, args []string, opt AddOptions) error {
	if !pairingName.MatchString(name) {
		return gdrive.Fatalf("%q can't be a pairing name: use letters, digits, '.', '_' or '-', like music", name)
	}
	var local, drive string
	var phones []string
	for _, a := range args {
		loc, err := parseLocation(a)
		if err != nil {
			return gdrive.Fatalf("%v", err)
		}
		switch {
		case loc.kind == "local" && local == "":
			local = loc.path
		case loc.kind == "drive" && drive == "":
			drive = loc.path
		case loc.kind == "phone":
			phones = append(phones, loc.path)
		default:
			return gdrive.Fatalf("a pairing has one folder here and one on Drive; %q is a second one", a)
		}
	}
	ps, err := loadPairings(cfg)
	if err != nil {
		return err
	}
	p := ps.Pairings[name]
	if p == nil {
		if local == "" || drive == "" {
			return gdrive.Fatalf("a new pairing needs a folder here and one on Drive, like: dhaara add %s ~/Music drive:Music", name)
		}
		for other, q := range ps.Pairings {
			if q.Local == local {
				return gdrive.Fatalf("%s is already in the pairing %q", tildePath(local), other)
			}
			if strings.EqualFold(q.Drive, drive) {
				return gdrive.Fatalf("drive:%s is already in the pairing %q", drive, other)
			}
		}
		role := "sync"
		if opt.Follow {
			role = "follow"
		}
		if fi, err := os.Stat(local); err == nil && !fi.IsDir() {
			return gdrive.Fatalf("not a folder: %s", local)
		} else if err != nil && role == "sync" {
			return gdrive.Fatalf("no folder at %s. To fill a new folder from Drive, add --follow", local)
		}
		d, _, err := connect(ctx, cfg, ui)
		if err != nil {
			return err
		}
		id, _, err := gdrive.ResolveFolder(ctx, d.NewListings(), drive, false)
		if err != nil {
			return err
		}
		if id == "" && role == "follow" {
			return gdrive.Fatalf("no folder drive:%s that dhaara can see. dhaara only sees folders it created itself", drive)
		}
		p = &Pairing{Local: local, Drive: drive, DriveID: id, Role: role, Exclude: opt.Exclude}
		ps.Pairings[name] = p
	} else {
		if local != "" || drive != "" {
			return gdrive.Fatalf("%q already pairs %s with drive:%s. To change that: dhaara forget %s, then add it again",
				name, tildePath(p.Local), p.Drive, name)
		}
		if opt.Follow || len(opt.Exclude) > 0 {
			return gdrive.Fatalf("--follow and --exclude are set when a pairing is first added; to change them: dhaara forget %s, then add it again", name)
		}
		if len(phones) == 0 {
			return gdrive.Fatalf("nothing to add to %q; give a phone folder, like phone:/sdcard/Music", name)
		}
	}
	for _, dir := range phones {
		for _, have := range p.Phones {
			if have.Dir == dir && have.Serial == opt.Serial {
				return gdrive.Fatalf("phone:%s is already in %q", dir, name)
			}
		}
		p.Phones = append(p.Phones, PhoneTarget{Dir: dir, Serial: opt.Serial})
	}
	if err := ps.save(cfg); err != nil {
		return err
	}
	describePairing(ui, name, p)
	ui.Printf("Saved in %s. Next: dhaara status %s", configPath(cfg), name)
	return nil
}

// Forget removes a pairing, or one of its phones, from config.json. No
// files are touched anywhere, and the checksums dhaara remembers stay, so
// adding the same folders again carries on where it was.
func Forget(cfg Config, ui *term.Console, name string, args []string) error {
	ps, err := loadPairings(cfg)
	if err != nil {
		return err
	}
	p := ps.Pairings[name]
	if p == nil {
		return gdrive.Fatalf("no pairing called %q. %s", name, ps.known())
	}
	if len(args) == 0 {
		delete(ps.Pairings, name)
		os.Remove(pairingPlanPath(cfg, name))
		if err := ps.save(cfg); err != nil {
			return err
		}
		ui.Printf("Forgot %q (%s <-> drive:%s). No files were touched.", name, tildePath(p.Local), p.Drive)
		return nil
	}
	for _, a := range args {
		loc, err := parseLocation(a)
		if err != nil || loc.kind != "phone" {
			return gdrive.Fatalf("only a phone can be taken out of a pairing (like phone:/sdcard/Music); to forget %q entirely: dhaara forget %s", name, name)
		}
		i := slices.IndexFunc(p.Phones, func(t PhoneTarget) bool { return t.Dir == loc.path })
		if i < 0 {
			return gdrive.Fatalf("%q has no phone:%s", name, loc.path)
		}
		p.Phones = slices.Delete(p.Phones, i, i+1)
		ui.Printf("Took phone:%s out of %q. Nothing on the phone was touched.", loc.path, name)
	}
	os.Remove(pairingPlanPath(cfg, name))
	return ps.save(cfg)
}

func describePairing(ui *term.Console, name string, p *Pairing) {
	if p.Role == "follow" {
		ui.Printf("%s  drive:%s -> %s  (follows Drive)", name, p.Drive, tildePath(p.Local))
	} else {
		ui.Printf("%s  %s <-> drive:%s  (two-way)", name, tildePath(p.Local), p.Drive)
	}
	for _, ph := range p.Phones {
		serial := ""
		if ph.Serial != "" {
			serial = ", phone " + ph.Serial
		}
		ui.Printf("  phone:%s  (follows Drive, files from %s%s)", ph.Dir, tildePath(p.Local), serial)
	}
	if len(p.Exclude) > 0 {
		ui.Printf("  leaves out: %s", strings.Join(p.Exclude, ", "))
	}
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
			return "~/" + rest
		}
	}
	return p
}

// pairingPlan is what "dhaara status" found for one pairing: its stages,
// in the order apply runs them.
type pairingPlan struct {
	Pairing string       `json:"pairing"`
	Created time.Time    `json:"created"`
	Stages  []*savedPlan `json:"stages"`
	Skipped []string     `json:"skipped,omitempty"` // places that couldn't be checked, and why

	problems bool
}

func pairingPlanPath(cfg Config, name string) string {
	return filepath.Join(plansDir(cfg), "pairing-"+name+".json")
}

func (pp *pairingPlan) steps() int {
	n := 0
	for _, s := range pp.Stages {
		n += len(s.Steps)
	}
	return n
}

func loadPairingPlan(cfg Config, name string) *pairingPlan {
	b, err := os.ReadFile(pairingPlanPath(cfg, name))
	if err != nil {
		return nil
	}
	var pp pairingPlan
	if json.Unmarshal(b, &pp) != nil {
		return nil
	}
	return &pp
}

func savePairingPlan(cfg Config, pp *pairingPlan) error {
	path := pairingPlanPath(cfg, pp.Pairing)
	if pp.steps() == 0 {
		os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(plansDir(cfg), 0o700); err != nil {
		return gdrive.Fatalf("cannot save the plan: %v", err)
	}
	if err := platform.WritePrivateJSON(path, pp); err != nil {
		return gdrive.Fatalf("cannot save the plan: %v", err)
	}
	return nil
}

// stageConfig is the run settings for one stage of pairing p.
func stageConfig(cfg Config, p *Pairing) Config {
	run := cfg
	run.Source, run.RemoteName = p.Local, p.Drive
	run.Exclude = append(slices.Clone(cfg.Exclude), p.Exclude...)
	run.collect, run.basedOn, run.readyAfter, run.predict, run.applying = nil, nil, nil, nil, nil
	return run
}

// checkPinned makes sure drive:PATH is still the folder the pairing was
// set up with, and pins it once it exists.
func checkPinned(ctx context.Context, cfg Config, d *gdrive.Drive, ps *pairingsFile, name string) error {
	p := ps.Pairings[name]
	id, _, err := gdrive.ResolveFolder(ctx, d.NewListings(), p.Drive, false)
	if err != nil {
		return err
	}
	switch {
	case p.DriveID != "" && id != p.DriveID:
		what := "is gone"
		if id != "" {
			what = "now leads to a different folder (another folder with that name?)"
		}
		return gdrive.Fatalf("drive:%s %s: it isn't the folder %q was set up with. Nothing was changed. "+
			"If that's expected: dhaara forget %s, then add it again", p.Drive, what, name, name)
	case p.DriveID == "" && id != "":
		p.DriveID = id
		return ps.save(cfg)
	}
	return nil
}

// checkPairing works out every stage of pairing name. Stages print to out
// (the details, shown with status -v).
func checkPairing(ctx context.Context, cfg Config, out *term.Console, name string, p *Pairing) (*pairingPlan, error) {
	pp := &pairingPlan{Pairing: name}
	base := stageConfig(cfg, p)

	first := &savedPlan{}
	var pred drivePrediction
	run := base
	run.collect = first
	var err error
	if p.Role == "follow" {
		run.Mode, run.Download, run.Pull = "pull", true, true
		_, err = Download(ctx, run, out)
	} else {
		run.Mode, run.predict = "sync", &pred
		_, err = Sync(ctx, run, out)
	}
	if errors.Is(err, ErrProblems) {
		pp.problems = true
	} else if err != nil {
		return nil, err
	}
	if first.Kind == "" {
		return nil, fmt.Errorf("internal error: the check of %s made no plan", name)
	}
	pp.Stages = append(pp.Stages, first)

	// Paths the first stage brings in step with Drive here: the phone can
	// count on them, though the copy here doesn't match yet.
	ready := map[string]bool{}
	for _, s := range first.Steps {
		ready[s.Path] = true
		if s.To != "" {
			ready[s.To] = true
		}
	}
	for _, ph := range p.Phones {
		where := "phone:" + ph.Dir
		run := base
		run.Mode, run.PhoneDir, run.PhoneSerial = "phone", ph.Dir, ph.Serial
		dev, err := newPhoneDevice(run)
		if err == nil {
			err = dev.Check(ctx)
		}
		if err != nil {
			pp.Skipped = append(pp.Skipped, fmt.Sprintf("%s: not checked: %v", where, err))
			continue
		}
		st := &savedPlan{}
		run.collect, run.readyAfter = st, ready
		if p.Role != "follow" {
			run.basedOn = &pred
		}
		err = Phone(ctx, run, out)
		if errors.Is(err, ErrProblems) {
			pp.problems = true
		} else if err != nil {
			return nil, err
		}
		if len(st.Notes) > 0 {
			pp.problems = true
		}
		pp.Stages = append(pp.Stages, st)
	}
	pp.Created = time.Now().UTC().Truncate(time.Second)
	return pp, nil
}

func stageName(s *savedPlan) string {
	switch s.Kind {
	case "sync":
		return "here <-> Drive"
	case "pull":
		return "Drive -> here"
	default:
		return "phone:" + s.PhoneDir
	}
}

// printPairingPlan shows what's pending. Removals are always listed; with
// all set, every step is. Steps not in old (the plan saved by the last
// status) are marked.
func printPairingPlan(ui *term.Console, p *Pairing, pp *pairingPlan, all bool, old *pairingPlan) {
	describePairing(ui, pp.Pairing, p)
	known := map[string]bool{}
	if old != nil {
		for _, s := range old.Stages {
			for _, st := range s.Steps {
				known[st.Key] = true
			}
		}
	}
	for _, s := range pp.Stages {
		if len(s.Steps) == 0 && len(s.Conflicts) == 0 && len(s.Notes) == 0 {
			ui.Printf("  %s: nothing to do", stageName(s))
			continue
		}
		counts := "nothing to apply"
		if len(s.Steps) > 0 {
			counts = stepCounts(s.Steps)
		}
		ui.Printf("  %s: %s", stageName(s), counts)
		shown := 0
		for _, st := range s.Steps {
			if !(st.Removal || all) {
				continue
			}
			if shown == 20 && !all {
				ui.Printf("      ... and more (see dhaara status -v)")
				break
			}
			mark := ""
			if old != nil && !known[st.Key] {
				mark = "  (new since the last status)"
			}
			ui.Printf("      %s%s", st.Line, mark)
			shown++
		}
		for i, c := range s.Conflicts {
			if i == 20 && !all {
				ui.Printf("      ... and %d more conflicts", len(s.Conflicts)-20)
				break
			}
			ui.Printf("      CONFLICT, left alone: %s", c)
		}
		for i, n := range s.Notes {
			if i == 20 && !all {
				ui.Printf("      ... and %d more", len(s.Notes)-20)
				break
			}
			ui.Printf("      %s", n)
		}
	}
	for _, s := range pp.Skipped {
		ui.Printf("  %s", s)
	}
}

// Status checks every place of the pairings (or of just one) and saves what
// is pending. It changes nothing.
func Status(ctx context.Context, cfg Config, ui *term.Console, name string, verbose, exitCode bool) error {
	ps, err := loadPairings(cfg)
	if err != nil {
		return err
	}
	names, err := ps.selectPairings(name)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		ui.Println("No pairings yet. Add one, like: dhaara add music ~/Music drive:Music")
		return nil
	}
	d, _, err := connect(ctx, cfg, ui) // signs in here, where the user can see it
	if err != nil {
		return err
	}
	out := term.NewPlain(io.Discard)
	if verbose {
		out = ui
	}
	var pending, problems, skipped bool
	for _, n := range names {
		if err := checkPinned(ctx, cfg, d, ps, n); err != nil {
			return err
		}
		ui.Printf("Checking %s...", n)
		pp, err := checkPairing(ctx, cfg, out, n, ps.Pairings[n])
		if err != nil {
			return err
		}
		printPairingPlan(ui, ps.Pairings[n], pp, verbose, nil)
		if err := savePairingPlan(cfg, pp); err != nil {
			return err
		}
		pending = pending || pp.steps() > 0
		problems = problems || pp.problems
		skipped = skipped || len(pp.Skipped) > 0
	}
	switch {
	case pending:
		ui.Println("Nothing was changed. Details: dhaara status -v    Apply: dhaara apply")
	case problems:
	case skipped:
		ui.Println("Up to date everywhere it could check (see \"not checked\" above).")
	default:
		ui.Println("Everything is up to date.")
	}
	switch {
	case problems:
		return ErrProblems
	case pending && exitCode:
		return ErrPending
	}
	return nil
}

// ApplyPairings carries out the pairings' plans. Asking first (the usual
// way), it checks everything again, shows the fresh plan and asks once per
// pairing. With cfg.Yes it does exactly the plan the last status saved:
// steps that no longer hold are skipped, new ones wait for the next status.
func ApplyPairings(ctx context.Context, cfg Config, ui *term.Console, name string) error {
	ps, err := loadPairings(cfg)
	if err != nil {
		return err
	}
	names, err := ps.selectPairings(name)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		ui.Println("No pairings yet. Add one, like: dhaara add music ~/Music drive:Music")
		return nil
	}
	if !cfg.Yes && cfg.Confirm == nil {
		return gdrive.Fatalf("nothing was applied: there is no terminal to ask in. Add --yes to apply the plan the last dhaara status saved")
	}
	d, _, err := connect(ctx, cfg, ui)
	if err != nil {
		return err
	}
	var problems bool
	for _, n := range names {
		p := ps.Pairings[n]
		if err := checkPinned(ctx, cfg, d, ps, n); err != nil {
			return err
		}
		saved := loadPairingPlan(cfg, n)
		var pp *pairingPlan
		if cfg.Yes {
			if saved == nil {
				ui.Printf("%s: nothing pending. (dhaara status finds what's new.)", n)
				continue
			}
			pp = saved
			printPairingPlan(ui, p, pp, false, nil)
		} else {
			ui.Printf("Checking %s again...", n)
			if pp, err = checkPairing(ctx, cfg, term.NewPlain(io.Discard), n, p); err != nil {
				return err
			}
			printPairingPlan(ui, p, pp, false, saved)
			if pp.steps() == 0 {
				os.Remove(pairingPlanPath(cfg, n))
				if pp.problems {
					problems = true
					ui.Println("Nothing to apply, but the conflicts above need you.")
				} else {
					ui.Println("Up to date: nothing to apply.")
				}
				continue
			}
			if !cfg.Confirm("Apply these changes? [y/N] ") {
				if err := savePairingPlan(cfg, pp); err != nil {
					return err
				}
				ui.Println("Skipped; the plan is still pending.")
				continue
			}
		}
		if err := checkStageRemovals(cfg, pp); err != nil {
			return err
		}
		stageProblems, err := runPairing(ctx, cfg, ui, p, pp)
		if err != nil {
			return err // stopped, or fatal: the plan stays pending
		}
		os.Remove(pairingPlanPath(cfg, n))
		if err := checkPinned(ctx, cfg, d, ps, n); err != nil { // a first sync made the folder
			return err
		}
		problems = problems || stageProblems || pp.problems
	}
	if problems {
		return ErrProblems
	}
	return nil
}

// checkStageRemovals stops before anything runs if any stage would remove
// too many files, so a run never stops half way for that.
func checkStageRemovals(cfg Config, pp *pairingPlan) error {
	for _, s := range pp.Stages {
		n := map[string]int{}
		for _, st := range s.Steps {
			if st.Removal {
				n[st.RunsOn]++
			}
		}
		for on, k := range n {
			where := map[string]string{"drive": "on Drive", "here": "here", "phone": "on phone:" + s.PhoneDir}[on]
			if err := checkMassRemoval(k, s.Totals[on], where, cfg); err != nil {
				return err
			}
		}
	}
	return nil
}

// runPairing runs the stages in order. A stage that stops with a fatal
// error stops the rest.
func runPairing(ctx context.Context, cfg Config, ui *term.Console, p *Pairing, pp *pairingPlan) (problems bool, err error) {
	for i, s := range pp.Stages {
		ui.Printf("[%d/%d] %s", i+1, len(pp.Stages), stageName(s))
		if len(s.Steps) == 0 {
			ui.Println("  nothing to do")
			continue
		}
		run := stageConfig(cfg, p)
		if s.Kind == "phone" {
			run.PhoneDir, run.PhoneSerial = s.PhoneDir, s.PhoneSerial
			dev, err := newPhoneDevice(run)
			if err == nil {
				err = dev.Check(ctx)
			}
			if err != nil {
				ui.Printf("  skipped, the phone isn't connected: %v", err)
				problems = true
				continue
			}
		}
		err := runStage(ctx, run, ui, s)
		if ctx.Err() != nil {
			return problems, ctx.Err()
		}
		var fe *gdrive.FatalError
		if errors.As(err, &fe) && !errors.Is(err, ErrProblems) {
			return problems, err
		}
		if err != nil {
			problems = true
			ui.Printf("  finished with problems: %v", err)
		}
	}
	return problems, nil
}

// runStage applies one saved plan, holding the Drive folder's lock.
func runStage(ctx context.Context, cfg Config, ui *term.Console, p *savedPlan) error {
	release, err := lockFolder(cfg, p.RemoteName, "apply")
	if err != nil {
		return err
	}
	defer release()
	run := cfg
	run.Mode, run.Source, run.RemoteName, run.PhoneDir = p.Kind, p.Source, p.RemoteName, p.PhoneDir
	if p.PhoneSerial != "" {
		run.PhoneSerial = p.PhoneSerial
	}
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
		err = fmt.Errorf("unknown plan kind %q", p.Kind)
	}
	return err
}
