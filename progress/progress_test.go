package progress

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// gateReader returns its data in (one or more) Reads, optionally blocking its
// first Read for `gate` so it outlasts the arming threshold.
type gateReader struct {
	data  []byte
	gate  time.Duration
	armed bool
	pos   int
}

func (g *gateReader) Read(p []byte) (int, error) {
	if !g.armed {
		g.armed = true
		if g.gate > 0 {
			time.Sleep(g.gate)
		}
	}
	if g.pos >= len(g.data) {
		return 0, io.EOF
	}
	n := copy(p, g.data[g.pos:])
	g.pos += n
	return n, nil
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	return b
}

// The wrapper must be a transparent pass-through of every byte, always.
func TestPassThroughIntegrity(t *testing.T) {
	data := []byte("0123456789abcdef")
	p := NewReader(bytes.NewReader(data), int64(len(data)))
	if got := readAll(t, p); !bytes.Equal(got, data) {
		t.Fatalf("data mismatch:\n got %q\nwant %q", got, data)
	}

	// Finish is idempotent and safe to call any number of times.
	p.Finish()
	p.Finish()
}

// A transfer that outlasts the threshold arms the bar (forced- enabled, since
// the test process has no TTY), finishing at its full total.
func TestArmsWhenSlow(t *testing.T) {
	data := []byte("x")
	p := NewReader(&gateReader{data: data, gate: 600 * time.Millisecond}, 1)
	p.enabled = true

	readAll(t, p)

	if p.bar == nil {
		t.Fatal("expected the bar to arm once the transfer outlasted the threshold")
	}
	if got := p.bar.Current(); got != int64(len(data)) {
		t.Fatalf("bar not at full total: got %d want %d", got, len(data))
	}
}

// A transfer that finishes well inside the threshold shows no bar at all.
func TestNoBarWhenFast(t *testing.T) {
	data := []byte("x")
	p := NewReader(&gateReader{data: data}, 1) // no gate -> instant
	p.enabled = true

	readAll(t, p)

	if p.bar != nil {
		t.Fatal("did not expect a bar for a fast transfer")
	}
}
