package gdrive

import (
	"context"
	"slices"
	"sync"
)

// Folders is what folder lookups need: list a folder, make one. *Drive
// does both straight against Drive; *Listings remembers what it has read.
type Folders interface {
	ListChildren(ctx context.Context, parentID string) ([]File, error)
	CreateFolder(ctx context.Context, name, parentID string) (string, error)
}

// Listings remembers folder listings for one run, so each folder is read
// from Drive at most once however many lookups need it. Folders it creates
// are added to what it remembers. It does not see other changes (uploads,
// moves, trash): checks made after changing Drive use a new Listings.
// Safe for concurrent use.
type Listings struct {
	d  *Drive
	mu sync.Mutex
	m  map[string][]File
}

func (d *Drive) NewListings() *Listings {
	return &Listings{d: d, m: map[string][]File{}}
}

// ListChildren returns the non-trashed items in a folder, oldest first.
func (l *Listings) ListChildren(ctx context.Context, parentID string) ([]File, error) {
	l.mu.Lock()
	kids, ok := l.m[parentID]
	l.mu.Unlock()
	if ok {
		return slices.Clone(kids), nil
	}
	kids, err := l.d.ListChildren(ctx, parentID)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if _, ok := l.m[parentID]; !ok {
		l.m[parentID] = kids
	}
	kids = slices.Clone(l.m[parentID])
	l.mu.Unlock()
	return kids, nil
}

// CreateFolder makes a folder and adds it to the parent's remembered
// listing, as its newest item.
func (l *Listings) CreateFolder(ctx context.Context, name, parentID string) (string, error) {
	id, err := l.d.CreateFolder(ctx, name, parentID)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	if kids, ok := l.m[parentID]; ok {
		l.m[parentID] = append(kids, File{ID: id, Name: name, MimeType: FolderMime})
	}
	l.m[id] = nil
	l.mu.Unlock()
	return id, nil
}

// Forget drops a remembered listing, so the next lookup reads it again.
func (l *Listings) Forget(parentID string) {
	l.mu.Lock()
	delete(l.m, parentID)
	l.mu.Unlock()
}
