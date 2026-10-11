package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type SQLStorage struct {
	dbPath string
	db     *sqlite.Conn
}

func NewSQLStorage(dbPath string) (*SQLStorage, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}

	db, err := sqlite.OpenConn(dbPath, sqlite.OpenCreate|sqlite.OpenReadWrite)
	if err != nil {
		return nil, err
	}

	createTable := "CREATE TABLE IF NOT EXISTS blobs (hash TEXT PRIMARY KEY, data BLOB NOT NULL)"

	var errValue error
	endFn := sqlitex.Transaction(db)
	stmt, err := db.Prepare(createTable)
	if err != nil {
		errValue = err
		endFn(&errValue)
		_ = db.Close()
		return nil, err
	}
	if _, err := stmt.Step(); err != nil {
		errValue = err
		endFn(&errValue)
		if err := stmt.Finalize(); err != nil {
			return nil, err
		}
		_ = db.Close()
		return nil, err
	}
	if err := stmt.Finalize(); err != nil {
		return nil, err
	}
	endFn(&errValue)

	return &SQLStorage{dbPath: dbPath, db: db}, nil
}

func (s *SQLStorage) Put(hash []byte, size int64, r io.Reader) error {
	var errValue error
	endFn := sqlitex.Transaction(s.db)
	defer endFn(&errValue)

	stmt, err := s.db.Prepare("INSERT INTO blobs (hash, data) VALUES (?, zeroblob(?))")
	if err != nil {
		return err
	}
	defer stmt.Finalize()

	stmt.BindBytes(1, hash)
	stmt.BindInt64(2, size)

	if _, err := stmt.Step(); err != nil {
		return err
	}

	rowid := s.db.LastInsertRowID()

	blob, err := s.db.OpenBlob("", "blobs", "data", rowid, true)
	if err != nil {
		return err
	}
	defer blob.Close()

	var buf [32 * 1024]byte
	for {
		n, readErr := r.Read(buf[:])
		if n > 0 {
			if _, writeErr := blob.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	return nil
}

func (s *SQLStorage) Get(hash []byte) (io.ReadCloser, int64, error) {
	var data []byte
	size := int64(0)
	err := sqlitex.ExecuteTransient(
		s.db,
		"SELECT data FROM blobs WHERE hash = ?",
		&sqlitex.ExecOptions{
			Args: []any{hash},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				size = int64(stmt.ColumnLen(0))
				data = make([]byte, size)
				stmt.ColumnBytes(0, data)
				return nil
			},
		},
	)
	if err != nil {
		if sqlite.ErrCode(err) == sqlite.ResultNotFound {
			return nil, 0, errors.New("blob not found")
		}
		return nil, 0, err
	}

	return io.NopCloser(bytes.NewReader(data)), size, nil
}

func (s *SQLStorage) Has(hash []byte) (bool, error) {
	var count int
	err := sqlitex.ExecuteTransient(
		s.db,
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

func (s *SQLStorage) Close() error {
	return s.db.Close()
}
