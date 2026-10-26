package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

// Trash moves a local file to the desktop trash (Files → Trash), so a
// removal can be undone. On the same disk this is a rename: no data is written.
func Trash(path string) error {
	gio, err := exec.LookPath("gio")
	if err != nil {
		return fmt.Errorf("cannot move to the trash: the gio command is missing (install libglib2.0-bin)")
	}
	out, err := exec.Command(gio, "trash", "--", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gio trash %s: %v: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
