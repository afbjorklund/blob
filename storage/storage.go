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

// Digest is the multihash (algorithm + digest bytes) that keys the CAS
// in every backend. The type is plain []byte so this package has no
// multiformats dependency; the storage layer treats it as opaque. The
// current implementation uses SHA2-256 multihashes, but the storage
// API itself is algorithm-agnostic.
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
