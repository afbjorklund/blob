package storage

import (
	"io"
	"os"
	"path/filepath"
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

func (s *DirStorage) Put(digest []byte, r io.Reader) error {
	hexDigest := string(digest)
	prefix := hexDigest[:2]

	dir := filepath.Join(s.rootDir, prefix)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	path := filepath.Join(dir, hexDigest)
	tmpPath := path + ".tmp"

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

	return os.Rename(tmpPath, path)
}

func (s *DirStorage) Get(digest []byte) (io.ReadCloser, error) {
	hexDigest := string(digest)
	prefix := hexDigest[:2]

	dir := filepath.Join(s.rootDir, prefix)
	path := filepath.Join(dir, hexDigest)

	return os.Open(path)
}

func (s *DirStorage) Has(digest []byte) (bool, error) {
	hexDigest := string(digest)
	prefix := hexDigest[:2]

	dir := filepath.Join(s.rootDir, prefix)
	path := filepath.Join(dir, hexDigest)

	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
