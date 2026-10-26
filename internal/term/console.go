// Package term prints messages above a live one-line progress display.
package term

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Console prints messages above a live one-line progress display.
type Console struct {
	mu     sync.Mutex
	w      io.Writer
	file   *os.File
	tty    bool
	status string
}

func NewConsole(f *os.File) *Console {
	tty := false
	if fi, err := f.Stat(); err == nil {
		tty = fi.Mode()&os.ModeCharDevice != 0
	}
	return &Console{w: f, file: f, tty: tty}
}

// NewPlain writes plain lines to w, with no progress line (for tests and logs).
func NewPlain(w io.Writer) *Console { return &Console{w: w} }

// IsTTY reports whether a live progress line is shown.
func (c *Console) IsTTY() bool { return c.tty }

func (c *Console) Printf(format string, a ...any) { c.Println(fmt.Sprintf(format, a...)) }

func (c *Console) Println(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tty && c.status != "" {
		io.WriteString(c.w, "\r\033[K")
	}
	io.WriteString(c.w, msg+"\n")
	if c.tty && c.status != "" {
		io.WriteString(c.w, c.status)
	}
}

func (c *Console) Status(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tty {
		return
	}
	if w := termWidth(c.file); len([]rune(msg)) > w-1 {
		msg = string([]rune(msg)[:w-1])
	}
	c.status = msg
	io.WriteString(c.w, "\r\033[K"+msg)
}

func (c *Console) ClearStatus() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tty && c.status != "" {
		io.WriteString(c.w, "\r\033[K")
	}
	c.status = ""
}

func Size(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, unit := range []string{"KB", "MB", "GB", "TB"} {
		v /= 1000
		if v < 1000 || unit == "TB" {
			return fmt.Sprintf("%.1f %s", v, unit)
		}
	}
	return ""
}

func Duration(d time.Duration) string {
	s := int(d.Seconds())
	if h := s / 3600; h > 0 {
		return fmt.Sprintf("%dh%02dm", h, s%3600/60)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}
