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

// Digest is the sha256 digest that keys the CAS in every backend.
type Digest = []byte

type StorageBackend interface {
	Put(hash Digest, size int64, r io.Reader) error
	Get(hash Digest) (io.ReadCloser, int64, error)
	Has(hash Digest) (bool, error)
	Index(name string, hash Digest) error
	Lookup(name string) (Digest, error)
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
