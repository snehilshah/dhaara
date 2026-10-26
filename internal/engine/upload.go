package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/term"
)

// ErrProblems means the run finished but some files failed or did not verify.
var ErrProblems = errors.New("some files could not be uploaded or verified")

// Summary describes what a run did (used by tests and the final message).
type Summary struct {
	Uploaded, AlreadyThere, Failed, Resumed, DuplicatesRemoved int
	Verified                                                   bool
}

// Upload sends new and changed files to the Drive folder. It never removes
// anything from Drive except replaced or duplicate copies (to Drive's trash).
func Upload(ctx context.Context, cfg Config, ui *term.Console) (Summary, error) {
	var sum Summary
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

	dirs, files, _, err := scanFolder(src, cfg.Exclude, ui)
	if err != nil {
		return sum, err
	}

	key := sha256.Sum256([]byte(src + "\x00" + dest))
	st, err := openState(filepath.Join(cfg.StateDir, hex.EncodeToString(key[:8])))
	if err != nil {
		return sum, gdrive.Fatalf("cannot create state folder: %v", err)
	}
	defer st.Flush()

	d, about, err := connect(ctx, cfg, ui)
	if err != nil {
		return sum, err
	}
	limit, hasLimit := parseInt(about.StorageQuota.Limit)
	usage, _ := parseInt(about.StorageQuota.Usage)

	if cfg.VerifyOnly {
		return verify(ctx, d, st, files, dirs, dest, ui, false, sum)
	}

	ui.Printf("Checking what is already in the Drive folder %q...", dest)
	tree, err := loadTree(ctx, d, dest, dirs, !cfg.DryRun)
	if err != nil {
		return sum, err
	}
	var todo []*LocalFile
	var need int64
	for _, f := range files {
		f.ParentID = tree.ids[f.RelDir]
		same, err := tree.sameName(ctx, f)
		if err != nil {
			return sum, err
		}
		match, err := anyMatch(f, same, st)
		if err != nil {
			return sum, err
		}
		if match != nil {
			sum.AlreadyThere++
			continue
		}
		for _, x := range same {
			f.ReplaceIDs = append(f.ReplaceIDs, x.ID)
		}
		todo = append(todo, f)
		need += f.Size
	}
	ui.Printf("Already on Drive: %d files.  To upload: %d files (%s).", sum.AlreadyThere, len(todo), term.Size(need))

	if cfg.DryRun {
		for i, f := range todo {
			if i == 100 {
				ui.Printf("  ... and %d more", len(todo)-100)
				break
			}
			ui.Printf("  would upload %s (%s)", f.Rel, term.Size(f.Size))
		}
		ui.Println("Dry run: nothing was uploaded or changed.")
		return sum, nil
	}

	if hasLimit && need > limit-usage {
		return sum, gdrive.Fatalf("not enough Google Drive space: this needs %s but only %s is free. "+
			"Free up space (and empty Drive's trash) or upgrade storage, then run again",
			term.Size(need), term.Size(max(0, limit-usage)))
	}

	if len(todo) > 0 {
		ui.Printf("Uploading with %d parallel uploads. Safe to stop with Ctrl+C and run again later.", cfg.Workers)
		started := time.Now()
		uploaded, failures, resumed, err := uploadAll(ctx, cfg, d, st, todo, ui)
		sum.Uploaded, sum.Failed, sum.Resumed = uploaded, len(failures), resumed
		if err != nil {
			return sum, err
		}
		msg := fmt.Sprintf("Uploaded %d files in %s", sum.Uploaded, term.Duration(time.Since(started)))
		if sum.Failed > 0 {
			msg += fmt.Sprintf("; %d FAILED (listed above)", sum.Failed)
		}
		ui.Println(msg + ".")
	}

	sum, err = verify(ctx, d, st, files, dirs, dest, ui, true, sum)
	if err == nil && sum.Failed > 0 {
		err = ErrProblems
	}
	return sum, err
}

