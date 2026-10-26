package platform

import (
	"log/slog"
	"os/exec"
	"time"
)

// InhibitSleep keeps the laptop from sleeping while uploading, using
// systemd-inhibit. The lock is held by a child "cat" process and ends when its
// input closes, so it is released even if dhaara is killed.
func InhibitSleep() (release func()) {
	noop := func() {}
	path, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		return noop
	}
	cmd := exec.Command(path, "--what=sleep:idle", "--who=dhaara",
		"--why=Uploading files to Google Drive", "--mode=block", "cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return noop
	}
	if err := cmd.Start(); err != nil {
		slog.Warn("cannot keep the laptop awake", "err", err)
		return noop
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	select {
	case <-exited:
		slog.Warn("cannot keep the laptop awake (systemd-inhibit exited)")
		return noop
	case <-time.After(300 * time.Millisecond):
	}
	return func() {
		stdin.Close()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	}
}
