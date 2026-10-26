package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/term"
)

// uploaded returns an env whose folder is already on the fake Drive.
func uploaded(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatalf("upload failed: %v\n%s", err, e.out)
	}
	return e
}

func (e *env) download(ctx context.Context, local string, change func(*Config)) (DownloadSummary, error) {
	e.t.Helper()
	cfg := e.cfg
	cfg.Download, cfg.Source, cfg.RemoteName = true, local, "UpdateMusic"
	if change != nil {
		change(&cfg)
	}
	return Download(ctx, cfg, term.NewPlain(e.out))
}

// checkDownloaded fails unless dir holds exactly the original files, with
// their original modified times, and no leftover partial files.
func (e *env) checkDownloaded(dir string) {
	e.t.Helper()
	want := e.localTree()
	got := map[string][]byte{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if strings.HasSuffix(d.Name(), partSuffix) {
			e.t.Errorf("leftover partial file: %s", rel)
			return nil
		}
		got[filepath.ToSlash(rel)], _ = os.ReadFile(p)
		if fi, _ := d.Info(); !fi.ModTime().Equal(testMtime) {
			e.t.Errorf("%s: modified time %v, want %v", rel, fi.ModTime(), testMtime)
		}
		return nil
	})
	for p, data := range want {
		if g, ok := got[p]; !ok {
			e.t.Errorf("not downloaded: %s", p)
		} else if !bytes.Equal(g, data) {
			e.t.Errorf("downloaded content differs: %s", p)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			e.t.Errorf("unexpected file: %s", p)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "empty-folder")); err != nil || !fi.IsDir() {
		e.t.Error("empty folder was not recreated")
	}
}

func TestDownloadSurvivesFaults(t *testing.T) {
	e := uploaded(t)
	e.fd.failRate = 0.1
	e.fd.dlChaos = true
	dir := filepath.Join(t.TempDir(), "restored")
	sum, err := e.download(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("download failed: %v\n%s", err, e.out)
	}
	e.checkDownloaded(dir)
	c := e.fd.snapshotCounts()
	t.Logf("faults: %v; summary %+v", c, sum)
	for _, k := range []string{"dl_cut", "ignored_range", "dl_corrupt", "chaos"} {
		if c[k] == 0 {
			t.Errorf("fault %q was never injected", k)
		}
	}
	if !sum.Verified {
		t.Error("not reported as verified")
	}
}

