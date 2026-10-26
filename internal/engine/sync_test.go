package engine

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snehilshah/dhaara/internal/adb"
	"github.com/snehilshah/dhaara/internal/exclude"
	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/term"
)

// Files the tests change, given distinct contents so moves are unambiguous.
const (
	tDeleted1 = "english/Mikey/HIStory/CD1/t0.mp3"
	tDeleted2 = "english/Mikey/HIStory/CD1/t1.mp3"
	tGoneDir  = "english/Mikey/HIStory/CD2" // deleted with everything in it
	tRenFrom  = "lofi/memory reboot (slowed).mp3"
	tRenTo    = "lofi/renamed ⧸ 'x'/memory reboot.mp3"
	tChanged  = "hindi/deemix/A.R. Rahman - Guru/g0.mp3"
	tNew      = "hindi/new song.mp3"
	tReport   = "duplicates_report.txt" // excluded by *.txt
)

func newSyncEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	for i, rel := range []string{tDeleted1, tDeleted2, tRenFrom, tChanged} {
		e.write(rel, strings.Repeat("unique "+rel, 3000+i))
	}
	e.cfg.Exclude = []string{"*.txt"}
	e.cfg.Trash = testTrash(t)
	return e
}

// testTrash moves "trashed" files into a temporary folder instead of the
// real desktop trash.
func testTrash(t *testing.T) func(string) error {
	dir := t.TempDir()
	var n atomic.Int64
	return func(p string) error {
		return os.Rename(p, filepath.Join(dir, strings.Repeat("x", int(n.Add(1)))))
	}
}

func (e *env) write(rel, content string) {
	e.t.Helper()
	writeIn(e.t, e.src, rel, content)
}

func writeIn(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(content), 0o644))
}

// curate makes the kind of changes the user makes in the library.
func (e *env) curate() {
	e.t.Helper()
	must(e.t, os.Remove(filepath.Join(e.src, tDeleted1)))
	must(e.t, os.Remove(filepath.Join(e.src, tDeleted2)))
	must(e.t, os.RemoveAll(filepath.Join(e.src, tGoneDir)))
	must(e.t, os.MkdirAll(filepath.Dir(filepath.Join(e.src, tRenTo)), 0o755))
	must(e.t, os.Rename(filepath.Join(e.src, tRenFrom), filepath.Join(e.src, tRenTo)))
	e.write(tChanged, "a better version of g0")
	e.write(tNew, "a new song")
	e.write(tReport, "a report that stays here")
}

func (e *env) syncCfg(src string, change func(*Config)) Config {
	cfg := e.cfg
	cfg.Mode, cfg.Source = "sync", src
	if cfg.RemoteName == "" {
		cfg.RemoteName = "UpdateMusic"
	}
	if change != nil {
		change(&cfg)
	}
	return cfg
}

// syncCheck is "dhaara sync": it saves a plan and changes nothing.
func (e *env) syncCheck(ctx context.Context, src string, change func(*Config)) (SyncSummary, error) {
	e.t.Helper()
	return Sync(ctx, e.syncCfg(src, change), term.NewPlain(e.out))
}

// planFor returns the pending plan of this kind for the local folder, or nil.
func (e *env) planFor(kind, src string) *savedPlan {
	e.t.Helper()
	abs, _ := filepath.Abs(src)
	plans, err := loadPlans(e.cfg, abs)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range plans {
		if p.Kind == kind {
			return p
		}
	}
	return nil
}

// sync is "dhaara sync" then "dhaara apply" for that plan.
func (e *env) sync(ctx context.Context, src string, change func(*Config)) (SyncSummary, error) {
	e.t.Helper()
	sum, err := e.syncCheck(ctx, src, change)
	p := e.planFor("sync", src)
	if p == nil || (err != nil && !errors.Is(err, ErrProblems)) {
		return sum, err
	}
	checkErr := err
	cfg := e.syncCfg(src, change)
	cfg.applying = p
	sum, err = Sync(ctx, cfg, term.NewPlain(e.out))
	os.Remove(p.path)
	if err == nil {
		err = checkErr // conflicts found by the check
	}
	return sum, err
}

