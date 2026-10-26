package engine

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/term"
)

// DownloadSummary describes what a download run did.
type DownloadSummary struct {
	Downloaded, AlreadyThere, Conflicts, Failed, Resumed, Skipped int
	Verified                                                      bool
}

// remoteFile is one file inside the Drive folder being downloaded.
type remoteFile struct {
	Rel      string // local relative path
	ID       string
	Size     int64
	MD5      string
	Modified time.Time
	Parent   string // Drive folder ID
}

func (rf remoteFile) local() *LocalFile { return &LocalFile{Rel: rf.Rel, Size: rf.Size} }

// partSuffix marks unfinished downloads; upload skips these files.
const partPrefix, partSuffix = ".dhaara-", ".part"

// Download fetches the Drive folder into cfg.Source; with cfg.Pull it also
// follows the removed list (moves, removals, replaced versions).
func Download(ctx context.Context, cfg Config, ui *term.Console) (DownloadSummary, error) {
	var sum DownloadSummary
	target, err := filepath.Abs(cfg.Source)
	if err != nil {
		return sum, err
	}
	if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
		return sum, gdrive.Fatalf("not a folder: %s", target)
	}

	d, _, err := connect(ctx, cfg, ui)
	if err != nil {
		return sum, err
	}
	ui.Printf("Reading the Drive folder %q...", cfg.RemoteName)
	rootID, _, err := gdrive.ResolveFolder(ctx, d, cfg.RemoteName, false)
	if err != nil {
		return sum, err
	}
	if rootID == "" {
		return sum, gdrive.Fatalf("no folder named %q found in your Drive. dhaara can only see folders it uploaded itself", cfg.RemoteName)
	}
	dirs, files, skipped, err := listRemote(ctx, d, rootID)
	if err != nil {
		return sum, err
	}
	for _, s := range skipped {
		ui.Printf("Skipping on Drive: %s", s)
	}
	sum.Skipped = len(skipped)
	var excludedRemote []string
	files, excludedRemote = excludeItems(files, func(rf remoteFile) string { return rf.Rel }, cfg.Exclude)
	dirs, _ = excludeItems(dirs, func(s string) string { return s }, cfg.Exclude)
	var total int64
	for _, rf := range files {
		total += rf.Size
	}
	ui.Printf("On Drive: %d files, %s, %d subfolders.  Local folder: %s", len(files), term.Size(total), len(dirs), target)
	printExcluded(ui, "on Drive", excludedRemote)

	key := sha256.Sum256([]byte("download\x00" + target + "\x00" + cfg.RemoteName))
	st, err := openState(filepath.Join(cfg.StateDir, "dl-"+hex.EncodeToString(key[:8])))
	if err != nil {
		return sum, gdrive.Fatalf("cannot create state folder: %v", err)
	}
	defer st.Flush()

	replaceOK := map[string]bool{}
	if cfg.Pull {
		done, err := pullRemovalsAndMoves(ctx, cfg, ui, d, st, target, rootID, files, replaceOK)
		if err != nil || done {
			return sum, err
		}
	}

	// Decide what to fetch.
	var todo []remoteFile
	var conflicts []string
	var need int64
	for _, rf := range files {
		path := filepath.Join(target, rf.Rel)
		fi, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			todo = append(todo, rf)
			need += rf.Size
			continue
		case err != nil:
			return sum, &gdrive.LocalError{Err: err}
		case !fi.Mode().IsRegular():
			conflicts = append(conflicts, rf.Rel+" (a folder or link is in the way)")
			continue
		}
		same, err := sameAsDrive(st, rf, path, fi)
		if err != nil {
			return sum, err
		}
		switch {
		case same:
			sum.AlreadyThere++
			restoreTime(path, rf, fi)
		case cfg.Overwrite || replaceOK[rf.Rel]:
			todo = append(todo, rf)
			need += rf.Size
		default:
			conflicts = append(conflicts, rf.Rel)
		}
	}
	sum.Conflicts = len(conflicts)
	ui.Printf("Already here: %d files.  To download: %d files (%s).", sum.AlreadyThere, len(todo), term.Size(need))
	if len(conflicts) > 0 {
		ui.Printf("%d local files differ from Drive and were left untouched (use --overwrite to replace them):", len(conflicts))
		for i, c := range conflicts {
			if i == 20 {
				ui.Printf("  ... and %d more (see the log file)", len(conflicts)-20)
				break
			}
			ui.Printf("  %s", c)
		}
		slog.Warn("local files differ from Drive", "files", conflicts)
	}

	if cfg.DryRun {
		for i, rf := range todo {
			if i == 100 {
				ui.Printf("  ... and %d more", len(todo)-100)
				break
			}
			ui.Printf("  would download %s (%s)", rf.Rel, term.Size(rf.Size))
		}
		ui.Println("Dry run: nothing was downloaded or changed.")
		return sum, nil
	}

	if free := freeSpace(existingParent(target)); free >= 0 && need > free {
		return sum, gdrive.Fatalf("not enough disk space: this needs %s but only %s is free on %s",
			term.Size(need), term.Size(free), existingParent(target))
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return sum, &gdrive.LocalError{Err: err}
	}
	if cfg.Pull { // only folders that hold files: Drive keeps emptied folders
		dirs = nil
		seenDir := map[string]bool{}
		for _, rf := range files {
			for _, p := range parentDirs(rf.Rel) {
				if !seenDir[p] {
					seenDir[p] = true
					dirs = append(dirs, p)
				}
			}
		}
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(target, dir), 0o755); err != nil {
			return sum, &gdrive.LocalError{Err: err}
		}
	}

	if len(todo) > 0 {
		ui.Printf("Downloading with %d parallel downloads. Safe to stop with Ctrl+C and run again later.", cfg.Workers)
		started := time.Now()
		downloaded, failed, resumed, err := downloadAll(ctx, cfg, d, st, target, todo, ui)
		sum.Downloaded, sum.Failed, sum.Resumed = downloaded, len(failed), resumed
		if err != nil {
			return sum, err
		}
		msg := fmt.Sprintf("Downloaded %d files in %s", downloaded, term.Duration(time.Since(started)))
		if len(failed) > 0 {
			msg += fmt.Sprintf("; %d FAILED (listed above)", len(failed))
		}
		ui.Println(msg + ".")
	}

	// Final check of every file, including ones that were already here.
	ui.Println("Verifying every local file against Drive (size + MD5 checksum)...")
	var bad []string
	for _, rf := range files {
		path := filepath.Join(target, rf.Rel)
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			bad = append(bad, rf.Rel)
			continue
		}
		same, err := sameAsDrive(st, rf, path, fi)
		if err != nil {
			return sum, err
		}
		if !same {
			bad = append(bad, rf.Rel)
		}
	}
	if len(bad) > 0 {
		ui.Printf("NOT VERIFIED: %d of %d files are missing or different here:", len(bad), len(files))
		for i, rel := range bad {
			if i == 50 {
				ui.Printf("  ... and %d more (see the log file)", len(bad)-50)
				break
			}
			ui.Printf("  %s", rel)
		}
		slog.Error("not verified", "files", bad)
		if len(conflicts) > 0 && !cfg.Overwrite {
			ui.Println("Files that differ were kept as they are; add --overwrite to replace them with Drive's copy.")
		} else {
			ui.Println("Run the same command again to download them.")
		}
		return sum, ErrProblems
	}
	sum.Verified = true
	ui.Printf("VERIFIED: all %d files are here and match Drive exactly.", len(files))
	if sum.Failed > 0 {
		return sum, ErrProblems
	}
	return sum, nil
}

