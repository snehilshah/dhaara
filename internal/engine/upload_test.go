package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/platform"
	"github.com/snehilshah/dhaara/internal/term"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

var fastRetry = gdrive.RetryPolicy{
	MaxRetries:   10,
	BaseDelay:    time.Millisecond,
	MaxDelay:     4 * time.Millisecond,
	FileAttempts: 3,
	FilePause:    time.Millisecond,
	StallTimeout: 5 * time.Second,
}

var testMtime = time.Date(2023, 10, 30, 10, 14, 50, 0, time.UTC)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type env struct {
	t   *testing.T
	fd  *fakeDrive
	src string
	cfg Config
	out *lockedBuffer
}

func newEnv(t *testing.T) *env { return newEnvWith(t, newFakeDrive(t)) }

func newEnvWith(t *testing.T, fd *fakeDrive) *env {
	t.Helper()
	tmp := t.TempDir()
	src := filepath.Join(tmp, "UpdateMusic")
	makeMusicFolder(t, src)
	cfg := Config{
		Source: src, Workers: 4, ChunkSize: 256 << 10,
		ClientSecret: filepath.Join(tmp, "config", "client_secret.json"),
		ConfigDir:    filepath.Join(tmp, "config"),
		StateDir:     filepath.Join(tmp, "state"),
		Endpoints:    fd.endpoints(),
		Retry:        fastRetry,
		NoInhibit:    true,
		TLSRoots:     fd.roots(),
	}
	must(t, os.MkdirAll(cfg.ConfigDir, 0o700))
	must(t, os.WriteFile(cfg.ClientSecret, []byte(`{"installed":{"client_id":"id","client_secret":"s"}}`), 0o600))
	// An expired access token forces a refresh on the first request.
	must(t, platform.WritePrivateJSON(filepath.Join(cfg.ConfigDir, "token.json"), gdrive.Token{AccessToken: "old", RefreshToken: "r"}))
	return &env{t: t, fd: fd, src: src, cfg: cfg, out: &lockedBuffer{}}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// makeMusicFolder builds a folder like the real one: nested albums, odd
// characters, empty files, and sizes around the 256 KiB chunk edges.
func makeMusicFolder(t *testing.T, root string) {
	t.Helper()
	rng := rand.New(rand.NewPCG(7, 7))
	const K = 256 << 10
	sizes := []int{0, 1, 1000, K - 1, K, K + 1, 2 * K, 2*K + K/2, 5*K + 123}
	layout := map[string][]string{
		"":                                {"top.mp3", "notes.txt"},
		"lofi":                            {"memory reboot (slowed).mp3", "it's fine.lrc"},
		"english/Ed Sheeran - ÷ (Deluxe)": {"01 Song ⧸⧸ 'x'.mp3", "02.mp3", "03.mp3", "04.mp3", "05.mp3", "cover.jpg", "01.lrc"},
		"english/Mikey/HIStory/CD1":       {"t0.mp3", "t1.mp3", "t2.mp3", "t3.mp3", "t4.mp3"},
		"english/Mikey/HIStory/CD2":       {"t0.mp3", "t1.mp3", "t2.mp3", "t3.mp3", "t4.mp3"},
		"hindi/deemix/A.R. Rahman - Guru": {"g0.mp3", "g1.mp3", "g2.mp3", "g0.lrc", "g1.lrc", "g2.lrc"},
		"empty-folder":                    nil,
	}
	i := 0
	for dir, names := range layout {
		must(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
		for _, n := range names {
			data := make([]byte, sizes[(i+rng.IntN(3))%len(sizes)])
			i++
			for j := range data {
				data[j] = byte(rng.Uint32())
			}
			p := filepath.Join(root, dir, n)
			must(t, os.WriteFile(p, data, 0o644))
			must(t, os.Chtimes(p, testMtime, testMtime))
		}
	}
}

func (e *env) run(ctx context.Context) (Summary, error) {
	e.t.Helper()
	return Upload(ctx, e.cfg, term.NewPlain(e.out))
}

func (e *env) localTree() map[string][]byte {
	out := map[string][]byte{}
	filepath.WalkDir(e.src, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(e.src, p)
			out[filepath.ToSlash(rel)], _ = os.ReadFile(p)
		}
		return nil
	})
	return out
}

