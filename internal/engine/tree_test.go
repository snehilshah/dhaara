package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/term"
)

// listTreeOneByOne is the walk listTree replaced: one folder at a time, in
// order. listTree must give exactly the same result.
func listTreeOneByOne(ctx context.Context, d *gdrive.Drive, rootID string) (*remoteTree, error) {
	t := &remoteTree{rootID: rootID, dirIDs: map[string]string{"": rootID}, byRel: map[string]remoteFile{}}
	type item struct{ id, rel string }
	queue := []item{{rootID, ""}}
	seen := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		kids, err := d.ListChildren(ctx, cur.id)
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			shown := k.Name
			if cur.rel != "" {
				shown = cur.rel + "/" + k.Name
			}
			if !gdrive.SafeName(k.Name) {
				t.skipped = append(t.skipped, fmt.Sprintf("%q (name is not safe to use as a file name)", shown))
				continue
			}
			rel := filepath.Join(cur.rel, k.Name)
			if !filepath.IsLocal(rel) {
				t.skipped = append(t.skipped, fmt.Sprintf("%q (path is not safe)", shown))
				continue
			}
			size := k.SizeBytes()
			mod, _ := time.Parse(time.RFC3339Nano, k.Modified)
			rf := remoteFile{Rel: rel, ID: k.ID, Size: size, MD5: k.MD5, Modified: mod, Parent: cur.id}
			if seen[rel] {
				if !k.IsFolder() {
					t.dups = append(t.dups, rf)
				}
				t.skipped = append(t.skipped, fmt.Sprintf("%q (another item on Drive has the same name; kept the oldest)", shown))
				continue
			}
			if k.IsFolder() {
				seen[rel] = true
				t.dirs = append(t.dirs, rel)
				t.dirIDs[rel] = k.ID
				queue = append(queue, item{k.ID, rel})
				continue
			}
			if strings.HasPrefix(k.MimeType, "application/vnd.google-apps.") || size < 0 || (size > 0 && k.MD5 == "") {
				t.skipped = append(t.skipped, fmt.Sprintf("%q (a Google Docs-type file, not a regular file)", shown))
				continue
			}
			seen[rel] = true
			t.files = append(t.files, rf)
			t.byRel[rel] = rf
		}
	}
	return t, nil
}

// randomDriveTree fills the fake with a messy folder: several levels, files
// and folders sharing names, unsafe names, Docs files and trashed items.
func randomDriveTree(fd *fakeDrive, seed uint64) string {
	rng := rand.New(rand.NewPCG(seed, seed))
	fd.mu.Lock()
	defer fd.mu.Unlock()
	mk := func(name, parent, mime string, data []byte) *fakeFile {
		f := fd.create(map[string]any{"name": name, "parents": []any{parent},
			"modifiedTime": "2026-01-02T03:04:05.678Z"}, data, mime)
		return f
	}
	root := mk("Tree", "root", gdrive.FolderMime, nil)
	folders := []string{root.ID}
	for i := range 6 { // some safe folders, two levels deep, so the rest has somewhere to go
		folders = append(folders, mk(fmt.Sprintf("d%d", i), folders[i/2], gdrive.FolderMime, nil).ID)
	}
	names := []string{"a", "b", "c", "a", "..", "x/y", "Song.mp3", "Song.mp3", "cover.jpg"}
	for range 200 {
		parent := folders[rng.IntN(len(folders))]
		name := names[rng.IntN(len(names))]
		switch rng.IntN(10) {
		case 0, 1, 2:
			folders = append(folders, mk(name, parent, gdrive.FolderMime, nil).ID)
		case 3:
			mk(name, parent, "application/vnd.google-apps.document", nil)
		case 4:
			mk(name, parent, "audio/mpeg", nil).Trashed = true
		default:
			mk(fmt.Sprintf("%s %d", name, rng.IntN(2)), parent, "audio/mpeg", []byte(name))
		}
	}
	return root.ID
}

