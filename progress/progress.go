// Package progress offers io.Reader wrappers that, for a transfer of a known
// size, show a progress bar on a TTY — but only once the transfer has been
// in flight long enough to warrant one (see threshold). A transfer that
// finishes inside that window (a fast local copy) prints nothing; one that
// outlasts it (a network store, or a dir on a slow mount) shows a live
// size/time/speed bar and a final summary line. The reader is otherwise a
// transparent pass-through, so it is always safe to wrap.
//
// The underlying bar comes from github.com/cheggaaa/pb/v3; this package is the
// only place that library is imported, so a future swap of progress-bar
// implementation stays contained here.
package progress

import (
	"io"
	"os"
	"sync"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/mattn/go-isatty"
)

// threshold is how long a transfer must have been running before a progress
// bar is shown at all. It is what keeps fast local transfers silent while
// still surfacing slow ones, local or remote, without having to classify the
// backend. Tune here.
const threshold = 500 * time.Millisecond

// Reader is an io.Reader that tracks a bounded transfer (of size bytes) and,
// if it outlasts threshold and stdout is a TTY, renders a progress bar. Call
// Finish once the transfer is over: it finalises the bar and is safe to call
// more than once (and a no-op when no bar was ever shown).
type Reader struct {
	r       io.Reader
	size    int64
	enabled bool

	mu    sync.Mutex
	count int64
	done  bool
	start time.Time
	bar   *pb.ProgressBar
}

// NewReader wraps r for a transfer of size bytes. It is always safe to use in
// place of r; a bar only appears if the transfer runs longer than threshold.
func NewReader(r io.Reader, size int64) *Reader {
	return &Reader{
		r:       r,
		size:    size,
		enabled: stdOutIsTTY(),
		start:   time.Now(),
	}
}

// Read reads from the underlying reader as it goes, feeding the bar once it
// has been armed by the threshold lapse.
func (p *Reader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)

	if p.enabled {
		p.mu.Lock()
		p.count += int64(n)
		if p.bar == nil && !p.done && time.Since(p.start) >= threshold {
			// Armed: start from where we already are (all but this chunk),
			// so the trailing Add(n) below lands the count exactly. Bytes
			// switches the counters to humanized units (MiB, MiB/s) the
			// way pb.NewProxyReader does — without it they read raw bytes.
			p.bar = pb.New64(p.size).Set(pb.Bytes, true).Start()
			p.bar.Add64(p.count - int64(n))
		}
		bar := p.bar
		p.mu.Unlock()
		if bar != nil {
			bar.Add(n)
		}
	}

	if err != nil {
		p.Finish()
	}
	return n, err
}

// Finish marks the transfer done: if a bar is active it writes the final frame
// and stops its refresh goroutine. Idempotent; a no-op when no bar was shown.
func (p *Reader) Finish() {
	p.mu.Lock()
	p.done = true
	bar := p.bar
	p.mu.Unlock()
	if bar != nil {
		bar.Finish()
	}
}

func stdOutIsTTY() bool {
	fd := os.Stdout.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}
