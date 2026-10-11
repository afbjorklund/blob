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

	createTable := "CREATE TABLE IF NOT EXISTS blobs (digest TEXT PRIMARY KEY, data BLOB NOT NULL)"

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

func (s *SQLStorage) Put(digest []byte, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	var errValue error
	endFn := sqlitex.Transaction(s.db)
	stmt, err := s.db.Prepare("INSERT OR REPLACE INTO blobs (digest, data) VALUES (?, ?)")
	if err != nil {
		errValue = err
		endFn(&errValue)
		return err
	}
	defer stmt.Finalize()

	stmt.BindBytes(1, digest)
	stmt.BindBytes(2, data)

	if _, err := stmt.Step(); err != nil {
		errValue = err
		endFn(&errValue)
		return err
	}
	endFn(&errValue)

	return nil
}

func (s *SQLStorage) PutStream(digest []byte, r io.Reader) error {
	var errValue error
	endFn := sqlitex.Transaction(s.db)
	stmt, err := s.db.Prepare("INSERT OR REPLACE INTO blobs (digest, data) VALUES (?, ?)")
	if err != nil {
		errValue = err
		endFn(&errValue)
		return err
	}
	defer stmt.Finalize()

	stmt.BindBytes(1, digest)
	stmt.BindZeroBlob(2, 0)

	if _, err := stmt.Step(); err != nil {
		errValue = err
		endFn(&errValue)
		return err
	}
	if err := stmt.Reset(); err != nil {
		errValue = err
		endFn(&errValue)
		return err
	}

	blob, err := s.db.OpenBlob("", "blobs", "data", 1, true)
	if err != nil {
		errValue = err
		endFn(&errValue)
		return err
	}
	defer blob.Close()

	if _, err := io.Copy(blob, r); err != nil {
		errValue = err
		endFn(&errValue)
		return err
	}
	endFn(&errValue)

	return nil
}

func (s *SQLStorage) Get(digest []byte) (io.ReadCloser, error) {
	stmt, err := s.db.Prepare("SELECT rowid FROM blobs WHERE digest = ?")
	if err != nil {
		return nil, err
	}
	defer stmt.Finalize()

	stmt.BindBytes(1, digest)

	if _, err := stmt.Step(); err != nil {
		if sqlite.ErrCode(err) == sqlite.ResultNotFound {
			return nil, errors.New("blob not found")
		}
		return nil, err
	}

	rowid := stmt.ColumnInt64(0)

	blob, err := s.db.OpenBlob("", "blobs", "data", rowid, false)
	if err != nil {
		return nil, err
	}

	return blob, nil
}

func (s *SQLStorage) Has(digest []byte) (bool, error) {
	stmt, err := s.db.Prepare("SELECT COUNT(*) FROM blobs WHERE digest = ?")
	if err != nil {
		return false, err
	}
	defer stmt.Finalize()

	stmt.BindBytes(1, digest)

	if _, err := stmt.Step(); err != nil {
		return false, err
	}

	count := stmt.ColumnInt(0)
	return count > 0, nil
}

func (s *SQLStorage) Close() error {
	return s.db.Close()
}
