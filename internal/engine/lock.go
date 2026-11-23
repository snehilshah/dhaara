package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
	"github.com/snehilshah/dhaara/internal/platform"
)

// lockFolder makes sure only one dhaara run changes a Drive folder at a
// time: apply, upload, download and snapshot take the folder's lock first.
// Checks don't, since they change nothing. The lock goes when release is
// called or the process ends.
func lockFolder(cfg Config, remote, what string) (release func(), err error) {
	remote = strings.Trim(remote, "/")
	dir := filepath.Join(cfg.StateDir, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, gdrive.Fatalf("cannot create state folder: %v", err)
	}
	k := sha256.Sum256([]byte(remote))
	info := fmt.Sprintf("%s of %q, process %d, started %s", what, remote, os.Getpid(), time.Now().Format("2006-01-02 15:04"))
	release, holder, err := platform.Lock(filepath.Join(dir, hex.EncodeToString(k[:8])+".lock"), info)
	if errors.Is(err, platform.ErrLocked) {
		if holder == "" {
			holder = "another dhaara run"
		}
		return nil, gdrive.Fatalf("another dhaara run is changing the Drive folder %q (%s). Nothing was changed; run again once it has finished", remote, holder)
	}
	if err != nil {
		return nil, gdrive.Fatalf("cannot lock the Drive folder %q: %v", remote, err)
	}
	return release, nil
}
