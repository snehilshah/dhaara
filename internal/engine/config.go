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
	Trash             func(path string) error  // moves a local file to the trash
	PhoneDir          string                   // phone: the folder on the phone
	PhoneSerial       string                   // phone: which phone ("" when only one is connected)
	ADB               string                   // phone: the adb program
	Yes               bool                     // apply: don't ask
	Confirm           func(prompt string) bool // apply: ask the user; nil when there's no terminal

	applying *savedPlan       // set by Apply: carry out this plan instead of saving one
	predict  *drivePrediction // set to receive what the sync plan leaves on Drive (check and apply)

	// Set by Status, which runs the stages of a pairing one after another:
	collect    *savedPlan       // receive the check's plan instead of saving it
	basedOn    *drivePrediction // phone check: plan against this Drive instead of reading it
	readyAfter map[string]bool  // phone check: paths the earlier stage brings in step with Drive here
}

// freeSpace is a variable so tests can pretend the disk is full.
var freeSpace = platform.FreeSpace
