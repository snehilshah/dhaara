package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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

// listTree walks the Drive folder. Items are listed oldest first, so when two
// share a name the oldest wins and the other is reported.
func listTree(ctx context.Context, d *gdrive.Drive, rootID string) (*remoteTree, error) {
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
