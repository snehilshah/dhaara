// Package engine moves folders between this computer, Google Drive and a
// phone: upload, download, two-way sync, pull, phone sync and snapshots.
package engine

import (
	"crypto/x509"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/platform"
)

// Config holds everything one run needs. Tests point Endpoints at a fake server.
type Config struct {
	Download     bool   // download instead of upload
	Source       string // local folder: what to upload, or where to download to
	RemoteName   string // folder name in My Drive
	Workers      int
	ChunkSize    int64 // resumable chunk size; files up to this size go in one request
	VerifyOnly   bool
	DryRun       bool
	Overwrite    bool // download: replace local files that differ from Drive
	NoInhibit    bool
	ClientSecret string
	ConfigDir    string
	StateDir     string
	Endpoints    gdrive.Endpoints
	Retry        gdrive.RetryPolicy
	PaceInterval time.Duration  // minimum gap between API requests
	TLSRoots     *x509.CertPool // tests only; nil = system certificates

	Mode              string   // upload, download, sync, pull, snapshot, phone
	Pull              bool     // download that also follows removals and moves (dhaara pull)
	Exclude           []string // paths left alone everywhere (see tree.go)
	AllowManyRemovals bool
	Trash             func(path string) error // moves a local file to the trash
	PhoneDir          string                  // phone: the folder on the phone
	ADB               string                  // phone: the adb program
}

// freeSpace is a variable so tests can pretend the disk is full.
var freeSpace = platform.FreeSpace