func (e *env) pullCheck(ctx context.Context, local string, change func(*Config)) (DownloadSummary, error) {
	e.t.Helper()
	return e.download(ctx, local, func(c *Config) {
		c.Mode, c.Pull = "pull", true
		if change != nil {
			change(c)
		}
	})
}

func (e *env) pull(ctx context.Context, local string, change func(*Config)) (DownloadSummary, error) {
	e.t.Helper()
	sum, err := e.pullCheck(ctx, local, change)
	p := e.planFor("pull", local)
	if p == nil || err != nil {
		return sum, err
	}
	sum, err = e.download(ctx, local, func(c *Config) {
		c.Mode, c.Pull, c.applying = "pull", true, p
		if change != nil {
			change(c)
		}
	})
	os.Remove(p.path)
	return sum, err
}

// readDir returns every file under root, without excluded ones.
func readDir(t *testing.T, root string, patterns []string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if !exclude.Match(rel, patterns) {
			out[filepath.ToSlash(rel)], _ = os.ReadFile(p)
		}
		return nil
	})
	return out
}

func withoutExcluded(m map[string][]byte, patterns []string) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range m {
		if !exclude.Match(k, patterns) {
			out[k] = v
		}
	}
	return out
}

func sameTrees(t *testing.T, what string, got, want map[string][]byte) {
	t.Helper()
	for p, data := range want {
		if g, ok := got[p]; !ok {
			t.Errorf("%s: missing %s", what, p)
		} else if !bytes.Equal(g, data) {
			t.Errorf("%s: content differs: %s", what, p)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s: extra file %s", what, p)
		}
	}
}

// treePath returns the files under a folder given by names from My Drive.
func (fd *fakeDrive) treePath(t *testing.T, names ...string) map[string][]byte {
	t.Helper()
	fd.mu.Lock()
	parent := "root"
	for _, n := range names {
		found := ""
		for _, id := range fd.order {
			if f := fd.files[id]; f.Parent == parent && f.Name == n && f.Mime == gdrive.FolderMime && !f.Trashed {
				found = id
			}
		}
		if found == "" {
			fd.mu.Unlock()
			t.Fatalf("no folder %q on Drive", strings.Join(names, "/"))
		}
		parent = found
	}
	fd.mu.Unlock()
	out := map[string][]byte{}
	var walk func(id, prefix string)
	walk = func(id, prefix string) {
		for _, fid := range fd.order {
			f := fd.files[fid]
			if f.Parent != id || f.Trashed {
				continue
			}
			p := strings.TrimPrefix(prefix+"/"+f.Name, "/")
			if f.Mime == gdrive.FolderMime {
				walk(f.ID, p)
				continue
			}
			if _, dup := out[p]; dup {
				t.Errorf("duplicate on Drive: %s", p)
			}
			out[p] = f.Data
		}
	}
	fd.mu.Lock()
	walk(parent, "")
	fd.mu.Unlock()
	return out
}

func (fd *fakeDrive) hasFolder(name string) bool {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	for _, f := range fd.files {
		if f.Name == name && f.Mime == gdrive.FolderMime && !f.Trashed {
			return true
		}
	}
	return false
}

func (fd *fakeDrive) folderNames(parentName string) []string {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	var parent string
	for _, id := range fd.order {
		if f := fd.files[id]; f.Parent == "root" && f.Name == parentName && !f.Trashed {
			parent = id
		}
	}
	var out []string
	for _, id := range fd.order {
		if f := fd.files[id]; f.Parent == parent && f.Mime == gdrive.FolderMime && !f.Trashed {
			out = append(out, f.Name)
		}
	}
	return out
}