// listRemote walks the Drive folder. It returns local relative folder paths,
// the files to download, and reasons for anything skipped.
func listRemote(ctx context.Context, d *gdrive.Drive, rootID string) (dirs []string, files []remoteFile, skipped []string, err error) {
	t, err := listTree(ctx, d, rootID)
	if err != nil {
		return nil, nil, nil, err
	}
	return t.dirs, t.files, t.skipped, nil
}

// sameAsDrive reports whether the local file at path matches Drive's copy.
func sameAsDrive(st *State, rf remoteFile, path string, fi os.FileInfo) (bool, error) {
	if fi.Size() != rf.Size {
		return false, nil
	}
	if rf.Size == 0 || rf.MD5 == "" {
		return true, nil
	}
	sum, err := st.MD5(&LocalFile{Rel: rf.Rel, Path: path, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()})
	if err != nil {
		return false, err
	}
	return sum == rf.MD5, nil
}

// restoreTime gives an identical local file the original modified time.
func restoreTime(path string, rf remoteFile, fi os.FileInfo) {
	if !rf.Modified.IsZero() && !fi.ModTime().Equal(rf.Modified) {
		if err := os.Chtimes(path, rf.Modified, rf.Modified); err != nil {
			slog.Warn("cannot set file time", "file", path, "err", err)
		}
	}
}