func TestDownloadRerunFetchesNothing(t *testing.T) {
	e := uploaded(t)
	dir := filepath.Join(t.TempDir(), "restored")
	if _, err := e.download(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	before := e.fd.count("media")
	sum, err := e.download(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.fd.count("media") != before || sum.Downloaded != 0 || sum.AlreadyThere != len(e.localTree()) {
		t.Errorf("second download should fetch nothing: %+v", sum)
	}
}

func TestDownloadIntoOriginalFolderIsNoop(t *testing.T) {
	e := uploaded(t)
	sum, err := e.download(context.Background(), e.src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.fd.count("media") != 0 || !sum.Verified {
		t.Errorf("nothing should be downloaded into an identical folder: %+v", sum)
	}
}

func TestDownloadStopAndResume(t *testing.T) {
	e := uploaded(t)
	dir := filepath.Join(t.TempDir(), "restored")
	ctx, cancel := context.WithCancel(context.Background())
	stopped := false
	e.fd.mu.Lock()
	e.fd.onMedia = func(n, size int) {
		if size >= 512<<10 && !stopped {
			stopped = true
			time.Sleep(200 * time.Millisecond) // let the first half arrive
			cancel()                           // like pressing Ctrl+C mid-file
		}
	}
	e.fd.mu.Unlock()
	if _, err := e.download(ctx, dir, func(c *Config) { c.Workers = 1 }); !errors.Is(err, context.Canceled) {
		t.Fatalf("want a stop, got %v", err)
	}
	e.fd.mu.Lock()
	e.fd.onMedia = nil
	e.fd.mu.Unlock()
	parts := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(d.Name(), partSuffix) {
			if fi, _ := d.Info(); fi.Size() > 0 {
				parts++
			}
		}
		return nil
	})
	if parts == 0 {
		t.Fatal("no partial file kept after stopping")
	}
	sum, err := e.download(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("resume failed: %v\n%s", err, e.out)
	}
	if sum.Resumed == 0 {
		t.Error("partial download was not resumed")
	}
	e.checkDownloaded(dir)
}

func TestDownloadKeepsDifferentLocalFiles(t *testing.T) {
	e := uploaded(t)
	dir := filepath.Join(t.TempDir(), "restored")
	if _, err := e.download(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "lofi", "it's fine.lrc")
	must(t, os.WriteFile(mine, []byte("my own edits"), 0o644))

	sum, err := e.download(context.Background(), dir, nil)
	if !errors.Is(err, ErrProblems) || sum.Conflicts != 1 {
		t.Fatalf("want 1 conflict reported, got %v %+v", err, sum)
	}
	if b, _ := os.ReadFile(mine); string(b) != "my own edits" {
		t.Fatal("local file was overwritten without --overwrite")
	}
	if _, err := e.download(context.Background(), dir, func(c *Config) { c.Overwrite = true }); err != nil {
		t.Fatalf("--overwrite run failed: %v", err)
	}
	e.checkDownloaded(dir)
}

func TestDownloadRefusesUnsafeNames(t *testing.T) {
	e := uploaded(t)
	e.fd.mu.Lock()
	var rootID string
	for _, f := range e.fd.files {
		if f.Parent == "root" && f.Name == "UpdateMusic" {
			rootID = f.ID
		}
	}
	for _, name := range []string{"../evil.mp3", "sub/dir.mp3", ".."} {
		e.fd.create(map[string]any{"name": name, "parents": []any{rootID}}, []byte("bad"), "audio/mpeg")
	}
	e.fd.mu.Unlock()

	base := t.TempDir()
	dir := filepath.Join(base, "restored")
	sum, err := e.download(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("download failed: %v\n%s", err, e.out)
	}
	if sum.Skipped != 3 {
		t.Errorf("want 3 unsafe items skipped, got %d", sum.Skipped)
	}
	if _, err := os.Stat(filepath.Join(base, "evil.mp3")); err == nil {
		t.Fatal("a file was written outside the download folder")
	}
	e.checkDownloaded(dir)
}

func TestDownloadMissingFolder(t *testing.T) {
	e := newEnv(t)
	_, err := e.download(context.Background(), t.TempDir(), nil)
	var fe *gdrive.FatalError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "no folder named") {
		t.Fatalf("want missing-folder error, got %v", err)
	}
}

func TestDownloadNotEnoughDisk(t *testing.T) {
	e := uploaded(t)
	orig := freeSpace
	freeSpace = func(string) int64 { return 10 }
	defer func() { freeSpace = orig }()
	_, err := e.download(context.Background(), filepath.Join(t.TempDir(), "x"), nil)
	if err == nil || !strings.Contains(err.Error(), "not enough disk space") {
		t.Fatalf("want disk space error, got %v", err)
	}
	if e.fd.count("media") != 0 {
		t.Error("downloaded despite not enough space")
	}
}

func TestDownloadStallIsAbandoned(t *testing.T) {
	e := uploaded(t)
	e.cfg.Retry.StallTimeout = 200 * time.Millisecond
	e.fd.mu.Lock()
	e.fd.hangMedia = 3 * time.Second
	e.fd.mu.Unlock()
	dir := filepath.Join(t.TempDir(), "restored")
	start := time.Now()
	if _, err := e.download(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	if e.fd.count("dl_hang") != 1 {
		t.Fatal("stall was not injected")
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Error("stalled download was not abandoned early")
	}
	e.checkDownloaded(dir)
}

func TestUploadSkipsUnfinishedDownloads(t *testing.T) {
	e := newEnv(t)
	must(t, os.WriteFile(filepath.Join(e.src, "lofi", partPrefix+"abc"+partSuffix), []byte("half"), 0o644))
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for p := range e.fd.tree(t, "UpdateMusic") {
		if strings.HasSuffix(p, partSuffix) {
			t.Errorf("unfinished download was uploaded: %s", p)
		}
	}
}