func (fd *fakeDrive) removedList(t *testing.T) []removedEntry {
	t.Helper()
	fd.mu.Lock()
	defer fd.mu.Unlock()
	for _, id := range fd.order {
		if f := fd.files[id]; strings.HasSuffix(f.Name, " removed.json") && !f.Trashed {
			var doc struct {
				Removed []removedEntry `json:"removed"`
			}
			if err := json.Unmarshal(f.Data, &doc); err != nil {
				t.Fatal(err)
			}
			return doc.Removed
		}
	}
	return nil
}

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func TestSyncFollowsLocalChanges(t *testing.T) {
	e := newSyncEnv(t)
	e.cfg.Exclude = nil // the first, plain upload sends notes.txt too
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.cfg.Exclude = []string{"*.txt"}
	original := e.localTree()
	e.curate()

	before := e.fd.snapshotCounts()
	if _, err := e.syncCheck(context.Background(), e.src, nil); err != nil {
		t.Fatalf("dry run: %v\n%s", err, e.out)
	}
	if d := delta(before, e.fd.snapshotCounts()); d["trash"]+d["move"]+d["rename"]+d["multipart"]+d["initiate"] != 0 {
		t.Fatalf("dry run changed Drive: %v", d)
	}
	if !strings.Contains(e.out.String(), "move to Drive's trash "+tDeleted1) {
		t.Errorf("dry run should list the deletion:\n%s", e.out)
	}

	e.fd.mu.Lock()
	e.fd.failRate = 0.1 // and survive a flaky connection
	e.fd.mu.Unlock()
	before = e.fd.snapshotCounts()
	sum, err := e.sync(context.Background(), e.src, nil)
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, e.out)
	}
	d := delta(before, e.fd.snapshotCounts())
	if d["multipart"]+d["initiate"] != 2 { // the changed file and the new one; the rename is a move
		t.Errorf("want 2 uploads, got %v", d)
	}
	if sum.MovedOnDrive != 1 || sum.TrashedOnDrive != 7 || sum.Uploaded != 2 {
		t.Errorf("summary %+v", sum)
	}
	drive := e.fd.tree(t, "UpdateMusic")
	sameTrees(t, "Drive", withoutExcluded(drive, e.cfg.Exclude), readDir(t, e.src, e.cfg.Exclude))
	if _, ok := drive["notes.txt"]; !ok {
		t.Error("an excluded file on Drive was removed")
	}
	if _, ok := drive[tReport]; ok {
		t.Error("an excluded local file was uploaded")
	}
	if !e.fd.hasFolder("CD2") {
		t.Error("an emptied folder was trashed on Drive; dhaara can't see everything in it")
	}

	rl := &removedList{entries: e.fd.removedList(t)}
	for _, rel := range []string{tDeleted1, tDeleted2, tRenFrom, tChanged, tGoneDir + "/t0.mp3"} {
		if !rl.Has(rel, md5hex(original[rel])) {
			t.Errorf("removed list lacks %s", rel)
		}
	}

	e.fd.mu.Lock()
	e.fd.failRate = 0
	e.fd.mu.Unlock()
	before = e.fd.snapshotCounts()
	if _, err := e.sync(context.Background(), e.src, nil); err != nil {
		t.Fatalf("second sync: %v\n%s", err, e.out)
	}
	if d := delta(before, e.fd.snapshotCounts()); d["trash"]+d["move"]+d["multipart"]+d["initiate"]+d["media"]+d["update"] != 0 {
		t.Errorf("second sync should do nothing: %v", d)
	}
}