// connect signs in (opening the browser the first time) and prints the account.
func connect(ctx context.Context, cfg Config, ui *term.Console) (*gdrive.Drive, gdrive.AboutInfo, error) {
	// Show long-running retries in the terminal, not only in the log.
	cfg.Retry.Notice = ui.Println
	hc := gdrive.NewHTTPClient(cfg.TLSRoots)
	auth, err := gdrive.LoadAuth(cfg.ClientSecret, filepath.Join(cfg.ConfigDir, "token.json"), hc, cfg.Endpoints, cfg.Retry)
	if err != nil {
		return nil, gdrive.AboutInfo{}, err
	}
	if !auth.SignedIn() {
		if err := auth.Login(ctx, ui); err != nil {
			return nil, gdrive.AboutInfo{}, err
		}
	}
	d := gdrive.New(hc, cfg.Endpoints, auth, cfg.Retry, cfg.PaceInterval)
	about, err := d.About(ctx)
	if err != nil {
		return nil, gdrive.AboutInfo{}, err
	}
	line := "Google account: " + about.User.EmailAddress
	limit, hasLimit := parseInt(about.StorageQuota.Limit)
	usage, _ := parseInt(about.StorageQuota.Usage)
	if hasLimit {
		line += fmt.Sprintf("  (Drive: %s used of %s)", term.Size(usage), term.Size(limit))
	}
	ui.Println(line)
	return d, about, nil
}