// checkDrive fails unless Drive holds exactly the local files, byte for byte.
func (e *env) checkDrive(dest string) {
	e.t.Helper()
	remote, local := e.fd.tree(e.t, dest), e.localTree()
	for p, data := range local {
		got, ok := remote[p]
		switch {
		case !ok:
			e.t.Errorf("missing on Drive: %s", p)
		case !bytes.Equal(got, data):
			e.t.Errorf("content differs on Drive: %s", p)
		}
	}
	for p := range remote {
		if _, ok := local[p]; !ok {
			e.t.Errorf("extra file on Drive: %s", p)
		}
	}
}

func delta(before, after map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			out[k] = d
		}
	}
	return out
}

func TestUploadSurvivesFaults(t *testing.T) {
	e := newEnv(t)
	e.fd.failRate = 0.15
	e.fd.chunkChaos = true
	sum, err := e.run(context.Background())
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, e.out)
	}
	e.checkDrive("UpdateMusic")
	c := e.fd.snapshotCounts()
	t.Logf("faults injected: %v; summary %+v", c, sum)
	for _, k := range []string{"chaos", "half_drop", "lost_reply", "corrupt", "multipart_lost_reply"} {
		if c[k] == 0 {
			t.Errorf("fault %q was never injected; test is not exercising it", k)
		}
	}
	if !sum.Verified {
		t.Error("run did not report verified")
	}
	if sum.DuplicatesRemoved == 0 {
		t.Error("lost multipart replies should have produced duplicates to clean up")
	}
	want := testMtime.Format("2006-01-02T15:04:05.000Z07:00")
	e.fd.mu.Lock()
	for _, f := range e.fd.files {
		if f.Mime != gdrive.FolderMime && f.ModifiedTime != want {
			t.Errorf("%s: modifiedTime %q, want %q", f.Name, f.ModifiedTime, want)
		}
	}
	e.fd.mu.Unlock()
}

func TestRerunUploadsNothing(t *testing.T) {
	e := newEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := e.fd.snapshotCounts()
	sum, err := e.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := delta(before, e.fd.snapshotCounts())
	if d["initiate"]+d["multipart"] != 0 || sum.Uploaded != 0 || sum.AlreadyThere != len(e.localTree()) {
		t.Errorf("second run should upload nothing: %v %+v", d, sum)
	}
}

func TestChangedFileIsReplaced(t *testing.T) {
	e := newEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.src, "lofi", "it's fine.lrc")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n[00:01.00] a new lyric line")
	f.Close()
	before := e.fd.snapshotCounts()
	sum, err := e.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := delta(before, e.fd.snapshotCounts())
	if sum.Uploaded != 1 || d["trash"] != 1 {
		t.Errorf("want 1 upload and the old copy trashed: %v %+v", d, sum)
	}
	e.checkDrive("UpdateMusic")
}

func TestStopAndResume(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.fd.onChunk = func(n int, complete bool) {
		if n >= 3 && !complete {
			cancel() // like pressing Ctrl+C in the middle of a file
		}
	}
	_, err := e.run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want a stop, got %v", err)
	}
	e.fd.mu.Lock()
	e.fd.onChunk = nil
	e.fd.mu.Unlock()
	sessions, _ := filepath.Glob(filepath.Join(e.cfg.StateDir, "*", "sessions.json"))
	if len(sessions) != 1 {
		t.Fatal("no saved sessions after stopping")
	}
	if raw, _ := os.ReadFile(sessions[0]); len(raw) < 10 {
		t.Fatalf("expected open sessions, got %s", raw)
	}
	sum, err := e.run(context.Background())
	if err != nil {
		t.Fatalf("resume run failed: %v\n%s", err, e.out)
	}
	if sum.Resumed == 0 {
		t.Error("no upload was resumed from where it stopped")
	}
	e.checkDrive("UpdateMusic")
}

