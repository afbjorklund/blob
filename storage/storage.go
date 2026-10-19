package storage

import (
	"errors"
	"io"
)

type StorageType string

const (
	StorageDir  StorageType = "dir"
	StorageSQL  StorageType = "sql"
	StorageHTTP StorageType = "http"
	StorageS3   StorageType = "s3"
)

// Mode is how a backend opens a store. It is the storage-layer analogue of an
// access level: a ReadOnly backend never writes (its SQL connections are
// opened read-only and its dir paths are never created or replaced), while a
// ReadWrite backend may add blobs and index names. Because it is about what
// operations are attempted, not who may call the backend, the same
// StorageBackend value can legitimately be used by any CLI — the distinction
// only becomes user-visible if a read-only caller tries to write, in which
// case the backend returns ErrReadOnly.
type Mode int

const (
	// ReadOnly opens a store so it can be read from. The sql backend opens its
	// SQLite connections with OpenReadOnly (an existing database file is
	// required); the dir backend neither creates directories nor replaces
	// entries.
	ReadOnly Mode = iota
	// ReadWrite opens a store so it can also be written to (Put / Index). The
	// sql backend uses OpenReadWrite|OpenCreate (creating on demand) and
	// enables WAL so concurrent readers are not blocked by a writer; the dir
	// backend creates the store and index directories as needed.
	ReadWrite
)

// ErrReadOnly is returned by a read-only backend when a write (Put or Index)
// is attempted. It distinguishes "this store cannot be written to" from a
// genuine failure, so a CLI can report it plainly to the caller.
var ErrReadOnly = errors.New("storage: read-only backend, cannot write")

// Digest is the multihash (algorithm + digest bytes) that keys the CAS
// in every backend. The type is plain []byte so this package has no
// multiformats dependency; the storage layer treats it as opaque. The
// current implementation uses SHA2-256 multihashes, but the storage
// API itself is algorithm-agnostic.
type Digest = []byte

// StorageBackend is a content-addressable blob store keyed by multihash. A
// value is opened in one Mode via NewStorage: a ReadOnly value serves the
// read path (Get / Has / Peek / Lookup) and a ReadWrite value additionally
// serves the write path (Put / Index). Implementations must be safe for
// concurrent use (e.g. the sql backend pools its connections), since a
// shared read-only gateway hands the same backend to many request goroutines.
type StorageBackend interface {
	// Put stores the blob r under hash. On a read-only backend it always
	// returns ErrReadOnly without touching the store.
	Put(hash Digest, size int64, r io.Reader) error
	// Get streams the stored blob. The returned ReadCloser is the reader;
	// Close it to release it (the sql backend releases the pooled connection
	// it was opened on, so Close must be called whether or not the read
	// completed). The second value is the blob size in bytes, the third the
	// blob's last-modified time as Unix seconds (0 when the backend does not
	// track it — in that case callers must treat the value as "unknown" and
	// typically omit it from any output).
	Get(hash Digest) (io.ReadCloser, int64, int64, error)
	Has(hash Digest) (bool, error)
	// Index records a name→hash association. On a read-only backend it
	// always returns ErrReadOnly without touching the store.
	Index(name string, hash Digest) error
	Lookup(name string) (Digest, error)
	// Peek returns up to n leading bytes of the blob identified by digest,
	// without reading the rest (fewer than n, possibly 0, if the blob is
	// shorter than n / empty). It backs the content-based codec derivation
	// (Json vs Raw) for a known digest, e.g. `lookup` name→CID and the
	// `/name` gateway. It reads at most n bytes, so it is bounded work
	// regardless of blob size and never materializes the blob.
	Peek(hash Digest, n int) ([]byte, error)
	// Names returns the index names currently recorded in the store (the
	// key set of the name→blob index). It lists the *index*, not the CAS:
	// a store with one blob and two aliases returns two names; a store
	// with N blobs and zero names returns nil. The results are sorted
	// lexicographically so callers get a stable order independent of
	// backend (dir ReadDir sorts by byte order, sql's ORDER BY name sorts
	// codepoint order, http/s3 receive the listing and re-sort). An empty
	// index is reported as a nil (or empty) slice, not an error.
	Names() ([]string, error)
	// Close releases the backend's resources (the sql backend closes its
	// connection pool, interrupting any reads in flight and unblocking any
	// waiting connection checkout); the dir backend has nothing to release.
	Close() error
}

// NewStorage opens a backend of the requested type in the given mode. `path`
// is the per-backend store location (a local dir for dir, a database file for
// sql, a base URL/endpoint for http and s3). `bucket` and `region` are used
// only by the s3 backend (the named store and the SigV4 region) and are
// ignored by the rest. `auth` are the explicit credentials the remote
// backends apply (Basic-auth user/password for http, access/secret key for
// s3; a no-op for dir and sql).
func NewStorage(backend StorageType, path, bucket, region string, auth Auth, mode Mode) (StorageBackend, error) {
	switch backend {
	case StorageDir:
		return NewDirStorage(path, mode)
	case StorageSQL:
		return NewSQLStorage(path, mode)
	case StorageHTTP:
		// For the http backend the `path` argument is the base URL of the
		// server (e.g. http://localhost:8080), not a local store dir — the
		// store lives on the remote side and the backend is a thin proxy. `auth`
		// are the explicit credentials (from --user/--pass); when absent the
		// http backend fills them from the URL's user:pass@ then ~/.netrc.
		return NewHTTPStorage(path, auth, mode)
	case StorageS3:
		// For the s3 backend `path` is the S3/Ceph RGW endpoint (host:port or
		// an http(s):// URL), `bucket` the named store within it, and `region`
		// the SigV4 region (empty lets minio-go defer to the server's bucket
		// location). `auth` carry the access key / secret.
		return NewS3Storage(path, bucket, region, auth, mode)
	default:
		return nil, errors.New("unknown storage backend")
	}
}
