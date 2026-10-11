package storage

import (
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

	createBlobs := "CREATE TABLE IF NOT EXISTS blobs (hash TEXT PRIMARY KEY, data BLOB NOT NULL)"

	if err := sqlitex.ExecuteTransient(db, createBlobs, nil); err != nil {
		_ = db.Close()
		return nil, err
	}

	createNames := "CREATE TABLE IF NOT EXISTS names (name TEXT PRIMARY KEY, cid TEXT NOT NULL)"

	if err := sqlitex.ExecuteTransient(db, createNames, nil); err != nil {
		_ = db.Close()
		return nil, err
	}

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

	_, err = blob.ReadFrom(r)
	return err
}

func (s *SQLStorage) Get(hash []byte) (io.ReadCloser, int64, error) {
	var rowid int64
	var size int64
	found := false
	err := sqlitex.ExecuteTransient(
		s.db,
		"SELECT rowid, length(data) FROM blobs WHERE hash = ?",
		&sqlitex.ExecOptions{
			Args: []any{hash},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				rowid = stmt.ColumnInt64(0)
				size = stmt.ColumnInt64(1)
				return nil
			},
		},
	)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, errors.New("blob not found")
	}

	blob, err := s.db.OpenBlob("", "blobs", "data", rowid, false)
	if err != nil {
		return nil, 0, err
	}

	return blob, size, nil
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

func (s *SQLStorage) Index(name, cid string) error {
	return sqlitex.ExecuteTransient(
		s.db,
		"INSERT OR REPLACE INTO names (name, cid) VALUES (?, ?)",
		&sqlitex.ExecOptions{Args: []any{name, cid}},
	)
}

func (s *SQLStorage) Lookup(name string) (string, error) {
	var cid string
	found := false
	err := sqlitex.ExecuteTransient(
		s.db,
		"SELECT cid FROM names WHERE name = ?",
		&sqlitex.ExecOptions{
			Args: []any{name},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				cid = stmt.ColumnText(0)
				return nil
			},
		},
	)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("name not found")
	}

	return cid, nil
}

func (s *SQLStorage) Close() error {
	return s.db.Close()
}