func TestNotEnoughSpace(t *testing.T) {
	e := newEnv(t)
	e.fd.limit = 1000
	_, err := e.run(context.Background())
	var fe *gdrive.FatalError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "not enough Google Drive space") {
		t.Fatalf("want a space error, got %v", err)
	}
	if e.fd.count("initiate")+e.fd.count("multipart") != 0 {
		t.Error("uploaded files despite not enough space")
	}
}

func TestVerifyOnlyFindsMissingFile(t *testing.T) {
	e := newEnv(t)
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.fd.mu.Lock()
	for _, f := range e.fd.files {
		if f.Name == "t3.mp3" && len(f.Data) > 0 {
			f.Trashed = true
			break
		}
	}
	e.fd.mu.Unlock()

	e.cfg.VerifyOnly = true
	if _, err := e.run(context.Background()); !errors.Is(err, ErrProblems) {
		t.Fatalf("verify-only should report a problem, got %v", err)
	}
	e.cfg.VerifyOnly = false
	sum, err := e.run(context.Background())
	if err != nil || sum.Uploaded != 1 {
		t.Fatalf("rerun should upload just the missing file: %v %+v", err, sum)
	}
	e.checkDrive("UpdateMusic")
}

func TestStalledConnectionIsAbandoned(t *testing.T) {
	e := newEnv(t)
	e.cfg.Retry.StallTimeout = 200 * time.Millisecond
	e.fd.hangOnce = 3 * time.Second
	start := time.Now()
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.fd.count("hang") != 1 {
		t.Fatal("stall was not injected")
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Error("the stalled request was not abandoned early")
	}
	e.checkDrive("UpdateMusic")
}

func TestRevokedSignInStopsCleanly(t *testing.T) {
	e := newEnv(t)
	e.fd.revoked = true
	_, err := e.run(context.Background())
	var fe *gdrive.FatalError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "sign-in expired") {
		t.Fatalf("want sign-in error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.ConfigDir, "token.json")); !os.IsNotExist(err) {
		t.Error("revoked token should be removed so the next run signs in again")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.cfg.DryRun = true
	if _, err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.fd.files); n != 0 {
		t.Errorf("dry run created %d items on Drive", n)
	}
	if !strings.Contains(e.out.String(), "would upload") {
		t.Error("dry run did not list files")
	}
}

func TestMissingCredentialsExplainsSetup(t *testing.T) {
	e := newEnv(t)
	os.Remove(e.cfg.ClientSecret)
	_, err := e.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "console.cloud.google.com") {
		t.Fatalf("want setup instructions, got %v", err)
	}
}

// Google's servers offer HTTP/2. dhaara must still talk plain HTTP/1.1 to
// them (a mix-up here once broke sign-in on the real Google servers).
func TestHTTPSWithHTTP2Server(t *testing.T) {
	e := newEnvWith(t, newFakeDriveTLS(t))
	e.fd.failRate = 0.1
	e.fd.chunkChaos = true
	if _, err := e.run(context.Background()); err != nil {
		t.Fatalf("run over HTTPS failed: %v\n%s", err, e.out)
	}
	if n := e.fd.count("http2"); n != 0 {
		t.Errorf("%d requests used HTTP/2; want HTTP/1.1 only", n)
	}
	e.checkDrive("UpdateMusic")
}

// When retries drag on, the user is told in the terminal.
func TestLongRetriesAreShown(t *testing.T) {
	e := newEnv(t)
	e.fd.failRate = 0.6
	e.run(context.Background())
	if !strings.Contains(e.out.String(), "Having trouble talking to Google") {
		t.Error("no retry notice was printed")
	}
}
