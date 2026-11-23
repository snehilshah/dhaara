package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/snehilshah/dhaara/internal/exclude"
	"github.com/snehilshah/dhaara/internal/gdrive"
)

// remoteTree is a whole Drive folder, walked once.
type remoteTree struct {
	rootID  string
	dirs    []string          // relative folder paths, parents before children
	dirIDs  map[string]string // relative folder path -> Drive ID ("" is the top folder)
	files   []remoteFile
	byRel   map[string]remoteFile
	dups    []remoteFile // later files whose name was already taken in their folder
	skipped []string     // reasons, for the user
}

// listWorkers is how many folder listings are read from Drive at once. The
// client's pacing still caps the request rate.
const listWorkers = 8

// listTree walks the Drive folder, one level at a time, reading the folders
// of a level in parallel. Items are listed oldest first, so when two share a
// name the oldest wins and the other is reported. The result is the same as
// reading the folders one by one, in order.
func listTree(ctx context.Context, fs gdrive.Folders, rootID string) (*remoteTree, error) {
	t := &remoteTree{rootID: rootID, dirIDs: map[string]string{"": rootID}, byRel: map[string]remoteFile{}}
	type item struct{ id, rel string }
	level := []item{{rootID, ""}}
	seen := map[string]bool{}
	for len(level) > 0 {
		ids := make([]string, len(level))
		for i, it := range level {
			ids[i] = it.id
		}
		listed, err := listAll(ctx, fs, ids)
		if err != nil {
			return nil, err
		}
		var next []item
		for i, cur := range level {
			for _, k := range listed[i] {
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
					next = append(next, item{k.ID, rel})
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
		level = next
	}
	return t, nil
}

// listAll reads several folders at once; out[i] holds the items of ids[i].
// The first error stops the rest.
func listAll(ctx context.Context, fs gdrive.Folders, ids []string) ([][]gdrive.File, error) {
	out := make([][]gdrive.File, len(ids))
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(listWorkers, len(ids)) {
		wg.Go(func() {
			for i := range jobs {
				kids, err := fs.ListChildren(ctx, ids[i])
				if err != nil {
					cancel(err)
					continue
				}
				out[i] = kids
			}
		})
	}
feed:
	for i := range ids {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if err := context.Cause(ctx); err != nil && ctx.Err() != nil {
		return nil, err
	}
	return out, nil
}

// excludeItems drops the items whose path is exclude.Match.
func excludeItems[T any](items []T, rel func(T) string, patterns []string) (kept []T, dropped []string) {
	if len(patterns) == 0 {
		return items, nil
	}
	for _, it := range items {
		if exclude.Match(rel(it), patterns) {
			dropped = append(dropped, rel(it))
			continue
		}
		kept = append(kept, it)
	}
	return kept, dropped
}

// parentDirs returns every folder above rel, deepest first ("a/b/c.mp3" -> "a/b", "a").
func parentDirs(rel string) []string {
	var out []string
	for d := filepath.Dir(rel); d != "." && d != "/" && d != ""; d = filepath.Dir(d) {
		out = append(out, d)
	}
	return out
}
