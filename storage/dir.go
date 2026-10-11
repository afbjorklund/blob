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
// "<first 2 hex chars>/<rest of hex>".
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
	if !ok || len(prefix) != 2 || len(rest) != 2*32-2 {
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
