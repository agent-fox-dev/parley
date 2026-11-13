// Package store opens the hub's two embedded stores and brings them to the
// current schema.
//
// Both stores are SQLite databases opened through modernc.org/sqlite, which
// needs no cgo: the transactional store at the configured database path, and
// the analytical store in AnalyticsFile beside it. Both run in write-ahead-log
// mode with foreign keys on. Every later package registers its own schema
// steps, which Open applies; the store itself owns no feature table.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	_ "modernc.org/sqlite" // registers the driver "sqlite"
)

// AnalyticsFile is the file name of the analytical store, in the directory
// of the transactional store.
const AnalyticsFile = "analytics.db"

// busyTimeout is how long, in milliseconds, a connection waits for another
// one's write lock before it fails with "database is locked". It covers
// writers in other connections of the pool, such as two packages writing the
// analytical store at once; writers of the transactional store's Tx already
// wait for each other in the process.
const busyTimeout = 10000

// readWrite are the connection parameters of both stores. busy_timeout
// comes first so that the switch to write-ahead-log mode waits for a lock
// too. _txlock=immediate takes the write lock when a transaction begins, so
// that two transactions never both read and then fail to upgrade.
var readWrite = fmt.Sprintf(
	"_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate",
	busyTimeout)

// ErrClosed is returned by every method of a Store after Close.
var ErrClosed = errors.New("store is closed")

// Options says where the stores and the workspace root live.
type Options struct {
	// DatabasePath is the file of the transactional store. Its directory is
	// the data directory, which also holds AnalyticsFile.
	DatabasePath string
	// WorkspaceRoot is the directory that holds the workspaces' repositories.
	// Open creates it; the store keeps nothing in it.
	WorkspaceRoot string
}

// Store holds the two open stores. Its methods are safe for concurrent use.
type Store struct {
	main      *sql.DB
	analytics *sql.DB
	// write holds one token: a transaction of Tx takes it before it begins,
	// so that the process's writers queue in memory instead of competing for
	// SQLite's lock, which would leave them to the busy timeout.
	write  chan struct{}
	closed atomic.Bool
}

// Open creates the data directory and the workspace root when they are
// missing, opens both stores, and applies the steps neither store has
// recorded yet (see Step). A failure returns no store and an error that names
// the path or the step that failed; a store file that exists and is not a
// database is left as it was.
func Open(ctx context.Context, opts Options, steps []Step) (*Store, error) {
	if opts.DatabasePath == "" {
		return nil, errors.New("open store: database path is empty")
	}
	if opts.WorkspaceRoot == "" {
		return nil, errors.New("open store: workspace root is empty")
	}
	if err := validate(steps); err != nil {
		return nil, err
	}

	dataDir := filepath.Dir(opts.DatabasePath)
	if err := makeDir(dataDir); err != nil {
		return nil, fmt.Errorf("create data directory %s: %w", dataDir, err)
	}
	if err := makeDir(opts.WorkspaceRoot); err != nil {
		return nil, fmt.Errorf("create workspace root %s: %w", opts.WorkspaceRoot, err)
	}

	analyticsPath := filepath.Join(dataDir, AnalyticsFile)
	// Both files are looked at before either is opened, so that one that is
	// not a database fails the open before the other store is created.
	for _, path := range []string{opts.DatabasePath, analyticsPath} {
		if err := checkDatabaseFile(path); err != nil {
			return nil, err
		}
	}

	main, err := openDB(ctx, opts.DatabasePath)
	if err != nil {
		return nil, err
	}
	analytics, err := openDB(ctx, analyticsPath)
	if err != nil {
		main.Close()
		return nil, err
	}
	s := &Store{main: main, analytics: analytics, write: make(chan struct{}, 1)}
	if err := s.migrate(ctx, steps); err != nil {
		main.Close()
		analytics.Close()
		return nil, err
	}
	return s, nil
}

// makeDir creates dir and its missing parents with mode 0700. A directory
// that exists keeps its mode.
func makeDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// sqliteHeader begins every SQLite database file.
const sqliteHeader = "SQLite format 3\x00"

// checkDatabaseFile fails when path exists and is not a database. It only
// reads: opening such a file through the engine could switch its journal
// mode, that is, write to it. A missing or empty file is fine, since the
// engine makes a new database of it.
func checkDatabaseFile(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open store %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("open store %s: %w", path, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("open store %s: is a directory", path)
	}
	if fi.Size() == 0 {
		return nil
	}
	header := make([]byte, len(sqliteHeader))
	if _, err := f.ReadAt(header, 0); err != nil || string(header) != sqliteHeader {
		return fmt.Errorf("open store %s: not a database", path)
	}
	return nil
}

// dsn returns the data source name of the database file at path with the
// driver's query parameters. The name is a SQLite URI, in which SQLite's own
// parameters such as mode=ro take effect too; the characters that would end
// the path in a URI are escaped.
func dsn(path, params string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	return "file:" + escaped + "?" + params
}

// openDB opens the store at path for reading and writing, creating the file
// when it is missing, and makes sure the engine can read it.
func openDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, readWrite))
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	// sql.Open connects lazily: the first query runs the connection's pragmas
	// and reads the schema, so a file the engine cannot use fails here.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
		db.Close()
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	return db, nil
}

// Tx runs fn in a transaction of the transactional store. It commits what fn
// wrote when fn returns nil; it rolls back when fn returns an error, which Tx
// returns, or panics, when Tx returns an error that carries the panic value.
// Transactions of Tx run one at a time, in the order they ask; the context
// ends a wait for the turn. fn must not call Tx itself: it would wait for its
// own turn forever.
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	if s.closed.Load() {
		return ErrClosed
	}
	select {
	case s.write <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.write }()
	return inTx(ctx, s.main, fn)
}

// inTx runs fn in a transaction of db, committing on nil and rolling back on
// an error or a panic, which it turns into an error: library code never
// takes the process down.
func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	done := false
	defer func() {
		if done {
			return
		}
		tx.Rollback()
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("transaction panicked: %w", e)
			} else {
				err = fmt.Errorf("transaction panicked: %v", r)
			}
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	done = true
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Ping returns nil when both stores answer a query, and otherwise an error
// that names the store that did not.
func (s *Store) Ping(ctx context.Context) error {
	if s.closed.Load() {
		return ErrClosed
	}
	var errs []error
	for _, d := range []struct {
		name string
		db   *sql.DB
	}{{"transactional store", s.main}, {"analytical store", s.analytics}} {
		var one int
		if err := d.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.name, err))
		}
	}
	return errors.Join(errs...)
}

// Close closes both stores. A second call does nothing and returns nil;
// every other method returns ErrClosed afterwards.
func (s *Store) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	return errors.Join(s.main.Close(), s.analytics.Close())
}

// Main returns the transactional store's handle, for reading. Write through
// Tx, which orders the process's writers.
func (s *Store) Main() *sql.DB {
	return s.main
}

// Analytics returns the analytical store's handle, which the packages that
// write telemetry and audit records read and write directly.
func (s *Store) Analytics() *sql.DB {
	return s.analytics
}
