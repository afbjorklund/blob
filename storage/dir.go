package storage

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type DirStorage struct {
	rootDir string
}

func NewDirStorage(rootDir string) (*DirStorage, error) {
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, err
	}

	return &DirStorage{rootDir: rootDir}, nil
}

// relPath is the in-store path of a blob, relative to the root dir:
// "<first 2 hex chars>/<rest of hex>". The hex encoding is this
// backend's own choice for turning the (opaque) multihash key bytes
// into a filesystem-friendly name; it does not imply any particular
// hash algorithm.
func (s *DirStorage) relPath(hash Digest) string {
	hexHash := hex.EncodeToString(hash)
	return path.Join(hexHash[:2], hexHash[2:])
}

func (s *DirStorage) Put(hash Digest, size int64, r io.Reader) error {
	dst := path.Join(s.rootDir, s.relPath(hash))

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	tmpPath := dst + ".tmp"

	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, dst)
}

func (s *DirStorage) Get(hash Digest) (io.ReadCloser, int64, error) {
	f, err := os.Open(filepath.Join(s.rootDir, s.relPath(hash)))
	if err != nil {
		return nil, 0, err
	}

	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}

	return f, stat.Size(), nil
}

func (s *DirStorage) Has(hash Digest) (bool, error) {
	_, err := os.Stat(filepath.Join(s.rootDir, s.relPath(hash)))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Index stores a symlink index/<name> -> ../<prefix>/<rest> pointing at
// the blob, so following the entry directly yields the content.
func (s *DirStorage) Index(name string, hash Digest) error {
	if strings.ContainsRune(name, os.PathSeparator) || name == ".." {
		return fmt.Errorf("invalid index name %q", name)
	}

	indexDir := filepath.Join(s.rootDir, "index")
	if err := os.MkdirAll(indexDir, 0755); err != nil {
		return err
	}

	dst := path.Join(indexDir, name)
	tmpPath := dst + ".tmp"

	target := "../" + s.relPath(hash)
	if err := os.Symlink(target, tmpPath); err != nil {
		return err
	}

	return os.Rename(tmpPath, dst)
}

// isValidLinkPart reports whether a single path segment uses only
// characters safe in a relative path: ASCII alphanumerics plus '-'
// and '_'. This deliberately does not assume a particular key length
// or a hex-only alphabet — the current string representation (CID
// base32hex, and hex of the multihash bytes used on disk) is a subset
// of this set.
func isValidLinkPart(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// Lookup resolves an index name to the blob key it points at. The
// entry must stay within the store and use only safe path characters;
// beyond that, the key's exact length and alphabet are not assumed
// (the multihash's algorithm + digest are opaque bytes to this backend).
func (s *DirStorage) Lookup(name string) (Digest, error) {
	link, err := os.Readlink(filepath.Join(s.rootDir, "index", name))
	if err != nil {
		return nil, err
	}

	target, ok := strings.CutPrefix(link, "../")
	if !ok {
		return nil, fmt.Errorf("index entry %q points outside the store: %q", name, link)
	}

	prefix, rest, ok := strings.Cut(target, "/")
	if !ok || !isValidLinkPart(prefix) || !isValidLinkPart(rest) {
		return nil, fmt.Errorf("index entry %q is not a valid blob path: %q", name, link)
	}

	digest, err := hex.DecodeString(prefix + rest)
	if err != nil {
		return nil, fmt.Errorf("index entry %q is not a valid blob path: %q", name, link)
	}

	return digest, nil
}

func (s *DirStorage) Close() error {
	return nil
}
