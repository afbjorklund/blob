package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

type SQLiteStorage struct {
	db *sql.DB
}

func NewSQLiteStorage(dbPath string) (*SQLiteStorage, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	createTable := `
	CREATE TABLE IF NOT EXISTS blobs (
		digest TEXT PRIMARY KEY,
		data BLOB NOT NULL
	);
	`

	if _, err := db.Exec(createTable); err != nil {
		db.Close()
		return nil, err
	}

	return &SQLiteStorage{db: db}, nil
}

func (s *SQLiteStorage) Put(digest []byte, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	_, err = s.db.Exec("INSERT OR REPLACE INTO blobs (digest, data) VALUES (?, ?)", string(digest), data)
	return err
}

func (s *SQLiteStorage) Get(digest []byte) (io.ReadCloser, error) {
	var data []byte
	err := s.db.QueryRow("SELECT data FROM blobs WHERE digest = ?", string(digest)).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, errors.New("blob not found")
	}
	if err != nil {
		return nil, err
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *SQLiteStorage) Has(digest []byte) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM blobs WHERE digest = ?", string(digest)).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *SQLiteStorage) Close() error {
	return s.db.Close()
}
