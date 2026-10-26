package engine

import (
	"fmt"
	"sync"
	"time"

	"github.com/snehilshah/dhaara/internal/term"
)

// Progress tracks bytes confirmed by Google and the current speed.
type Progress struct {
	mu         sync.Mutex
	totalBytes int64
	totalFiles int
	offsets    map[string]int64
	done       int64
	uploaded   int
	failed     int
	sent       func() int64
	samples    []sample
}

type sample struct {
	at   time.Time
	sent int64
}

func newProgress(files []*LocalFile, sent func() int64) *Progress {
	p := &Progress{totalFiles: len(files), offsets: map[string]int64{}, sent: sent}
	for _, f := range files {
		p.totalBytes += f.Size
	}
	return p
}

// Set records how many bytes of f Google has confirmed.
func (p *Progress) Set(f *LocalFile, offset int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += offset - p.offsets[f.Rel]
	p.offsets[f.Rel] = offset
}

func (p *Progress) Finish(f *LocalFile, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.offsets[f.Rel]
	delete(p.offsets, f.Rel)
	if ok {
		p.uploaded++
		p.done += f.Size - prev
	} else {
		p.failed++
		p.done -= prev
		p.totalBytes -= f.Size
	}
}

func (p *Progress) Counts() (uploaded, failed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.uploaded, p.failed
}

func (p *Progress) Line() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now, sent := time.Now(), p.sent()
	p.samples = append(p.samples, sample{now, sent})
	for len(p.samples) > 2 && now.Sub(p.samples[0].at) > 30*time.Second {
		p.samples = p.samples[1:]
	}
	first := p.samples[0]
	speed := 0.0
	if secs := now.Sub(first.at).Seconds(); secs > 0 {
		speed = float64(sent-first.sent) / secs
	}
	eta := "--"
	if speed > 0 {
		eta = term.Duration(time.Duration(float64(p.totalBytes-p.done) / speed * float64(time.Second)))
	}
	pct := 100.0
	if p.totalBytes > 0 {
		pct = 100 * float64(p.done) / float64(p.totalBytes)
	}
	line := fmt.Sprintf("%5.1f%%  %s / %s  %s/s (%.0f Mbit/s)  ETA %s  files %d/%d",
		pct, term.Size(p.done), term.Size(p.totalBytes), term.Size(int64(speed)), speed*8/1e6,
		eta, p.uploaded, p.totalFiles)
	if p.failed > 0 {
		line += fmt.Sprintf("  failed %d", p.failed)
	}
	return line
}
