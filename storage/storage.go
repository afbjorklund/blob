package storage

import (
	"io"
)

type StorageBackend interface {
	Put(digest []byte, r io.Reader) error
	Get(digest []byte) (io.ReadCloser, error)
	Has(digest []byte) (bool, error)
}
