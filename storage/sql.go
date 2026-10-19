package storage

import (
	"context"
	"errors"
	"io"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// SQLStorage stores a multihash-keyed CAS in SQLite. The `hash` column holds
// the multihash bytes (algorithm + digest) opaquely; no multiformats decoding
// happens here.
//
// A SQLStorage is always backed by a pool of SQLite connections
// ([sqlitex.Pool]). A single *sqlite.Conn is not safe for concurrent use, but
// a pool hands out one connection at a time safely ([Pool.Take] / [Pool.Put]),
// which is exactly what lets the shared read-only gateway serve many requests
// at once and what makes concurrent readers/writers of the CLI safe: each
// operation takes a connection, does only the work it needs (a few statements
// or one streamed blob read), and puts it back.
//
// For a read-write store the pool is opened in WAL mode (OpenWAL), so
// concurrent readers on their own pool connections are not blocked while a
// writer is committing. That is the concurrency story: many readers via the
// pool, and read/write overlap via WAL.
type SQLStorage struct {
	// pool hands out connections for the lifetime of the backend. It is
	// closed (interrupting in-flight reads and unblocking any waiting Take)
	// by Close.
	pool *sqlitex.Pool

	// readOnly is true when the pool was opened read-only. It is also the
	// defensive check the write path (Put / Index) uses to refuse work rather
	// than depending on the open flag alone.
	readOnly bool

	closed bool
}

// poolSize is the number of pooled connections per open backend. Ten is the
// sqlitex default and matches the gateway's model — many concurrent readers.
// It is deliberately fixed rather than scaled to a machine: this is a local
// store, not a database to be sized up.
const poolSize = 10

// initQueries are the per-store DDL statements, executed on a fresh
// read-write open so a brand-new store becomes usable without a separate
// migration step. A read-only open never runs these: it requires an already
// initialized store (an existing database file).
// The `mtime` column defaults to "now" (seconds since the Unix epoch) for
// new rows inserted via this store's own Put. We are not using SQLite's
// `CURRENT_TIMESTAMP` because it is ISO-8601 text; a plain integer is easier
// to render in a header and to compare in a maintenance query. (SQLite
// allows this: a DEFAULT expression must be parenthesised — bare function
// names are rejected by the schema parser.)
var initQueries = []string{
	"CREATE TABLE IF NOT EXISTS blobs (hash BLOB PRIMARY KEY, data BLOB NOT NULL, mtime INTEGER NOT NULL DEFAULT (strftime('%s','now')))",
	"CREATE TABLE IF NOT EXISTS names (name TEXT PRIMARY KEY, hash BLOB NOT NULL REFERENCES blobs(hash))",
}

// prepareConn is the pool's per-connection setup, invoked once for each pooled
// connection on its first checkout: enable foreign keys (enforcing the
// names→blobs reference), then create the schema on a writable store. It is
// safe for concurrent use (the pool may call it from several goroutines at
// startup) because it is stateless and each call takes its own connection.
func prepareConn(readOnly bool) func(conn *sqlite.Conn) error {
	return func(conn *sqlite.Conn) error {
		if err := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = ON", nil); err != nil {
			return err
		}
		if readOnly {
			return nil
		}
		for _, q := range initQueries {
			if err := sqlitex.ExecuteTransient(conn, q, nil); err != nil {
				return err
			}
		}
		return nil
	}
}

func NewSQLStorage(dbPath string, mode Mode) (*SQLStorage, error) {
	var (
		flags    sqlite.OpenFlags
		readOnly bool
	)
	if mode == ReadWrite {
		// OpenReadWrite|OpenCreate so a new store can be created, and OpenWAL
		// so a later writer does not hold readers out (read/write overlap).
		flags = sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenWAL
	} else {
		readOnly = true
		flags = sqlite.OpenReadOnly
	}

	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{
		Flags: flags,
		// PrepareConn runs once per pooled connection on first use; it doubles
		// as the schema bootstrap for a brand-new read-write store.
		PrepareConn: prepareConn(readOnly),
		PoolSize:    poolSize,
	})
	if err != nil {
		return nil, err
	}

	return &SQLStorage{pool: pool, readOnly: readOnly}, nil
}

// take pulls a connection from the pool for the duration of one operation.
// It blocks until a connection is free (Take), so a burst of concurrent
// readers is served in sequence up to the pool size rather than failing.
func (s *SQLStorage) take(ctx context.Context) (*sqlite.Conn, error) {
	return s.pool.Take(ctx)
}

func (s *SQLStorage) put(conn *sqlite.Conn) {
	s.pool.Put(conn)
}

// connReader is the io.ReadCloser returned by Get. A *sqlite.Blob is valid
// only while the connection that opened it is in use, so the reader carries
// that connection and returns it to the pool when the caller closes it (or
// the read finishes). This is what prevents the classic "use a pooled
// connection after it was recycled" crash: the connection stays checked out
// for as long as the caller holds the reader.
type connReader struct {
	blob *sqlite.Blob
	conn *sqlite.Conn
	put  func(*sqlite.Conn)
	done bool
}

func (r *connReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	return r.blob.Read(p)
}

func (r *connReader) Close() error {
	if r.done {
		return nil
	}
	r.done = true
	// Close the blob first, then return the connection. (A read in flight can
	// be interrupted by Close(); that is expected teardown, not corruption.)
	err := r.blob.Close()
	r.put(r.conn)
	return err
}

