package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
)

// The removed list records every file that sync took out of the Drive folder
// (deleted, replaced by a new version, or moved), by path and MD5. Devices
// that only take changes (pull, phone) use it to tell "removed on purpose"
// from "only ever existed here": a local file is removed only if its path
// and MD5 are both on the list. Unlike Drive's trash it never empties itself.
//
// It lives outside the synced folder, in "dhaara data/<folder> (<folder's
// Drive ID>) removed.json" in My Drive, so it is never downloaded or copied
// with the files. The ID keeps apart folders that share a name.
const dataFolderName = "dhaara data"

type removedEntry struct {
	Path string    `json:"path"`
	MD5  string    `json:"md5"`
	Size int64     `json:"size"`
	When time.Time `json:"when"`
	Why  string    `json:"why"` // deleted, replaced, moved
}

type removedList struct {
	d        *gdrive.Drive
	name     string // "" when the synced folder doesn't exist yet
	folderID string // "dhaara data"
	fileID   string
	entries  []removedEntry
	changed  bool
}

func removedFileName(remotePath, targetID string) string {
	return fmt.Sprintf("%s (%s) removed.json", path.Base(strings.Trim(remotePath, "/")), targetID)
}

// loadRemoved reads the list for the Drive folder targetID. A missing list
// (or folder, targetID "") is just empty.
func loadRemoved(ctx context.Context, d *gdrive.Drive, remotePath, targetID string) (*removedList, error) {
	r := &removedList{d: d}
	if targetID == "" {
		return r, nil
	}
	r.name = removedFileName(remotePath, targetID)
	var err error
	if r.folderID, err = gdrive.EnsureFolder(ctx, d, "root", dataFolderName, false); err != nil || r.folderID == "" {
		return r, err
	}
	kids, err := d.ListChildren(ctx, r.folderID)
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		if !k.IsFolder() && k.Name == r.name {
			r.fileID = k.ID
			break
		}
	}
	if r.fileID == "" {
		return r, nil
	}
	raw, err := d.ReadSmall(ctx, r.fileID)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Removed []removedEntry `json:"removed"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, gdrive.Fatalf("cannot read %s/%s on Drive (%v). Fix or remove it, then run again", dataFolderName, r.name, err)
	}
	r.entries = doc.Removed
	return r, nil
}

func (r *removedList) Has(path, md5 string) bool {
	for _, e := range r.entries {
		if e.Path == path && e.MD5 == md5 {
			return true
		}
	}
	return false
}

func (r *removedList) HasPath(path string) bool {
	for _, e := range r.entries {
		if e.Path == path {
			return true
		}
	}
	return false
}

func (r *removedList) Add(path, md5 string, size int64, why string) {
	if md5 == "" || r.Has(path, md5) {
		return
	}
	r.entries = append(r.entries, removedEntry{Path: path, MD5: md5, Size: size, When: time.Now().UTC().Truncate(time.Second), Why: why})
	r.changed = true
}

// Forget drops an entry because that exact file is back in the Drive folder.
func (r *removedList) Forget(path, md5 string) {
	out := r.entries[:0]
	for _, e := range r.entries {
		if e.Path == path && e.MD5 == md5 {
			r.changed = true
			continue
		}
		out = append(out, e)
	}
	r.entries = out
}

func (r *removedList) Len() int { return len(r.entries) }

// Save writes the list to Drive if it changed. It must succeed before the
// files it describes are removed, so other devices always learn of a removal.
func (r *removedList) Save(ctx context.Context) error {
	if !r.changed {
		return nil
	}
	if r.name == "" {
		return fmt.Errorf("internal error: removed list for a folder that doesn't exist")
	}
	if r.folderID == "" {
		id, err := gdrive.EnsureFolder(ctx, r.d, "root", dataFolderName, true)
		if err != nil {
			return fmt.Errorf("cannot create %q on Drive: %w", dataFolderName, err)
		}
		r.folderID = id
	}
	sort.SliceStable(r.entries, func(i, j int) bool { return r.entries[i].Path < r.entries[j].Path })
	data, err := json.MarshalIndent(struct {
		Note    string         `json:"note"`
		Removed []removedEntry `json:"removed"`
	}{"Written by dhaara sync. Devices remove a file only if its path and md5 are listed here.", r.entries}, "", " ")
	if err != nil {
		return err
	}
	id, err := r.d.WriteSmall(ctx, r.fileID, r.name, r.folderID, data)
	if err != nil {
		return fmt.Errorf("cannot save the removed list on Drive: %w", err)
	}
	r.fileID, r.changed = id, false
	return nil
}