func parseInt(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// uploadAll runs the workers. It returns how many files were uploaded, the
// failed files, how many uploads resumed an earlier session, and a fatal or
// stop error if any.
func uploadAll(ctx context.Context, cfg Config, d *gdrive.Drive, st *State, todo []*LocalFile, ui *term.Console) (int, []string, int, error) {
	prog := newProgress(todo, d.Sent)
	u := &uploader{d: d, st: st, prog: prog, chunk: cfg.ChunkSize, retry: d.Retry()}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var mu sync.Mutex
	var failures []string
	jobs := make(chan *LocalFile)
	var wg sync.WaitGroup
	for range cfg.Workers {
		wg.Go(func() {
			for f := range jobs {
				err := u.uploadOne(ctx, f)
				var fe *gdrive.FatalError
				switch {
				case err == nil:
					prog.Finish(f, true)
					slog.Info("uploaded and verified", "file", f.Rel)
				case ctx.Err() != nil:
					// stopping; the file will be picked up again next run
				case errors.As(err, &fe):
					cancel(err)
				default:
					prog.Finish(f, false)
					slog.Error("upload failed", "file", f.Rel, "err", err)
					ui.Printf("FAILED  %s: %v", f.Rel, err)
					mu.Lock()
					failures = append(failures, f.Rel)
					mu.Unlock()
				}
			}
		})
	}

	stopReporter := startReporter(ui, prog)

feed:
	for _, f := range todo {
		select {
		case jobs <- f:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	stopReporter()

	uploaded, _ := prog.Counts()
	return uploaded, failures, int(u.resumed.Load()), context.Cause(ctx)
}

// tree maps local folders to Drive folder IDs and caches folder listings.
type tree struct {
	d        *gdrive.Drive
	ids      map[string]string // local rel dir -> Drive folder ID ("" if missing)
	listings map[string][]gdrive.File
}

func (t *tree) children(ctx context.Context, id string) ([]gdrive.File, error) {
	if l, ok := t.listings[id]; ok {
		return l, nil
	}
	l, err := t.d.ListChildren(ctx, id)
	if err == nil {
		t.listings[id] = l
	}
	return l, err
}

// loadTree finds (and, if create is set, makes) the Drive folder for every
// local folder. When several folders share a name, the oldest one is used.
func loadTree(ctx context.Context, d *gdrive.Drive, dest string, dirs []string, create bool) (*tree, error) {
	t := &tree{d: d, ids: map[string]string{}, listings: map[string][]gdrive.File{}}
	ensure := func(name, parent string) (string, error) {
		kids, err := t.children(ctx, parent)
		if err != nil {
			return "", err
		}
		for _, k := range kids {
			if k.IsFolder() && k.Name == name {
				return k.ID, nil
			}
		}
		if !create {
			return "", nil
		}
		id, err := d.CreateFolder(ctx, name, parent)
		if err != nil {
			return "", err
		}
		t.listings[parent] = append(t.listings[parent], gdrive.File{ID: id, Name: name, MimeType: gdrive.FolderMime})
		t.listings[id] = nil
		return id, nil
	}
	root, _, err := gdrive.ResolveFolder(ctx, d, dest, create)
	if err != nil {
		return nil, err
	}
	t.ids[""] = root
	for _, dir := range dirs { // parents always come before their children
		parentRel := filepath.Dir(dir)
		if parentRel == "." {
			parentRel = ""
		}
		parent := t.ids[parentRel]
		if parent == "" {
			t.ids[dir] = ""
			continue
		}
		if t.ids[dir], err = ensure(filepath.Base(dir), parent); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// sameName returns Drive files in f's folder with f's name, oldest first.
func (t *tree) sameName(ctx context.Context, f *LocalFile) ([]gdrive.File, error) {
	parent := t.ids[f.RelDir]
	if parent == "" {
		return nil, nil
	}
	kids, err := t.children(ctx, parent)
	if err != nil {
		return nil, err
	}
	var out []gdrive.File
	for _, k := range kids {
		if !k.IsFolder() && k.Name == f.Name {
			out = append(out, k)
		}
	}
	return out, nil
}

// matching returns the Drive copies whose size and MD5 equal the local file's.
func matching(f *LocalFile, items []gdrive.File, st *State) ([]gdrive.File, error) {
	var out []gdrive.File
	for _, x := range items {
		if x.SizeBytes() != f.Size {
			continue
		}
		if f.Size == 0 && x.MD5 == "" {
			out = append(out, x)
			continue
		}
		sum, err := st.MD5(f)
		if err != nil {
			return nil, err
		}
		if x.MD5 == sum {
			out = append(out, x)
		}
	}
	return out, nil
}

func anyMatch(f *LocalFile, items []gdrive.File, st *State) (*gdrive.File, error) {
	m, err := matching(f, items, st)
	if err != nil || len(m) == 0 {
		return nil, err
	}
	return &m[0], nil
}

// verify re-reads the Drive folder and checks every local file is there with
// the same size and MD5. With dedupe set, extra identical copies (left by a
// retried upload whose first reply was lost) are moved to Drive's trash.
func verify(ctx context.Context, d *gdrive.Drive, st *State, files []*LocalFile, dirs []string, dest string,
	ui *term.Console, dedupe bool, sum Summary) (Summary, error) {
	ui.Println("Verifying every file on Drive (size + MD5 checksum)...")
	t, err := loadTree(ctx, d, dest, dirs, false)
	if err != nil {
		return sum, err
	}
	var bad []string
	for _, f := range files {
		same, err := t.sameName(ctx, f)
		if err != nil {
			return sum, err
		}
		m, err := matching(f, same, st)
		if err != nil {
			return sum, err
		}
		if len(m) == 0 {
			bad = append(bad, f.Rel)
			continue
		}
		if dedupe {
			for _, extra := range m[1:] { // keep the oldest copy
				if err := d.Trash(ctx, extra.ID); err != nil {
					return sum, err
				}
				sum.DuplicatesRemoved++
				slog.Info("trashed duplicate copy", "file", f.Rel, "id", extra.ID)
			}
		}
	}
	if len(bad) > 0 {
		ui.Printf("NOT VERIFIED: %d of %d files are missing or different on Drive:", len(bad), len(files))
		for i, rel := range bad {
			if i == 50 {
				ui.Printf("  ... and %d more (see the log file)", len(bad)-50)
				break
			}
			ui.Printf("  %s", rel)
		}
		slog.Error("not verified", "files", bad)
		ui.Println("Run the same command again to upload them.")
		return sum, ErrProblems
	}
	sum.Verified = true
	ui.Printf("VERIFIED: all %d files are on Drive and match your local copies exactly.", len(files))
	if sum.DuplicatesRemoved > 0 {
		ui.Printf("Moved %d duplicate copies to Drive's trash.", sum.DuplicatesRemoved)
	}
	return sum, nil
}
