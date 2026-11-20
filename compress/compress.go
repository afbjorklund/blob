// Package compress reads a stream that may or may not be compressed and
// writes the decompressed (or passthrough) bytes to a destination, returning
// the total byte count of the result — a streaming `zcat f | wc -c` for
// compressed input and `wc -c` for plain input.
//
// The surface is a single entry point (Decompress) deliberately: the caller
// does not know or care which codec the stream carries — it is detected from
// the leading bytes on the way through. Today the only recognised codec is
// gzip (the media type for gzipped OCI layers is
// "application/vnd.oci.image.layer.v1.tar+gzip"); adding another codec later
// (zstd, "application/vnd.oci.image.layer.v1.tar+zstd") is a local change
// inside Decompress, the call sites do not move.
//
// The package is pure Go, no external deps, and works over io streams so a
// layer of any size (MB, GB, TB) is processed with O(1) memory — the
// decompressed blob is streamed directly into dst and never held in RAM.
package compress

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// gzipMagic are the leading two bytes of every gzip stream (RFC 1952). Two
// bytes are enough to disambiguate gzip from anything else; the rest of the
// 10-byte header carries optional fields (OS, mtime, filename) that the
// decompressor parses and the caller does not need.
var gzipMagic = []byte{0x1f, 0x8b}

// Decompress streams r into dst, decoding through the gzip reader if r's
// leading bytes carry the gzip magic; otherwise it copies the bytes
// verbatim. It returns the total bytes written to dst.
//
// The peek of the first two bytes (or 1, if that is all the reader has) is
// replayed into dst when the stream is plain, so no leading bytes are lost
// even on a 1-byte reader. The peek is replayed into the gzip reader (via
// io.MultiReader) when the stream is gzip, so the codec sees the full
// stream from its start. O(1) memory — the stream is decoded incrementally
// into dst, not buffered whole.
func Decompress(r io.Reader, dst io.Writer) (int64, error) {
	head := make([]byte, len(gzipMagic))
	n, _ := io.ReadFull(r, head)
	if n == len(gzipMagic) && bytes.Equal(head, gzipMagic) {
		gz, err := gzip.NewReader(io.MultiReader(bytes.NewReader(head), r))
		if err != nil {
			return 0, fmt.Errorf("compress: gzip stream: %w", err)
		}
		total, err := io.Copy(dst, gz)
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return 0, err
		}
		return total, nil
	}
	if n > 0 {
		if _, err := dst.Write(head[:n]); err != nil {
			return 0, err
		}
	}
	copied, err := io.Copy(dst, r)
	return int64(n) + copied, err
}
