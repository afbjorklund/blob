package storage

import (
	"errors"
	"io"
)

type StorageType string

const (
	StorageDir    StorageType = "dir"
	StorageSQLite StorageType = "sqlite"
)

type StorageBackend interface {
	Put(digest []byte, r io.Reader) error
	Get(digest []byte) (io.ReadCloser, error)
	Has(digest []byte) (bool, error)
}

func NewStorage(backend StorageType, path string) (StorageBackend, error) {
	switch backend {
	case StorageDir:
		return NewDirStorage(path)
	case StorageSQLite:
		return NewSQLiteStorage(path)
	default:
		return nil, errors.New("unknown storage backend")
	}
}
