package storage

import (
	"errors"
	"io"
)

type StorageType string

const (
	StorageDir StorageType = "dir"
	StorageSQL StorageType = "sql"
)

type StorageBackend interface {
	Put(digest []byte, size int64, r io.Reader) error
	Get(digest []byte) (io.ReadCloser, int64, error)
	Has(digest []byte) (bool, error)
}

func NewStorage(backend StorageType, path string) (StorageBackend, error) {
	switch backend {
	case StorageDir:
		return NewDirStorage(path)
	case StorageSQL:
		return NewSQLStorage(path)
	default:
		return nil, errors.New("unknown storage backend")
	}
}