func TestPullFollowsSync(t *testing.T) {
	e := newSyncEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other laptop")
	if _, err := e.download(context.Background(), other, nil); err != nil {
		t.Fatal(err)
	}
	writeIn(t, other, "mine only.mp3", "never on Drive")
	writeIn(t, other, "notes.txt", "excluded, so never touched")
	writeIn(t, other, tDeleted2, "a different version, made on this laptop")

	e.curate()
	if _, err := e.sync(context.Background(), e.src, nil); err != nil {
		t.Fatalf("sync: %v\n%s", err, e.out)
	}

	beforeTree := readDir(t, other, nil)
	if _, err := e.pullCheck(context.Background(), other, nil); err != nil {
		t.Fatalf("dry run: %v\n%s", err, e.out)
	}
	sameTrees(t, "after dry run", readDir(t, other, nil), beforeTree)

	before := e.fd.snapshotCounts()
	if _, err := e.pull(context.Background(), other, nil); err != nil {
		t.Fatalf("pull: %v\n%s", err, e.out)
	}
	if d := delta(before, e.fd.snapshotCounts()); d["media"] != 2 { // the changed file and the new one
		t.Errorf("want 2 downloads (the rename is a local move), got %v", d)
	}
	got := readDir(t, other, e.cfg.Exclude)
	if string(got["mine only.mp3"]) != "never on Drive" || string(got[tDeleted2]) != "a different version, made on this laptop" {
		t.Error("pull removed or changed a file that wasn't removed from Drive")
	}
	delete(got, "mine only.mp3")
	delete(got, tDeleted2)
	sameTrees(t, "other laptop", got, readDir(t, e.src, e.cfg.Exclude))
	if _, err := os.Stat(filepath.Join(other, tGoneDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the emptied folder stayed")
	}
	if _, err := os.Stat(filepath.Join(other, "notes.txt")); err != nil {
		t.Error("an excluded file was removed")
	}

	before = e.fd.snapshotCounts()
	if _, err := e.pull(context.Background(), other, nil); err != nil {
		t.Fatalf("second pull: %v\n%s", err, e.out)
	}
	if d := delta(before, e.fd.snapshotCounts()); d["media"] != 0 {
		t.Errorf("second pull downloaded %v", d)
	}
}

func TestSyncFollowsDriveChanges(t *testing.T) {
	e := newSyncEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "UpdateMusic")
	if _, err := e.download(context.Background(), other, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sync(context.Background(), other, nil); err != nil { // its first sync: nothing differs
		t.Fatalf("first sync there: %v\n%s", err, e.out)
	}
	e.curate()
	if _, err := e.sync(context.Background(), e.src, nil); err != nil {
		t.Fatalf("sync: %v\n%s", err, e.out)
	}
	before := e.fd.snapshotCounts()
	sum, err := e.sync(context.Background(), other, nil)
	if err != nil {
		t.Fatalf("sync there: %v\n%s", err, e.out)
	}
	if sum.MovedHere != 1 || sum.TrashedHere != 7 || sum.Downloaded != 2 || sum.Uploaded != 0 {
		t.Errorf("summary %+v", sum)
	}
	if d := delta(before, e.fd.snapshotCounts()); d["trash"]+d["multipart"]+d["initiate"] != 0 {
		t.Errorf("following Drive changed Drive: %v", d)
	}
	sameTrees(t, "second laptop", readDir(t, other, e.cfg.Exclude), readDir(t, e.src, e.cfg.Exclude))
}

func TestSyncConflictTouchesNeither(t *testing.T) {
	e := newSyncEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "UpdateMusic")
	if _, err := e.download(context.Background(), other, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sync(context.Background(), other, nil); err != nil {
		t.Fatal(err)
	}
	e.write(tChanged, "edited on the first laptop")
	if _, err := e.sync(context.Background(), e.src, nil); err != nil {
		t.Fatal(err)
	}
	writeIn(t, other, tChanged, "edited on the second laptop")
	sum, err := e.sync(context.Background(), other, nil)
	if !errors.Is(err, ErrProblems) || sum.Conflicts != 1 {
		t.Fatalf("want one conflict, got %+v, %v\n%s", sum, err, e.out)
	}
	if b, _ := os.ReadFile(filepath.Join(other, tChanged)); string(b) != "edited on the second laptop" {
		t.Error("conflict changed the local file")
	}
	if string(e.fd.tree(t, "UpdateMusic")[tChanged]) != "edited on the first laptop" {
		t.Error("conflict changed Drive")
	}
}

func TestSyncFirstRunWithoutRecordRemovesNothing(t *testing.T) {
	e := newSyncEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "UpdateMusic")
	if _, err := e.download(context.Background(), other, nil); err != nil {
		t.Fatal(err)
	}
	must(t, os.Remove(filepath.Join(other, tDeleted1)))
	writeIn(t, other, "extra.mp3", "only here")
	sum, err := e.sync(context.Background(), other, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if sum.TrashedOnDrive != 0 || sum.Downloaded != 1 || sum.Uploaded != 1 {
		t.Errorf("first sync without a record should only add: %+v", sum)
	}
}

func TestSyncStopsBeforeMassRemoval(t *testing.T) {
	e := newSyncEnv(t)
	for i := range 30 {
		e.write(fmt.Sprintf("bulk/%02d.mp3", i), fmt.Sprintf("bulk %d", i))
	}
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	must(t, os.RemoveAll(filepath.Join(e.src, "bulk")))
	before := e.fd.snapshotCounts()
	_, err := e.sync(context.Background(), e.src, nil)
	var fe *gdrive.FatalError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "--allow-many-removals") {
		t.Fatalf("want a stop, got %v", err)
	}
	if d := delta(before, e.fd.snapshotCounts()); d["trash"] != 0 {
		t.Errorf("removed files anyway: %v", d)
	}
	if _, err := e.sync(context.Background(), e.src, func(c *Config) { c.AllowManyRemovals = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	sameTrees(t, "Drive", withoutExcluded(e.fd.tree(t, "UpdateMusic"), e.cfg.Exclude), readDir(t, e.src, e.cfg.Exclude))
}

func (e *env) snapshot(ctx context.Context) error {
	cfg := e.cfg
	cfg.Mode, cfg.RemoteName = "snapshot", "UpdateMusic"
	return Snapshot(ctx, cfg, term.NewPlain(e.out))
}

func TestSnapshotCopiesOnDrive(t *testing.T) {
	e := uploaded(t)
	e.fd.failRate = 0.1
	before := e.fd.snapshotCounts()
	if err := e.snapshot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	d := delta(before, e.fd.snapshotCounts())
	if d["media"]+d["multipart"]+d["initiate"] != 0 {
		t.Errorf("a snapshot must not move file data: %v", d)
	}
	date := time.Now().Format("2006-01-02")
	original := e.fd.tree(t, "UpdateMusic")
	sameTrees(t, "snapshot", e.fd.treePath(t, "UpdateMusic snapshots", date), original)

	// A stopped snapshot continues where it stopped, under the same name.
	ctx, cancel := context.WithCancel(context.Background())
	e.fd.mu.Lock()
	e.fd.failRate = 0
	e.fd.onCopy = func(n int) {
		if n == d["copy"]+5 {
			cancel()
		}
	}
	e.fd.mu.Unlock()
	if err := e.snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want a stop, got %v", err)
	}
	e.fd.mu.Lock()
	e.fd.onCopy = nil
	e.fd.mu.Unlock()
	if names := e.fd.folderNames("UpdateMusic snapshots"); !strings.Contains(strings.Join(names, "|"), date+" (2)"+unfinishedSuffix) {
		t.Fatalf("no unfinished snapshot: %v", names)
	}
	before = e.fd.snapshotCounts()
	if err := e.snapshot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if d := delta(before, e.fd.snapshotCounts()); d["copy"] >= len(original) {
		t.Errorf("the resumed snapshot copied everything again: %v", d)
	}
	sameTrees(t, "resumed snapshot", e.fd.treePath(t, "UpdateMusic snapshots", date+" (2)"), original)
}

// dirPhone is a phone made of a local folder.
type dirPhone struct{ root string }

func (p *dirPhone) Check(context.Context) error { return nil }

func (p *dirPhone) List(context.Context) (map[string]adb.File, error) {
	out := map[string]adb.File{}
	err := filepath.WalkDir(p.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, _ := d.Info()
		rel, _ := filepath.Rel(p.root, path)
		out[rel] = adb.File{Size: fi.Size(), MtimeNs: fi.ModTime().Unix() * 1e9}
		return nil
	})
	return out, err
}