func (s *SQLStorage) Put(hash Digest, size int64, r io.Reader) error {
	if s.readOnly {
		return ErrReadOnly
	}

	conn, err := s.take(context.Background())
	if err != nil {
		return err
	}
	defer s.put(conn)

	// Insert the row with a sized-but-unsized blob (zeroblob), then stream the
	// data into it via the SQLite blob API so the bytes are never held whole.
	errValue := error(nil)
	endTx := sqlitex.Transaction(conn)
	defer endTx(&errValue)

	stmt, err := conn.Prepare("INSERT INTO blobs (hash, data) VALUES (?, zeroblob(?))")
	if err != nil {
		return err
	}
	defer stmt.Finalize()

	stmt.BindBytes(1, hash)
	stmt.BindInt64(2, size)
	if _, err := stmt.Step(); err != nil {
		return err
	}

	rowid := conn.LastInsertRowID()
	blob, err := conn.OpenBlob("", "blobs", "data", rowid, true)
	if err != nil {
		return err
	}
	defer blob.Close()

	if _, err := blob.ReadFrom(r); err != nil {
		return err
	}
	return nil
}

func (s *SQLStorage) Get(hash Digest) (io.ReadCloser, int64, int64, error) {
	conn, err := s.take(context.Background())
	if err != nil {
		return nil, 0, 0, err
	}

	var (
		rowid int64
		size  int64
		mtime int64
		found bool
	)
	qerr := sqlitex.ExecuteTransient(
		conn,
		"SELECT rowid, length(data), mtime FROM blobs WHERE hash = ?",
		&sqlitex.ExecOptions{
			Args: []any{hash},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				rowid = stmt.ColumnInt64(0)
				size = stmt.ColumnInt64(1)
				mtime = stmt.ColumnInt64(2)
				return nil
			},
		},
	)
	if qerr != nil {
		s.put(conn)
		return nil, 0, 0, qerr
	}
	if !found {
		s.put(conn)
		return nil, 0, 0, errors.New("blob not found")
	}

	blob, err := conn.OpenBlob("", "blobs", "data", rowid, false)
	if err != nil {
		s.put(conn)
		return nil, 0, 0, err
	}

	return &connReader{blob: blob, conn: conn, put: s.put}, size, mtime, nil
}

func (s *SQLStorage) Has(hash Digest) (bool, error) {
	conn, err := s.take(context.Background())
	if err != nil {
		return false, err
	}
	defer s.put(conn)

	var count int
	err = sqlitex.ExecuteTransient(
		conn,
		"SELECT COUNT(*) FROM blobs WHERE hash = ?",
		&sqlitex.ExecOptions{
			Args: []any{hash},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				count = stmt.ColumnInt(0)
				return nil
			},
		},
	)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *SQLStorage) Index(name string, hash Digest) error {
	if s.readOnly {
		return ErrReadOnly
	}
	conn, err := s.take(context.Background())
	if err != nil {
		return err
	}
	defer s.put(conn)

	return sqlitex.ExecuteTransient(
		conn,
		"INSERT OR REPLACE INTO names (name, hash) VALUES (?, ?)",
		&sqlitex.ExecOptions{Args: []any{name, hash}},
	)
}

func (s *SQLStorage) Lookup(name string) (Digest, error) {
	conn, err := s.take(context.Background())
	if err != nil {
		return nil, err
	}
	defer s.put(conn)

	var hash []byte
	found := false
	err = sqlitex.ExecuteTransient(
		conn,
		"SELECT hash FROM names WHERE name = ?",
		&sqlitex.ExecOptions{
			Args: []any{name},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				h, rerr := io.ReadAll(stmt.ColumnReader(0))
				if rerr != nil {
					return rerr
				}
				hash = h
				found = true
				return nil
			},
		},
	)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("name not found")
	}

	return hash, nil
}

// Names lists the recorded names (the rows of the names table — the index,
// not the CAS: blobs without a row are invisible). ORDER BY name gives the
// contract's stable lexicographic order; an empty table is a zero rows, not
// an error.
func (s *SQLStorage) Names() ([]string, error) {
	conn, err := s.take(context.Background())
	if err != nil {
		return nil, err
	}
	defer s.put(conn)

	var names []string
	err = sqlitex.ExecuteTransient(
		conn,
		"SELECT name FROM names ORDER BY name",
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				b, rerr := io.ReadAll(stmt.ColumnReader(0))
				if rerr != nil {
					return rerr
				}
				names = append(names, string(b))
				return nil
			},
		},
	)
	if err != nil {
		return nil, err
	}
	return names, nil
}

// Peek returns the first n bytes of a blob, without reading the rest (fewer if
// the blob is shorter than n or empty). It opens the row's data blob
// (read-only) the same way Get does and copies at most n bytes from the head,
// so the work is bounded by n regardless of blob size and the blob is never
// materialized.
func (s *SQLStorage) Peek(hash Digest, n int) ([]byte, error) {
	conn, err := s.take(context.Background())
	if err != nil {
		return nil, err
	}
	defer s.put(conn)

	var rowid int64
	found := false
	qerr := sqlitex.ExecuteTransient(
		conn,
		"SELECT rowid FROM blobs WHERE hash = ?",
		&sqlitex.ExecOptions{
			Args: []any{hash},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				rowid = stmt.ColumnInt64(0)
				return nil
			},
		},
	)
	if qerr != nil {
		return nil, qerr
	}
	if !found {
		return nil, errors.New("blob not found")
	}

	blob, err := conn.OpenBlob("", "blobs", "data", rowid, false)
	if err != nil {
		return nil, err
	}
	defer blob.Close()

	buf := make([]byte, n)
	got, err := blob.Read(buf)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:got], nil
}

func (s *SQLStorage) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.pool.Close()
}
