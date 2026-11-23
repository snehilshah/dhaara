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

// pairingEnv is the curating laptop: the music folder uploaded to Drive, and
// a phone (a folder) holding the same files.
type pairingEnv struct {
	*env
	phoneDir string
	phone    phoneDevice
}

func newPairingEnv(t *testing.T) *pairingEnv {
	t.Helper()
	e := newSyncEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	pe := &pairingEnv{env: e, phoneDir: filepath.Join(t.TempDir(), "sdcard UpdateMusic")}
	if _, err := e.download(context.Background(), pe.phoneDir, nil); err != nil {
		t.Fatal(err)
	}
	pe.phone = &dirPhone{pe.phoneDir}
	old := newPhoneDevice
	newPhoneDevice = func(Config) (phoneDevice, error) { return pe.phone, nil }
	t.Cleanup(func() { newPhoneDevice = old })
	pe.add("music", e.src, "drive:UpdateMusic", "phone:/sdcard/UpdateMusic")
	return pe
}

func (pe *pairingEnv) ui() *term.Console { return term.NewPlain(pe.out) }

func (pe *pairingEnv) add(name string, args ...string) {
	pe.t.Helper()
	if err := Add(context.Background(), pe.cfg, pe.ui(), name, args, AddOptions{}); err != nil {
		pe.t.Fatalf("add: %v\n%s", err, pe.out)
	}
}

func (pe *pairingEnv) status() error {
	return Status(context.Background(), pe.cfg, pe.ui(), "", false, false)
}

func (pe *pairingEnv) applyYes(change func(*Config)) error {
	cfg := pe.cfg
	cfg.Yes, cfg.AllowManyRemovals = true, true
	if change != nil {
		change(&cfg)
	}
	return ApplyPairings(context.Background(), cfg, pe.ui(), "")
}

func (pe *pairingEnv) phoneTree() map[string][]byte {
	return readDir(pe.t, pe.phoneDir, pe.cfg.Exclude)
}
func (pe *pairingEnv) localTree() map[string][]byte { return readDir(pe.t, pe.src, pe.cfg.Exclude) }

// The phone is planned against Drive as the sync will leave it, so one
// status and one apply bring all three places in step, with no step of
// the phone's plan found stale or new.
func TestPairingStatusThenApply(t *testing.T) {
	pe := newPairingEnv(t)
	pe.curate()
	if err := pe.status(); err != nil {
		t.Fatalf("status: %v\n%s", err, pe.out)
	}
	pp := loadPairingPlan(pe.cfg, "music")
	if pp == nil || len(pp.Stages) != 2 || pp.Stages[0].Kind != "sync" || pp.Stages[1].Kind != "phone" {
		t.Fatalf("want a two-stage plan (sync, phone), got %+v", pp)
	}
	if len(pp.Stages[1].Steps) == 0 {
		t.Fatal("the phone stage found nothing to do; it should follow the pending sync")
	}
	if !strings.Contains(pe.out.String(), "remove") {
		t.Errorf("status doesn't list the removals:\n%s", pe.out)
	}
	sameTrees(t, "Drive untouched by status", pe.fd.tree(t, "UpdateMusic"), readDir(t, pe.phoneDir, pe.cfg.Exclude))

	pe.out.Reset()
	if err := pe.applyYes(nil); err != nil {
		t.Fatalf("apply: %v\n%s", err, pe.out)
	}
	for _, bad := range []string{"no longer match", "appeared since the check", "skipped"} {
		if strings.Contains(pe.out.String(), bad) {
			t.Errorf("apply output has %q: the prediction was off\n%s", bad, pe.out)
		}
	}
	local := pe.localTree()
	sameTrees(t, "Drive", pe.fd.tree(t, "UpdateMusic"), local)
	sameTrees(t, "phone", pe.phoneTree(), local)
	if loadPairingPlan(pe.cfg, "music") != nil {
		t.Error("the plan is still pending after apply")
	}

	pe.out.Reset()
	if err := pe.status(); err != nil || !strings.Contains(pe.out.String(), "Everything is up to date") {
		t.Errorf("status after apply: %v\n%s", err, pe.out)
	}
}

// Asking first, apply checks again by itself; declining changes nothing and
// leaves the fresh plan pending.
func TestPairingApplyAsks(t *testing.T) {
	pe := newPairingEnv(t)
	pe.curate()
	before := pe.fd.tree(t, "UpdateMusic")
	cfg := pe.cfg
	cfg.AllowManyRemovals = true
	cfg.Confirm = func(string) bool { return false }
	if err := ApplyPairings(context.Background(), cfg, pe.ui(), "music"); err != nil {
		t.Fatalf("declined apply: %v\n%s", err, pe.out)
	}
	sameTrees(t, "Drive after no", pe.fd.tree(t, "UpdateMusic"), before)
	if loadPairingPlan(pe.cfg, "music") == nil {
		t.Error("after no, the plan should be pending")
	}
	cfg.Confirm = func(string) bool { return true }
	if err := ApplyPairings(context.Background(), cfg, pe.ui(), "music"); err != nil {
		t.Fatalf("apply: %v\n%s", err, pe.out)
	}
	sameTrees(t, "phone", pe.phoneTree(), pe.localTree())
}

type offPhone struct{ *dirPhone }

func (offPhone) Check(context.Context) error { return gdrive.Fatalf("no phone ready over adb") }

