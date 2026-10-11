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
	Put(hash []byte, size int64, r io.Reader) error
	Get(hash []byte) (io.ReadCloser, int64, error)
	Has(hash []byte) (bool, error)
	Index(name, cid string) error
	Lookup(name string) (string, error)
	Close() error
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
