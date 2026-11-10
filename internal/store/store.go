// Package store holds the hub's two embedded stores: the transactional
// store, where every feature keeps its records, and the analytical store
// beside it, where telemetry and audit data go. Both are SQLite files
// (modernc.org/sqlite, no cgo) in the data directory.
package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	_ "modernc.org/sqlite" // registers the driver "sqlite"
)

// ErrClosed is returned by a store that has been closed.
var ErrClosed = errors.New("store is closed")

// AnalyticsFile is the analytical store's file name in the data directory.
const AnalyticsFile = "analytics.db"

// Options locate the stores: the transactional store's file, whose
// directory is the data directory, and the workspace root.
type Options struct {
	DatabasePath  string
	WorkspaceRoot string
}

// Store is the pair of open stores.
type Store struct {
	opts      Options
	main      *sql.DB
	analytics *sql.DB
	closed    atomic.Bool
}

// dsn opens a file in write-ahead-log mode with foreign keys on, a busy
// timeout that makes concurrent writers wait for each other, and
// transactions that take the write lock when they begin.
func dsn(path string) string {
	return path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
}

// Open creates the data directory and the workspace root with missing
// parents and mode 0700 (an existing directory keeps its mode), opens the
// transactional store at the database path and the analytical store at
// analytics.db beside it, and applies the schema steps neither has recorded
// yet. Every failure names the path or the step it is about, and leaves no
// store open.
func Open(ctx context.Context, opts Options, steps []Step) (*Store, error) {
	if err := checkSteps(steps); err != nil {
		return nil, err
	}
	dataDir := filepath.Dir(opts.DatabasePath)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create data directory %s: %w", dataDir, err)
	}
	if err := os.MkdirAll(opts.WorkspaceRoot, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create workspace root %s: %w", opts.WorkspaceRoot, err)
	}
	s := &Store{opts: opts}
	var err error
	if s.main, err = openFile(ctx, opts.DatabasePath); err != nil {
		return nil, err
	}
	if s.analytics, err = openFile(ctx, s.AnalyticsPath()); err != nil {
		s.main.Close()
		return nil, err
	}
	if err := s.migrate(ctx, steps); err != nil {
		s.main.Close()
		s.analytics.Close()
		return nil, err
	}
	return s, nil
}

// sqliteHeader starts every SQLite database file.
var sqliteHeader = []byte("SQLite format 3\x00")

// openFile opens one store file; a file that exists and is not a database
// is refused before the engine touches it, so its bytes stay as they are.
func openFile(ctx context.Context, path string) (*sql.DB, error) {
	if err := checkHeader(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("cannot open store %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot open store %s: %w", path, err)
	}
	return db, nil
}

func checkHeader(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot open store %s: %w", path, err)
	}
	defer f.Close()
	head := make([]byte, len(sqliteHeader))
	n, err := io.ReadFull(f, head)
	if n == 0 && (err == io.EOF || err == nil) {
		return nil // an empty file is an empty database
	}
	if !bytes.Equal(head[:n], sqliteHeader) {
		return fmt.Errorf("cannot open store %s: the file is not a database", path)
	}
	return nil
}

// DatabasePath is the transactional store's file.
func (s *Store) DatabasePath() string { return s.opts.DatabasePath }

// AnalyticsPath is the analytical store's file.
func (s *Store) AnalyticsPath() string {
	return filepath.Join(filepath.Dir(s.opts.DatabasePath), AnalyticsFile)
}

// WorkspaceRoot is the directory that holds the repositories.
func (s *Store) WorkspaceRoot() string { return s.opts.WorkspaceRoot }

// DB is the transactional store; write to it through Tx.
func (s *Store) DB() *sql.DB { return s.main }

// Analytics is the analytical store.
func (s *Store) Analytics() *sql.DB { return s.analytics }

// Tx runs fn in a transaction of the transactional store: it commits when fn
// returns nil and rolls back when fn returns an error, which it returns, or
// panics, which it returns as an error carrying the panic value.
func (s *Store) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.tx(ctx, s.main, fn)
}

// AnalyticsTx runs fn in a transaction of the analytical store, as Tx does.
func (s *Store) AnalyticsTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.tx(ctx, s.analytics, fn)
}

func (s *Store) tx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) (err error) {
	if s.closed.Load() {
		return ErrClosed
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			err = fmt.Errorf("transaction panicked: %v", p)
		}
	}()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Ping reports whether both stores answer a query.
func (s *Store) Ping(ctx context.Context) error {
	if s.closed.Load() {
		return ErrClosed
	}
	for _, db := range []*sql.DB{s.main, s.analytics} {
		var one int
		if err := db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
			return err
		}
	}
	return nil
}

// Close closes both stores. It is idempotent; every later call of Tx and
// Ping fails with ErrClosed.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return errors.Join(s.main.Close(), s.analytics.Close())
}
