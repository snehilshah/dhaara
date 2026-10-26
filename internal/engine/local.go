package engine

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/platform"
)

// LocalFile is one file in the folder being uploaded.
type LocalFile struct {
	Rel     string // path relative to the upload folder
	Path    string // absolute path
	RelDir  string // folder part of Rel ("" for the top level)
	Name    string
	Mime    string
	Size    int64
	MtimeNs int64

	ParentID   string   // Drive folder it goes into
	ReplaceIDs []string // older, different copies on Drive to trash after upload
}

func (f *LocalFile) ModTime() time.Time { return time.Unix(0, f.MtimeNs) }

// scan lists the folder. Directories come before their contents, in name order.
func scan(root string) (dirs []string, files []*LocalFile, skipped []string, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel != "." {
				dirs = append(dirs, rel)
			}
			return nil
		}
		if !d.Type().IsRegular() { // symlinks, sockets, devices
			skipped = append(skipped, path)
			return nil
		}
		if strings.HasPrefix(d.Name(), partPrefix) && strings.HasSuffix(d.Name(), partSuffix) {
			skipped = append(skipped, path+" (unfinished dhaara download)")
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		relDir := filepath.Dir(rel)
		if relDir == "." {
			relDir = ""
		}
		files = append(files, &LocalFile{
			Rel: rel, Path: path, RelDir: relDir, Name: d.Name(),
			Mime: mimeType(d.Name()), Size: info.Size(), MtimeNs: info.ModTime().UnixNano(),
		})
		return nil
	})
	return dirs, files, skipped, err
}

func mimeType(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		if base, _, err := mime.ParseMediaType(t); err == nil {
			return base
		}
	}
	return "application/octet-stream"
}

// session is a saved resumable upload, so a re-run can continue mid-file.
type session struct {
	URI     string    `json:"uri"`
	Size    int64     `json:"size"`
	MtimeNs int64     `json:"mtime_ns"`
	Parent  string    `json:"parent"`
	Created time.Time `json:"created"`
}

type md5Entry struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	MD5     string `json:"md5"`
}

// State is what dhaara remembers between runs for one folder: open upload
// sessions, and local MD5s (so re-runs don't re-read every file).
type State struct {
	mu       sync.Mutex
	dir      string
	sessions map[string]session
	md5s     map[string]md5Entry
	unsaved  int
}

func openState(dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &State{dir: dir, sessions: map[string]session{}, md5s: map[string]md5Entry{}}
	loadJSON(filepath.Join(dir, "sessions.json"), &s.sessions)
	loadJSON(filepath.Join(dir, "md5.json"), &s.md5s)
	return s, nil
}

func loadJSON(path string, v any) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		err = json.Unmarshal(raw, v)
	}
	if err != nil {
		slog.Warn("ignoring unreadable state file", "path", path, "err", err)
	}
}

func (s *State) Session(rel string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[rel]
	return v, ok
}

func (s *State) SetSession(rel string, v session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[rel] = v
	return platform.WritePrivateJSON(filepath.Join(s.dir, "sessions.json"), s.sessions)
}

func (s *State) ClearSession(rel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[rel]; !ok {
		return nil
	}
	delete(s.sessions, rel)
	return platform.WritePrivateJSON(filepath.Join(s.dir, "sessions.json"), s.sessions)
}

// OpenSessions is the number of half-finished uploads saved for a re-run.
func (s *State) OpenSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// MD5 returns the file's MD5, from the cache when size and time are unchanged.
func (s *State) MD5(f *LocalFile) (string, error) {
	s.mu.Lock()
	e, ok := s.md5s[f.Rel]
	s.mu.Unlock()
	if ok && e.Size == f.Size && e.MtimeNs == f.MtimeNs {
		return e.MD5, nil
	}
	fh, err := os.Open(f.Path)
	if err != nil {
		return "", &gdrive.LocalError{Err: err}
	}
	defer fh.Close()
	h := md5.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", &gdrive.LocalError{Err: err}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.md5s[f.Rel] = md5Entry{f.Size, f.MtimeNs, sum}
	if s.unsaved++; s.unsaved >= 100 {
		s.flushLocked()
	}
	return sum, nil
}

// Remember stores a checksum we already know (e.g. computed while downloading).
func (s *State) Remember(f *LocalFile, sum string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.md5s[f.Rel] = md5Entry{f.Size, f.MtimeNs, sum}
	if s.unsaved++; s.unsaved >= 100 {
		s.flushLocked()
	}
}

func (s *State) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsaved > 0 {
		s.flushLocked()
	}
}

func (s *State) flushLocked() {
	if err := platform.WritePrivateJSON(filepath.Join(s.dir, "md5.json"), s.md5s); err != nil {
		slog.Warn("cannot save checksum cache", "err", err)
		return
	}
	s.unsaved = 0
}