func (p *dirPhone) sum(rel string) string {
	b, err := os.ReadFile(filepath.Join(p.root, rel))
	if err != nil {
		return ""
	}
	return md5hex(b)
}

func (p *dirPhone) MD5(_ context.Context, rels []string) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range rels {
		out[r] = p.sum(r)
	}
	return out, nil
}

func (p *dirPhone) Remove(_ context.Context, rels []string, md5s map[string]string) (map[string]bool, error) {
	ok := map[string]bool{}
	for _, r := range rels {
		if p.sum(r) == md5s[r] {
			ok[r] = os.Remove(filepath.Join(p.root, r)) == nil
		}
	}
	return ok, nil
}

func (p *dirPhone) Move(_ context.Context, moves []adb.Move) (map[string]bool, error) {
	ok := map[string]bool{}
	for _, m := range moves {
		ok[m.From] = moveLocal(filepath.Join(p.root, m.From), filepath.Join(p.root, m.To)) == nil
	}
	return ok, nil
}

func (p *dirPhone) Push(_ context.Context, pp adb.Push) error {
	b, err := os.ReadFile(pp.Local)
	if err != nil {
		return err
	}
	dst := filepath.Join(p.root, pp.Rel)
	os.MkdirAll(filepath.Dir(dst), 0o755)
	return os.WriteFile(dst, b, 0o644)
}