// existingParent returns path or its nearest existing parent (for free space).
func existingParent(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func downloadAll(ctx context.Context, cfg Config, d *gdrive.Drive, st *State, target string, todo []remoteFile, ui *term.Console) (int, []string, int, error) {
	locals := make([]*LocalFile, len(todo))
	for i, rf := range todo {
		locals[i] = rf.local()
	}
	prog := newProgress(locals, d.Received)
	dl := &downloader{d: d, st: st, prog: prog, retry: d.Retry(), root: target}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var mu sync.Mutex
	var failures []string
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range cfg.Workers {
		wg.Go(func() {
			for i := range jobs {
				rf, lf := todo[i], locals[i]
				err := dl.fetchOne(ctx, rf, lf)
				var fe *gdrive.FatalError
				switch {
				case err == nil:
					prog.Finish(lf, true)
					slog.Info("downloaded and verified", "file", rf.Rel)
				case ctx.Err() != nil:
					// stopping; the partial file is kept and resumed next run
				case errors.As(err, &fe):
					cancel(err)
				default:
					prog.Finish(lf, false)
					slog.Error("download failed", "file", rf.Rel, "err", err)
					ui.Printf("FAILED  %s: %v", rf.Rel, err)
					mu.Lock()
					failures = append(failures, rf.Rel)
					mu.Unlock()
				}
			}
		})
	}
	stopReporter := startReporter(ui, prog)
feed:
	for i := range todo {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	stopReporter()
	done, _ := prog.Counts()
	return done, failures, int(dl.resumed.Load()), context.Cause(ctx)
}

type downloader struct {
	d       *gdrive.Drive
	st      *State
	prog    *Progress
	retry   gdrive.RetryPolicy
	root    string
	resumed atomic.Int64
}

// fetchOne downloads one file, retrying the whole file a few times if needed.
func (dl *downloader) fetchOne(ctx context.Context, rf remoteFile, lf *LocalFile) error {
	var lastErr error
	for attempt := 1; attempt <= dl.retry.FileAttempts; attempt++ {
		err := dl.tryFetch(ctx, rf, lf)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var fe *gdrive.FatalError
		var le *gdrive.LocalError
		var ae *gdrive.APIError
		switch {
		case errors.As(err, &fe), errors.As(err, &le):
			return err
		case errors.As(err, &ae) && !ae.Retryable():
			return err
		}
		lastErr = err
		slog.Warn("file attempt failed", "file", rf.Rel, "attempt", attempt, "err", err)
		if attempt < dl.retry.FileAttempts {
			if err := gdrive.SleepCtx(ctx, dl.retry.FilePause); err != nil {
				return err
			}
		}
	}
	return lastErr
}

var unsafeIDChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// tryFetch downloads into a hidden ".part" file next to the destination,
// resuming from whatever is already there, checks the MD5, sets the original
// modified time, and only then renames it into place.
func (dl *downloader) tryFetch(ctx context.Context, rf remoteFile, lf *LocalFile) error {
	final := filepath.Join(dl.root, rf.Rel)
	part := filepath.Join(filepath.Dir(final), partPrefix+unsafeIDChars.ReplaceAllString(rf.ID, "_")+partSuffix)
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return &gdrive.LocalError{Err: err}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return &gdrive.LocalError{Err: err}
	}

	have := fi.Size()
	h := md5.New()
	if have > rf.Size {
		have = 0
	}
	if have > 0 {
		if _, err := io.CopyN(h, f, have); err != nil {
			return &gdrive.LocalError{Err: err}
		}
		dl.resumed.Add(1)
		slog.Info("resuming download", "file", rf.Rel, "offset", have)
	}
	if err := f.Truncate(have); err != nil {
		return &gdrive.LocalError{Err: err}
	}
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		return &gdrive.LocalError{Err: err}
	}
	dl.prog.Set(lf, have)

	mediaURL := dl.d.APIURL("/files/"+url.PathEscape(rf.ID), url.Values{"alt": {"media"}})
	r := gdrive.NewRetrier(dl.retry)
	restarts := 0
	for have < rf.Size {
		before := have
		err := dl.d.Stream(ctx, mediaURL, gdrive.Headers("Range", fmt.Sprintf("bytes=%d-", have)),
			func(resp *http.Response, body io.Reader) error {
				switch resp.StatusCode {
				case http.StatusPartialContent:
					if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); !ok || start != have {
						return gdrive.ErrRestart
					}
				case http.StatusOK:
					if have > 0 {
						return gdrive.ErrRestart // the server ignored our Range; start over
					}
				case http.StatusRequestedRangeNotSatisfiable:
					return gdrive.ErrRestart
				default:
					data, _ := io.ReadAll(io.LimitReader(body, 64<<10))
					return gdrive.ParseAPIError(&gdrive.Response{Status: resp.StatusCode, Header: resp.Header, Body: data})
				}
				w := &countingWriter{w: io.MultiWriter(fileWriter{f}, h), n: &have, after: func() { dl.prog.Set(lf, have) }}
				_, err := io.Copy(w, io.LimitReader(body, rf.Size-have))
				return err
			})
		if err == nil && have < rf.Size {
			err = &gdrive.NetError{Err: io.ErrUnexpectedEOF}
		}
		switch {
		case err == nil:
		case errors.Is(err, gdrive.ErrRestart):
			if restarts++; restarts > 5 {
				return errors.New("download kept restarting")
			}
			if err := f.Truncate(0); err != nil {
				return &gdrive.LocalError{Err: err}
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return &gdrive.LocalError{Err: err}
			}
			h.Reset()
			have = 0
			dl.prog.Set(lf, 0)
		case !gdrive.IsRetryable(err):
			return err
		default:
			if have > before {
				r.Reset()
			}
			slog.Warn("download interrupted, retrying", "file", rf.Rel, "offset", have, "err", err)
			if werr := r.Wait(ctx, err); werr != nil {
				return werr
			}
		}
	}

	if err := f.Sync(); err != nil {
		return &gdrive.LocalError{Err: err}
	}
	if err := f.Close(); err != nil {
		return &gdrive.LocalError{Err: err}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if rf.MD5 != "" && sum != rf.MD5 {
		os.Remove(part)
		dl.prog.Set(lf, 0)
		return &gdrive.IntegrityError{Msg: fmt.Sprintf("downloaded copy did not match Drive (md5 %s, expected %s); trying again", sum, rf.MD5)}
	}
	if !rf.Modified.IsZero() {
		if err := os.Chtimes(part, rf.Modified, rf.Modified); err != nil {
			return &gdrive.LocalError{Err: err}
		}
	}
	if err := os.Rename(part, final); err != nil {
		return &gdrive.LocalError{Err: err}
	}
	mt := rf.Modified
	if mt.IsZero() {
		if fi, err := os.Stat(final); err == nil {
			mt = fi.ModTime()
		}
	}
	dl.st.Remember(&LocalFile{Rel: rf.Rel, Size: rf.Size, MtimeNs: mt.UnixNano()}, sum)
	return nil
}

// fileWriter marks write failures as local problems (disk full, etc.).
type fileWriter struct{ f *os.File }

func (w fileWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if err != nil {
		err = &gdrive.LocalError{Err: err}
	}
	return n, err
}

type countingWriter struct {
	w     io.Writer
	n     *int64
	after func()
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	c.after()
	return n, err
}

// contentRangeStart parses "bytes 100-199/200" and returns 100.
func contentRangeStart(v string) (int64, bool) {
	v, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, false
	}
	start, _, ok := strings.Cut(v, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(start, 10, 64)
	return n, err == nil
}

// startReporter shows the live progress line until the returned func is called.
func startReporter(ui *term.Console, prog *Progress) (stop func()) {
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		lastPlain := time.Now()
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
				if ui.IsTTY() {
					ui.Status(prog.Line())
				} else if time.Since(lastPlain) >= time.Minute {
					ui.Println(prog.Line())
					lastPlain = time.Now()
				}
			}
		}
	}()
	return func() {
		close(quit)
		<-done
		ui.ClearStatus()
	}
}
