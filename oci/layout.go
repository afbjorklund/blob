package oci

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

// DirLayout is a filesystem-backed OCI layout:
//
//	dir/
//	├── oci-layout
//	├── index.json
//	└── blobs/sha256/<hex>   (one file per blob)
//
// It implements both Source (read layout) and Sink (write layout).

// NewDir returns a DirLayout rooted at dir (dir is created lazily on the first
// write or on New()).
func NewDir(dir string) *DirLayout { return &DirLayout{root: dir} }

type DirLayout struct{ root string }

// New creates the layout skeleton (blobs/sha256/ + oci-layout).
// index.json is left to PutIndex.
func (l *DirLayout) New() error {
	if err := os.MkdirAll(filepath.Join(l.root, "blobs", "sha256"), 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(l.root, "oci-layout"), []byte(layoutDoc), 0644)
}

// Has reports whether the blob named by d is present.
func (l *DirLayout) Has(d Digest) (bool, error) {
	_, err := os.Stat(l.blobPath(d))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Get streams the blob named by d.
func (l *DirLayout) Get(d Digest) (io.ReadCloser, int64, error) {
	f, err := os.Open(l.blobPath(d))
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// Put stores r (size bytes) under d, via temp-file + rename (atomic within the
// directory, mirroring the store's dir backend).
func (l *DirLayout) Put(d Digest, size int64, r io.Reader) error {
	dst := l.blobPath(d)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// PutIndex writes indexBytes to the layout root's index.json (creating root).
func (l *DirLayout) PutIndex(indexBytes []byte) error {
	if err := os.MkdirAll(l.root, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(l.root, "index.json"), indexBytes, 0644)
}

// IndexBytes returns the bytes of the layout's index.json.
func (l *DirLayout) IndexBytes() ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(l.root, "index.json"))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Root is the layout's root directory.
func (l *DirLayout) Root() string { return l.root }

// blobPath is the layout path for a digest: blobs/<algorithm>/<encoded>.
func (l *DirLayout) blobPath(d Digest) string {
	return filepath.Join(l.root, "blobs", d.Algorithm().String(), d.Encoded())
}

const layoutDoc = `{"imageLayoutVersion":"1.0.0"}`

// TarSink writes a layout into a tar stream (forward-only). It is used for
// `oci get -o out.tar`. The caller owns the io.Writer (e.g. a file opened
// for write).
type TarSink struct {
	w    *tar.Writer
	seen map[string]bool
}

// NewTarSink returns a TarSink writing to w (w must be opened for write).
func NewTarSink(w *tar.Writer) *TarSink {
	return &TarSink{w: w, seen: map[string]bool{}}
}

// Has is always false (a tar sink has no prior state to check).
func (t *TarSink) Has(Digest) (bool, error) { return false, nil }

// Put writes the blob entry.
func (t *TarSink) Put(d Digest, size int64, r io.Reader) error {
	name := entryName(d)
	if t.seen[name] {
		return nil
	}
	t.seen[name] = true
	hdr := &tar.Header{Name: name, Mode: 0644, Size: size}
	if err := t.w.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := io.Copy(t.w, r)
	return err
}

// PutIndex writes the oci-layout marker and index.json (once each).
func (t *TarSink) PutIndex(indexBytes []byte) error {
	if !t.seen["oci-layout"] {
		t.seen["oci-layout"] = true
		b := []byte(layoutDoc)
		hdr := &tar.Header{Name: "oci-layout", Mode: 0644, Size: int64(len(b))}
		if err := t.w.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := t.w.Write(b); err != nil {
			return err
		}
	}
	if t.seen["index.json"] {
		return nil
	}
	t.seen["index.json"] = true
	hdr := &tar.Header{Name: "index.json", Mode: 0644, Size: int64(len(indexBytes))}
	if err := t.w.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := t.w.Write(indexBytes)
	return err
}

// entryName is the layout path for a digest inside a tar:
// blobs/<algorithm>/<encoded>.
func entryName(d Digest) string {
	return path.Join("blobs", d.Algorithm().String(), d.Encoded())
}

// TarFileSource reads an OCI layout from a seekable file (any ordinary file,
// including a docker-save hybrid). It does a one-time scan to build an
// (entry-name → data-offset) table, then answers Get(d) with a seek and a
// LimitReader. Blobs are streamed — never buffered whole. The docker-archive
// half of a hybrid (manifest.json, repositories) is skipped because Get is
// only ever asked for layout-shaped names (index.json, blobs/**).
type TarFileSource struct {
	r       io.ReadSeeker
	entries map[string]tarEntry
}

type tarEntry struct {
	offset int64 // byte offset of the entry data (after the tar header)
	size   int64
}

// NewTarFileSource scans r and returns a TarFileSource. r must support
// io.ReadSeeker (a regular file). The caller owns r; the TarFileSource holds
// a reference for the lifetime of its Get calls.
func NewTarFileSource(r io.ReadSeeker) (*TarFileSource, error) {
	t := &TarFileSource{r: r, entries: map[string]tarEntry{}}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return t, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			name := path.Clean(hdr.Name)
			// tr.Next() has consumed the tar header; r is now at the start
			// of the entry data.
			dataOff, err := r.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil, err
			}
			t.entries[name] = tarEntry{offset: dataOff, size: hdr.Size}
		}
		// Consume the entry data so the next tr.Next() finds the right
		// header (tar handles the 512-byte block padding via tr).
		if _, err := io.Copy(io.Discard, io.LimitReader(tr, hdr.Size)); err != nil {
			return nil, err
		}
	}
}

// Has reports whether a blob entry is present in the tar.
func (t *TarFileSource) Has(d Digest) (bool, error) {
	_, ok := t.entries[entryName(d)]
	return ok, nil
}

// HasEntry reports whether the tar contains a regular-file entry with the
// given name (e.g. "manifest.json" — the marker of a legacy docker archive).
func (t *TarFileSource) HasEntry(name string) bool {
	_, ok := t.entries[path.Clean(name)]
	return ok
}

// Get streams the entry for d by seeking to its recorded data offset.
func (t *TarFileSource) Get(d Digest) (io.ReadCloser, int64, error) {
	name := entryName(d)
	e, ok := t.entries[name]
	if !ok {
		return nil, 0, fmt.Errorf("oci: tar layout missing %q", name)
	}
	if _, err := t.r.Seek(e.offset, io.SeekStart); err != nil {
		return nil, 0, err
	}
	return &limitReadCloser{reader: io.LimitReader(t.r, e.size)}, e.size, nil
}

// IndexBytes returns the layout's index.json (read from the tar).
func (t *TarFileSource) IndexBytes() ([]byte, error) {
	e, ok := t.entries["index.json"]
	if !ok {
		return nil, fmt.Errorf("oci: tar layout has no index.json (not an OCI layout)")
	}
	if _, err := t.r.Seek(e.offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(t.r, e.size))
}

// limitReadCloser adapts an io.Reader to a no-op Close (the underlying
// ReadSeeker is owned by the TarFileSource, not the caller).
type limitReadCloser struct{ reader io.Reader }

func (c *limitReadCloser) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *limitReadCloser) Close() error               { return nil }