func (p *dirPhone) RemoveEmptyDirs(_ context.Context, rels []string) error {
	removeEmptyDirs(p.root, rels)
	return nil
}

func (e *env) phoneCheck(ctx context.Context, dev phoneDevice, change func(*Config)) error {
	cfg := e.cfg
	cfg.Mode, cfg.Source, cfg.RemoteName, cfg.PhoneDir = "phone", e.src, "UpdateMusic", "/sdcard/UpdateMusic"
	if change != nil {
		change(&cfg)
	}
	old := newPhoneDevice
	newPhoneDevice = func(Config) (phoneDevice, error) { return dev, nil }
	defer func() { newPhoneDevice = old }()
	return Phone(ctx, cfg, term.NewPlain(e.out))
}

func (e *env) phone(ctx context.Context, dev phoneDevice, change func(*Config)) error {
	err := e.phoneCheck(ctx, dev, change)
	p := e.planFor("phone", e.src)
	if p == nil || (err != nil && !errors.Is(err, ErrProblems)) {
		return err
	}
	checkErr := err
	err = e.phoneCheck(ctx, dev, func(c *Config) {
		if change != nil {
			change(c)
		}
		c.applying = p
	})
	os.Remove(p.path)
	if err == nil {
		err = checkErr // conflicts found by the check
	}
	return err
}

func testPhoneFollowsDrive(t *testing.T, dev func(root string) phoneDevice) {
	e := newSyncEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	phoneDir := filepath.Join(t.TempDir(), "sdcard UpdateMusic")
	if _, err := e.download(context.Background(), phoneDir, nil); err != nil {
		t.Fatal(err)
	}
	writeIn(t, phoneDir, "phone only.mp3", "recorded on the phone")
	writeIn(t, phoneDir, "top.mp3", "lyrics saved into the file by Geet")

	e.curate()
	if _, err := e.sync(context.Background(), e.src, nil); err != nil {
		t.Fatal(err)
	}
	phone := dev(phoneDir)
	before := readDir(t, phoneDir, nil)
	if err := e.phoneCheck(context.Background(), phone, nil); err != nil && !errors.Is(err, ErrProblems) { // top.mp3 is a conflict
		t.Fatalf("dry run: %v\n%s", err, e.out)
	}
	sameTrees(t, "phone after dry run", readDir(t, phoneDir, nil), before)

	mediaBefore := e.fd.count("media")
	err := e.phone(context.Background(), phone, nil)
	if !errors.Is(err, ErrProblems) { // the edited top.mp3 is a conflict
		t.Fatalf("want a reported conflict, got %v\n%s", err, e.out)
	}
	if e.fd.count("media") != mediaBefore {
		t.Error("phone downloaded from Drive; it should copy from the laptop")
	}
	got := readDir(t, phoneDir, e.cfg.Exclude)
	if string(got["phone only.mp3"]) != "recorded on the phone" || string(got["top.mp3"]) != "lyrics saved into the file by Geet" {
		t.Error("phone lost a file that wasn't removed from Drive")
	}
	delete(got, "phone only.mp3")
	want := readDir(t, e.src, e.cfg.Exclude)
	delete(got, "top.mp3")
	delete(want, "top.mp3")
	sameTrees(t, "phone", got, want)
	for rel := range got {
		if strings.HasSuffix(rel, partSuffix) {
			t.Errorf("leftover part file %s", rel)
		}
	}
}

func TestPhoneFollowsDrive(t *testing.T) {
	testPhoneFollowsDrive(t, func(root string) phoneDevice { return &dirPhone{root} })
}