func TestListTreeMatchesOneByOne(t *testing.T) {
	for seed := range uint64(5) {
		e := newEnv(t)
		rootID := randomDriveTree(e.fd, seed+1)
		e.fd.failRate = 0.1 // retries must not change the result
		ctx := context.Background()
		d, _, err := connect(ctx, e.cfg, term.NewPlain(e.out))
		if err != nil {
			t.Fatal(err)
		}
		want, err := listTreeOneByOne(ctx, d, rootID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := listTree(ctx, d.NewListings(), rootID)
		if err != nil {
			t.Fatal(err)
		}
		if len(want.dirs) < 10 || len(want.dups) == 0 || len(want.skipped) == 0 {
			t.Fatalf("seed %d: tree too tame to test: %d folders, %d dups, %d skipped", seed, len(want.dirs), len(want.dups), len(want.skipped))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: parallel walk differs from one-by-one walk\n got dirs %v\nwant dirs %v", seed, got.dirs, want.dirs)
		}
	}
}

// listings returns how many times each Drive folder was listed.
func listings(counts map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range counts {
		if id, ok := strings.CutPrefix(k, "list:"); ok {
			out[id] = v
		}
	}
	return out
}

func TestEachFolderReadOncePerCheck(t *testing.T) {
	e := uploaded(t)
	e.curate()
	before := e.fd.snapshotCounts()
	if _, err := e.syncCheck(context.Background(), e.src, nil); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	for id, n := range listings(delta(before, e.fd.snapshotCounts())) {
		if n > 1 {
			t.Errorf("a check listed folder %s %d times; want once", id, n)
		}
	}

	// Applying lists each folder once while planning and once more for the
	// final check of both sides, never more.
	before = e.fd.snapshotCounts()
	if err := e.apply(context.Background(), func(c *Config) { c.Yes, c.AllowManyRemovals = true, true }); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	for id, n := range listings(delta(before, e.fd.snapshotCounts())) {
		if n > 2 {
			t.Errorf("an apply listed folder %s %d times; want at most 2", id, n)
		}
	}
	sameTrees(t, "after apply", e.fd.tree(t, "UpdateMusic"), e.localTree())
}

func TestOneChangingRunPerFolder(t *testing.T) {
	e := uploaded(t)
	release, err := lockFolder(e.cfg, "UpdateMusic", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	e.curate()
	if _, err := e.syncCheck(context.Background(), e.src, nil); err != nil {
		t.Fatalf("a check must not need the lock: %v\n%s", err, e.out)
	}
	before := e.fd.snapshotCounts()
	err = e.apply(context.Background(), func(c *Config) { c.Yes, c.AllowManyRemovals = true, true })
	var fe *gdrive.FatalError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "another dhaara run") || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("apply while locked: want a clear stop, got %v", err)
	}
	if d := delta(before, e.fd.snapshotCounts()); d["trash"]+d["move"]+d["initiate"]+d["multipart"] != 0 {
		t.Fatalf("apply changed Drive while another run held the lock: %v", d)
	}
	if e.planFor("sync", e.src) == nil {
		t.Fatal("the plan must stay pending after a locked apply")
	}
	release()
	if err := e.apply(context.Background(), func(c *Config) { c.Yes, c.AllowManyRemovals = true, true }); err != nil {
		t.Fatalf("apply after the lock was released: %v\n%s", err, e.out)
	}
	sameTrees(t, "after apply", e.fd.tree(t, "UpdateMusic"), e.localTree())
}

// The prediction made before a sync apply must equal what Drive holds after.
func TestPredictedDriveMatchesApply(t *testing.T) {
	e := uploaded(t)
	e.curate()
	if _, err := e.syncCheck(context.Background(), e.src, nil); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	var pred drivePrediction
	if err := e.apply(context.Background(), func(c *Config) {
		c.Yes, c.AllowManyRemovals, c.predict = true, true, &pred
	}); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if pred.files == nil {
		t.Fatal("no prediction was made")
	}
	got := map[string]string{}
	for rel, data := range e.fd.tree(t, "UpdateMusic") {
		got[rel] = fmt.Sprintf("%s %d", md5hex(data), len(data))
	}
	want := map[string]string{}
	for rel, r := range pred.files {
		want[rel] = fmt.Sprintf("%s %d", r.MD5, r.Size)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Drive after apply differs from the prediction\n got %v\nwant %v", got, want)
	}

	ctx := context.Background()
	d, _, err := connect(ctx, e.cfg, term.NewPlain(e.out))
	if err != nil {
		t.Fatal(err)
	}
	ls := d.NewListings()
	rootID, _, err := gdrive.ResolveFolder(ctx, ls, "UpdateMusic", false)
	if err != nil {
		t.Fatal(err)
	}
	real, err := loadRemoved(ctx, d, ls, "UpdateMusic", rootID)
	if err != nil {
		t.Fatal(err)
	}
	strip := func(es []removedEntry) map[string]bool {
		m := map[string]bool{}
		for _, x := range es {
			m[x.Path+" "+x.MD5+" "+x.Why] = true
		}
		return m
	}
	if g, w := strip(real.entries), strip(pred.removed); len(g) == 0 || !reflect.DeepEqual(g, w) {
		t.Errorf("removed list after apply differs from the prediction\n got %v\nwant %v", g, w)
	}
}
