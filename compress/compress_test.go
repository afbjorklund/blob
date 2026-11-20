package compress

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

// gzipOf returns a gzip-compressed copy of in.
func gzipOf(t *testing.T, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(in); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestDecompressGzip(t *testing.T) {
	original := []byte("hello, this is a layer tar that is compressed for the test")
	comp := gzipOf(t, original)
	// Sanity: the compressed form really does differ from the original bytes.
	if bytes.Equal(original, comp) {
		t.Fatalf("gzip is a no-op on this input; cannot test decompression")
	}

	var out bytes.Buffer
	n, err := Decompress(bytes.NewReader(comp), &out)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if n != int64(len(original)) {
		t.Fatalf("Decompress count = %d, want %d (original's length)", n, len(original))
	}
	if !bytes.Equal(out.Bytes(), original) {
		t.Fatalf("Decompress content:\ngot  %q\nwant %q", out.Bytes(), original)
	}
}

func TestDecompressPlain(t *testing.T) {
	in := []byte("plain bytes that are not gzip")
	var out bytes.Buffer
	n, err := Decompress(bytes.NewReader(in), &out)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if n != int64(len(in)) {
		t.Fatalf("Decompress count = %d, want %d", n, len(in))
	}
	if !bytes.Equal(out.Bytes(), in) {
		t.Fatalf("Decompress content: %q != %q", out.Bytes(), in)
	}
}

// TestDecompressOneByte: a 1-byte input is not a gzip stream (needs 2 bytes of
// magic) and must pass through intact. The peek-and-replay path must not drop
// the single byte.
func TestDecompressOneByte(t *testing.T) {
	in := []byte("a")
	var out bytes.Buffer
	n, err := Decompress(bytes.NewReader(in), &out)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if n != 1 {
		t.Fatalf("Decompress count = %d, want 1", n)
	}
	if !bytes.Equal(out.Bytes(), in) {
		t.Fatalf("Decompress dropped the byte: %q", out.Bytes())
	}
}

// TestDecompressEmpty: an empty reader must result in zero bytes produced and
// no error.
func TestDecompressEmpty(t *testing.T) {
	var out bytes.Buffer
	n, err := Decompress(bytes.NewReader(nil), &out)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if n != 0 {
		t.Fatalf("Decompress count = %d, want 0", n)
	}
	if out.Len() != 0 {
		t.Fatalf("Decompress produced %d bytes, want 0", out.Len())
	}
}

// TestDecompressLarge: a payload larger than any internal buffer round-trips
// through Decompress. Catches off-by-one in the streaming copy (the peek
// replay, the gzip boundary, io.Copy).
func TestDecompressLarge(t *testing.T) {
	original := make([]byte, 4<<20+37) // ~4 MiB + an oddity
	for i := range original {
		original[i] = byte(i%251 + 'a')
	}
	comp := gzipOf(t, original)

	var out bytes.Buffer
	n, err := Decompress(bytes.NewReader(comp), &out)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if n != int64(len(original)) {
		t.Fatalf("Decompress count = %d, want %d", n, len(original))
	}
	if !bytes.Equal(out.Bytes(), original) {
		t.Fatalf("Decompress: content differs (%d bytes out)", out.Len())
	}
}

// TestDecompressGzipTruncationCorrupt: a gzip stream that claims a body it
// does not have must surface an error (not silently produce partial output)
// so a caller cannot silently import a broken layer.
func TestDecompressGzipTruncationCorrupt(t *testing.T) {
	// A gzip with the full header and a body that is cut short — the gzip
	// reader will report a checksum (or EOF) error when asked to close.
	original := []byte("x")
	comp := gzipOf(t, original)
	// Cut off at least one byte so the trailer (which carries the checksum) is
	// gone.
	comp = comp[:len(comp)-2]
	var out bytes.Buffer
	_, err := Decompress(bytes.NewReader(comp), &out)
	if err == nil {
		t.Fatalf("Decompress(corrupt gzip) = nil, want an error")
	}
}

// TestDecompressNoPanicOnNonSeekable: a single-pass reader (e.g. the network
// source a layer could come from) must not be re-read. Decompress only
// consumes r once. We exercise this with a one-shot reader (strings.Reader
// is one-shot, not seekable) and confirm the same content round-trips.
func TestDecompressNoPanicOnNonSeekable(t *testing.T) {
	in := []byte("one-shot reader: plain content that is not gzip")
	var out bytes.Buffer
	r := strings.NewReader(string(in)) // io.Reader, not io.ReadSeeker
	if _, err := Decompress(r, &out); err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(out.Bytes(), in) {
		t.Fatalf("Decompress: %q != %q", out.Bytes(), in)
	}
}