// A phone that isn't connected is skipped; the rest still happens.
func TestPairingPhoneAway(t *testing.T) {
	pe := newPairingEnv(t)
	before := pe.phoneTree()
	pe.phone = offPhone{&dirPhone{pe.phoneDir}}
	pe.curate()
	if err := pe.status(); err != nil {
		t.Fatalf("status: %v\n%s", err, pe.out)
	}
	if !strings.Contains(pe.out.String(), "not checked") {
		t.Errorf("status doesn't say the phone was skipped:\n%s", pe.out)
	}
	if err := pe.applyYes(nil); err != nil {
		t.Fatalf("apply: %v\n%s", err, pe.out)
	}
	sameTrees(t, "Drive", pe.fd.tree(t, "UpdateMusic"), pe.localTree())
	sameTrees(t, "phone untouched", pe.phoneTree(), before)
}

// Too many removals anywhere stop the apply before its first stage.
func TestPairingRemovalGuardFirst(t *testing.T) {
	pe := newPairingEnv(t)
	for i := range 30 {
		pe.write(filepath.Join("bulk", strings.Repeat("b", i+1)+".mp3"), "bulk")
	}
	if err := pe.status(); err != nil {
		t.Fatalf("%v\n%s", err, pe.out)
	}
	if err := pe.applyYes(nil); err != nil {
		t.Fatalf("%v\n%s", err, pe.out)
	}
	must(t, os.RemoveAll(filepath.Join(pe.src, "bulk")))
	if err := pe.status(); err != nil {
		t.Fatalf("%v\n%s", err, pe.out)
	}
	before := pe.fd.snapshotCounts()
	err := pe.applyYes(func(c *Config) { c.AllowManyRemovals = false })
	if err == nil || !strings.Contains(err.Error(), "--allow-many-removals") {
		t.Fatalf("want the removal guard, got %v", err)
	}
	if d := delta(before, pe.fd.snapshotCounts()); d["trash"] != 0 {
		t.Errorf("Drive changed although the guard stopped the apply: %v", d)
	}
}

// Another laptop follows Drive into a new folder.
func TestPairingFollow(t *testing.T) {
	pe := newPairingEnv(t)
	other := pe.cfg
	other.ConfigDir, other.StateDir = filepath.Join(t.TempDir(), "config"), filepath.Join(t.TempDir(), "state")
	must(t, os.MkdirAll(other.ConfigDir, 0o700))
	tok, err := os.ReadFile(filepath.Join(pe.cfg.ConfigDir, "token.json"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(other.ConfigDir, "token.json"), tok, 0o600))
	dir := filepath.Join(t.TempDir(), "UpdateMusic")
	ctx := context.Background()
	if err := Add(ctx, other, pe.ui(), "music", []string{dir, "drive:UpdateMusic"}, AddOptions{Follow: true}); err != nil {
		t.Fatalf("add --follow: %v\n%s", err, pe.out)
	}
	if err := Status(ctx, other, pe.ui(), "", false, false); err != nil {
		t.Fatalf("status: %v\n%s", err, pe.out)
	}
	other.Yes = true
	if err := ApplyPairings(ctx, other, pe.ui(), ""); err != nil {
		t.Fatalf("apply: %v\n%s", err, pe.out)
	}
	sameTrees(t, "follower", readDir(t, dir, pe.cfg.Exclude), pe.fd.tree(t, "UpdateMusic"))
}

// A pairing stays tied to the Drive folder it was set up with.
func TestPairingPinnedFolder(t *testing.T) {
	pe := newPairingEnv(t)
	ps, err := loadPairings(pe.cfg)
	must(t, err)
	if ps.Pairings["music"].DriveID == "" {
		t.Fatal("add didn't pin the Drive folder")
	}
	ps.Pairings["music"].DriveID = "someone-else"
	must(t, ps.save(pe.cfg))
	err = pe.status()
	var fe *gdrive.FatalError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "isn't the folder") {
		t.Fatalf("want a stop for a different folder, got %v", err)
	}
}

// Setting up a pairing for folders that already sync carries on from what
// dhaara remembers: no first sync, nothing read again.
func TestPairingKeepsEarlierState(t *testing.T) {
	pe := newPairingEnv(t)
	if _, err := pe.sync(context.Background(), pe.src, nil); err != nil {
		t.Fatal(err)
	}
	pe.out.Reset()
	if err := Status(context.Background(), pe.cfg, pe.ui(), "", true, false); err != nil {
		t.Fatalf("%v\n%s", err, pe.out)
	}
	if strings.Contains(pe.out.String(), "First sync") {
		t.Errorf("the pairing started a first sync instead of using the earlier state:\n%s", pe.out)
	}
}

func TestAddChecks(t *testing.T) {
	pe := newPairingEnv(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"music", []string{pe.src, "drive:Other"}, "already pairs"},
		{"other", []string{pe.src, "drive:Other"}, "already in the pairing"},
		{"bad name", []string{pe.src, "drive:Other"}, "can't be a pairing name"},
		{"new", []string{"drive:New"}, "needs a folder here"},
		{"music", []string{"phone:sdcard/x"}, "full path"},
		{"music", []string{"phone:/sdcard/UpdateMusic"}, "already in"},
	} {
		err := Add(ctx, pe.cfg, pe.ui(), tc.name, tc.args, AddOptions{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("add %s %v: want %q, got %v", tc.name, tc.args, tc.want, err)
		}
	}
	if err := Forget(pe.cfg, pe.ui(), "music", []string{"phone:/sdcard/UpdateMusic"}); err != nil {
		t.Fatal(err)
	}
	ps, _ := loadPairings(pe.cfg)
	if len(ps.Pairings["music"].Phones) != 0 {
		t.Error("forget didn't take the phone out")
	}
	if err := Forget(pe.cfg, pe.ui(), "music", nil); err != nil {
		t.Fatal(err)
	}
	ps, _ = loadPairings(pe.cfg)
	if len(ps.Pairings) != 0 {
		t.Error("forget didn't remove the pairing")
	}
}
