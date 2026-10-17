package oci

import (
	"archive/tar"
	"compress/gzip"
	"io"
)

// Member is one entry in a layer tar (plain or gzip-compressed).
type Member struct {
	Name     string
	Size     int64
	Typeflag byte
}

// ListMembers streams a layer's tar (plain or gzip) and returns its member
// entries. The layer is read entirely (it must fit in a single tar), but a
// member's *content* is not returned — only its name/size/type. For very large
// layers prefer a streaming consumer over collecting the whole slice (this is
// a convenience for `oci ls`, where the layer is expected to be small enough
// to enumerate).
func ListMembers(r io.Reader, compressed bool) ([]Member, error) {
	rc, err := openLayer(r, compressed)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	var members []Member
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			members = append(members, Member{Name: hdr.Name, Size: hdr.Size, Typeflag: hdr.Typeflag})
		}
	}
	return members, nil
}

// ReadMember returns a reader for the member named name (the tar entry's Name,
// e.g. "bin/busybox"), or nil if no such entry exists. Only regular files are
// returned. The caller must Close the reader (which releases the underlying
// tar/gzip stream).
func ReadMember(r io.Reader, compressed bool, name string) (io.ReadCloser, int64, error) {
	rc, err := openLayer(r, compressed)
	if err != nil {
		return nil, 0, err
	}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			_ = rc.Close()
			return nil, 0, nil
		}
		if err != nil {
			_ = rc.Close()
			return nil, 0, err
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Name == name {
			return &memberReader{rc: rc, lim: io.LimitReader(tr, hdr.Size)}, hdr.Size, nil
		}
	}
}

// memberReader reads exactly one tar member (bounded by its size) and keeps the
// underlying stream open for the caller to Close.
type memberReader struct {
	rc  io.ReadCloser
	lim io.Reader
}

func (m *memberReader) Read(p []byte) (int, error) {
	return m.lim.Read(p)
}

func (m *memberReader) Close() error { return m.rc.Close() }

// openLayer wraps r in a gzip reader when compressed is true, else returns r.
func openLayer(r io.Reader, compressed bool) (io.ReadCloser, error) {
	if !compressed {
		return nopCloser{r}, nil
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	return gz, nil
}

type nopCloser struct{ r io.Reader }

func (n nopCloser) Read(p []byte) (int, error) { return n.r.Read(p) }
func (n nopCloser) Close() error               { return nil }
