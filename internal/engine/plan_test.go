package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/term"
)

func (e *env) apply(ctx context.Context, change func(*Config)) error {
	e.t.Helper()
	cfg := e.cfg
	cfg.Mode, cfg.Source = "apply", "" // every pending plan, as plain "dhaara apply"
	if change != nil {
		change(&cfg)
	}
	return Apply(ctx, cfg, term.NewPlain(e.out))
}

func (e *env) pendingText() string {
	e.t.Helper()
	before := len(e.out.String())
	cfg := e.cfg
	cfg.Source = ""
	if err := Pending(context.Background(), cfg, term.NewPlain(e.out)); err != nil {
		e.t.Fatal(err)
	}
	return e.out.String()[before:]
}

func TestCheckChangesNothingUntilApplied(t *testing.T) {
	e := newSyncEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.curate()
	driveBefore := e.fd.tree(t, "UpdateMusic")
	localBefore := readDir(t, e.src, nil)

	if _, err := e.syncCheck(context.Background(), e.src, nil); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	sameTrees(t, "Drive after the check", e.fd.tree(t, "UpdateMusic"), driveBefore)
	sameTrees(t, "local after the check", readDir(t, e.src, nil), localBefore)

	pending := e.pendingText()
	for _, want := range []string{"remove from Drive (to Drive's trash) " + tDeleted1, "upload " + tNew, "move on Drive " + tRenFrom} {
		if !strings.Contains(pending, want) {
			t.Errorf("pending lacks %q:\n%s", want, pending)
		}
	}

	// Saying no changes nothing and keeps the plan.
	asked := 0
	if err := e.apply(context.Background(), func(c *Config) {
		c.Confirm = func(string) bool { asked++; return false }
	}); err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("asked %d times, want 1", asked)
	}
	sameTrees(t, "Drive after saying no", e.fd.tree(t, "UpdateMusic"), driveBefore)
	if e.planFor("sync", e.src) == nil {
		t.Fatal("saying no dropped the plan")
	}

	// No terminal and no --yes: refuse, change nothing.
	if err := e.apply(context.Background(), nil); err == nil {
		t.Error("apply without a terminal or --yes went ahead")
	}
	sameTrees(t, "Drive after refusing", e.fd.tree(t, "UpdateMusic"), driveBefore)

	if err := e.apply(context.Background(), func(c *Config) { c.Confirm = func(string) bool { return true } }); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	sameTrees(t, "Drive after apply", withoutExcluded(e.fd.tree(t, "UpdateMusic"), e.cfg.Exclude), readDir(t, e.src, e.cfg.Exclude))
	if e.planFor("sync", e.src) != nil {
		t.Error("the applied plan is still pending")
	}
	if !strings.Contains(e.pendingText(), "Nothing pending") {
		t.Error("pending should be empty after apply")
	}
}

func TestApplyDoesOnlyWhatWasReviewed(t *testing.T) {
	e := newSyncEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := e.localTree()
	e.curate()
	if _, err := e.syncCheck(context.Background(), e.src, nil); err != nil {
		t.Fatal(err)
	}

	// After the check: a deleted song comes back, a planned new song is
	// edited, and a new one appears.
	e.write(tDeleted1, string(original[tDeleted1]))
	e.write(tNew, "edited after the check")
	e.write("late arrival.mp3", "added after the check")

	if err := e.apply(context.Background(), func(c *Config) { c.Yes = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	drive := e.fd.tree(t, "UpdateMusic")
	if _, ok := drive[tDeleted1]; !ok {
		t.Error("a file restored after the check was removed from Drive")
	}
	if _, ok := drive[tNew]; ok {
		t.Error("a file edited after the check was uploaded anyway")
	}
	if _, ok := drive["late arrival.mp3"]; ok {
		t.Error("a file that appeared after the check was uploaded")
	}
	if _, ok := drive[tDeleted2]; ok {
		t.Error("an unchanged planned removal didn't happen")
	}
	if _, ok := drive[tRenTo]; !ok {
		t.Error("an unchanged planned move didn't happen")
	}
	out := e.out.String()
	if !strings.Contains(out, "SKIPPED: 2 planned changes") || !strings.Contains(out, "appeared since the check") {
		t.Errorf("apply should report the skipped and the new changes:\n%s", out)
	}
}

func TestApplyPullSkipsChangedLocalFile(t *testing.T) {
	e := newSyncEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	if _, err := e.download(context.Background(), other, nil); err != nil {
		t.Fatal(err)
	}
	e.curate()
	if _, err := e.sync(context.Background(), e.src, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pullCheck(context.Background(), other, nil); err != nil {
		t.Fatal(err)
	}
	writeIn(t, other, tDeleted1, "edited on the other laptop after the check")

	if err := e.apply(context.Background(), func(c *Config) { c.Yes = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if b, _ := os.ReadFile(filepath.Join(other, tDeleted1)); string(b) != "edited on the other laptop after the check" {
		t.Error("pull removed a file that changed after the check")
	}
	if fileExists(filepath.Join(other, tDeleted2)) {
		t.Errorf("an unchanged planned removal didn't happen\n%s", e.out)
	}
}

func TestApplyStopsBeforeMassRemoval(t *testing.T) {
	e := newSyncEnv(t)
	for i := range 30 {
		e.write(filepath.Join("bulk", strings.Repeat("b", i+1)+".mp3"), strings.Repeat("x", i+1))
	}
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	must(t, os.RemoveAll(filepath.Join(e.src, "bulk")))
	if _, err := e.syncCheck(context.Background(), e.src, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "WARNING: this removes 30") {
		t.Errorf("the check should warn:\n%s", e.out)
	}
	err := e.apply(context.Background(), func(c *Config) { c.Yes = true })
	var fe *gdrive.FatalError
	if !errors.As(err, &fe) {
		t.Fatalf("want a stop, got %v", err)
	}
	if e.planFor("sync", e.src) == nil {
		t.Error("a stopped apply dropped the plan")
	}
	if err := e.apply(context.Background(), func(c *Config) { c.Yes, c.AllowManyRemovals = true, true }); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if _, ok := e.fd.tree(t, "UpdateMusic")["bulk/b.mp3"]; ok {
		t.Error("removals didn't happen with --allow-many-removals")
	}
}