// TestPhoneShellScripts runs the real adb code against a stand-in "adb" that
// runs the phone-side scripts in this machine's shell, inside a folder.
func TestPhoneShellScripts(t *testing.T) {
	for _, tool := range []string{"sh", "md5sum", "stat", "find"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("needs " + tool)
		}
	}
	bin := t.TempDir()
	fake := filepath.Join(bin, "adb")
	must(t, os.WriteFile(fake, []byte(`#!/bin/sh
case "$1" in
get-state) echo device ;;
shell) shift; [ "$1" = "-T" ] && shift; exec sh -c "$*" ;;
push) mkdir -p "$(dirname "$3")" && cp -p "$2" "$3" ;;
*) echo "unknown $1" >&2; exit 1 ;;
esac
`), 0o755))
	testPhoneFollowsDrive(t, func(root string) phoneDevice { return newADBDevice(fake, root) })

	// Names that would break a careless shell script.
	root := t.TempDir()
	odd := []string{"it's \"quoted\" $(echo no).mp3", "-starts with dash.mp3", "tab\there.mp3", "back\\slash.mp3", "ünï ⧸ code.mp3"}
	for _, n := range odd {
		writeIn(t, root, "a dir/"+n, n)
	}
	dev := newADBDevice(fake, root)
	files, err := dev.List(context.Background())
	if err != nil || len(files) != len(odd) {
		t.Fatalf("list: %v %v", files, err)
	}
	var rels []string
	md5s := map[string]string{}
	for _, n := range odd {
		rel := "a dir/" + n
		rels = append(rels, rel)
		md5s[rel] = md5hex([]byte(n))
	}
	got, err := dev.MD5(context.Background(), rels)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range rels {
		if got[rel] != md5s[rel] {
			t.Errorf("md5 of %q: %q", rel, got[rel])
		}
	}
	md5s[rels[0]] = strings.Repeat("0", 32) // wrong checksum: must not be removed
	ok, err := dev.Remove(context.Background(), rels, md5s)
	if err != nil {
		t.Fatal(err)
	}
	if ok[rels[0]] || !fileExists(filepath.Join(root, rels[0])) {
		t.Error("removed a file whose checksum didn't match")
	}
	for _, rel := range rels[1:] {
		if !ok[rel] || fileExists(filepath.Join(root, rel)) {
			t.Errorf("not removed: %q", rel)
		}
	}
}

func TestSyncKeepsDriveFolderWithExcludedFiles(t *testing.T) {
	e := newSyncEnv(t)
	e.write("keep/a.mp3", "a song")
	e.write("keep/notes.txt", "excluded, but on Drive")
	e.cfg.Exclude = nil
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.cfg.Exclude = []string{"*.txt"}
	must(t, os.RemoveAll(filepath.Join(e.src, "keep")))
	if _, err := e.sync(context.Background(), e.src, nil); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	drive := e.fd.tree(t, "UpdateMusic")
	if _, ok := drive["keep/a.mp3"]; ok {
		t.Error("the deleted file is still on Drive")
	}
	if string(drive["keep/notes.txt"]) != "excluded, but on Drive" {
		t.Error("the folder holding an excluded file was trashed")
	}
}

func TestNestedDriveFolder(t *testing.T) {
	e := newSyncEnv(t)
	e.cfg.RemoteName = "Work/Backups/UpdateMusic"
	if _, err := e.run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	e.curate()
	if _, err := e.sync(context.Background(), e.src, nil); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	sameTrees(t, "Drive", e.fd.treePath(t, "Work", "Backups", "UpdateMusic"), readDir(t, e.src, e.cfg.Exclude))

	other := filepath.Join(t.TempDir(), "UpdateMusic")
	if _, err := e.pull(context.Background(), other, func(c *Config) {
		c.RemoteName = "Work/Backups/UpdateMusic"
	}); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	sameTrees(t, "pulled", readDir(t, other, e.cfg.Exclude), readDir(t, e.src, e.cfg.Exclude))
	if fileExists(filepath.Join(other, tGoneDir)) {
		t.Error("pull made a folder that holds no files")
	}

	cfg := e.cfg
	cfg.Mode = "snapshot"
	if err := Snapshot(context.Background(), cfg, term.NewPlain(e.out)); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	sameTrees(t, "snapshot", e.fd.treePath(t, "Work", "Backups", "UpdateMusic snapshots", time.Now().Format("2006-01-02")),
		e.fd.treePath(t, "Work", "Backups", "UpdateMusic"))
}
