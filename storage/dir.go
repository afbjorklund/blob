package storage

import (
	"fmt"
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

func (s *DirStorage) Put(hash []byte, size int64, r io.Reader) error {
	hexHash := fmt.Sprintf("%x", hash)
	prefix := hexHash[:2]

	dir := filepath.Join(s.rootDir, prefix)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	rest := hexHash[2:]
	filename := rest

	path := filepath.Join(dir, filename)
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

func (s *DirStorage) Get(hash []byte) (io.ReadCloser, int64, error) {
	hexHash := fmt.Sprintf("%x", hash)
	prefix := hexHash[:2]
	rest := hexHash[2:]

	filename := rest
	path := filepath.Join(s.rootDir, prefix, filename)

	f, err := os.Open(path)
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

func (s *DirStorage) Has(hash []byte) (bool, error) {
	hexHash := fmt.Sprintf("%x", hash)
	prefix := hexHash[:2]
	rest := hexHash[2:]

	filename := rest
	path := filepath.Join(s.rootDir, prefix, filename)

	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (s *DirStorage) Index(name, cid string) error {
	indexDir := filepath.Join(s.rootDir, "index")
	if err := os.MkdirAll(indexDir, 0755); err != nil {
		return err
	}

	path := filepath.Join(indexDir, name)
	tmpPath := path + ".tmp"

	if err := os.WriteFile(tmpPath, []byte(cid), 0644); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

func (s *DirStorage) Lookup(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.rootDir, "index", name))
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (s *DirStorage) Close() error {
	return nil
}
