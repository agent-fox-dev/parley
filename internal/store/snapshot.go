package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// readOnly are the connection parameters of a snapshot's source. mode=ro
// keeps the engine from writing the database, so it never checkpoints the
// write-ahead log of a running hub or one that crashed.
var readOnly = fmt.Sprintf("mode=ro&_pragma=busy_timeout(%d)", busyTimeout)

// readFrozen are the connection parameters of a snapshot's source that has
// no write-ahead log. A read-only connection would still create the log and
// its shared-memory index beside the database and leave them there; an
// immutable one reads the database file alone and creates nothing.
const readFrozen = "mode=ro&immutable=1"

// Snapshot writes a copy of the store file at src to the new file dst. The
// copy is one read transaction of the engine (VACUUM INTO), so it holds the
// transactions committed before it began, each with all its rows, while
// other connections, in this process or another, go on writing; it is a
// database without a write-ahead log. Snapshot changes nothing of the
// source: not the database, not its write-ahead log. dst must not exist; on
// a failure Snapshot removes what it wrote of dst. The error names the file
// that failed.
func Snapshot(ctx context.Context, src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", src, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("snapshot %s: is a directory", src)
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("snapshot to %s: %w", dst, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("snapshot to %s: %w", dst, err)
	}

	if err := snapshotFrozen(ctx, src, dst, fi); !errors.Is(err, errLive) {
		return err
	}
	return vacuumInto(ctx, src, dst, readOnly)
}

// errLive reports that a store without a write-ahead log was opened by a
// writer while it was copied, so the copy must be taken from the live store.
var errLive = errors.New("the store is in use")

// snapshotFrozen copies a store that has no write-ahead log, that is, one
// no connection has open for writing, reading its file alone. When the log
// appears or the file changes before the copy ends, a writer opened it and
// the copy may be torn: snapshotFrozen removes it and returns errLive.
func snapshotFrozen(ctx context.Context, src, dst string, before fs.FileInfo) error {
	wal := src + "-wal"
	if _, err := os.Lstat(wal); !errors.Is(err, fs.ErrNotExist) {
		return errLive
	}
	if err := vacuumInto(ctx, src, dst, readFrozen); err != nil {
		return err
	}
	after, err := os.Stat(src)
	_, walErr := os.Lstat(wal)
	if err == nil && errors.Is(walErr, fs.ErrNotExist) &&
		after.Size() == before.Size() && after.ModTime().Equal(before.ModTime()) {
		return nil
	}
	if err := os.Remove(dst); err != nil {
		return fmt.Errorf("snapshot to %s: %w", dst, err)
	}
	return errLive
}

// vacuumInto writes the database at src, opened with params, to dst.
func vacuumInto(ctx context.Context, src, dst, params string) error {
	db, err := sql.Open("sqlite", dsn(src, params))
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", src, err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		// The engine may have created dst before it failed.
		if rmErr := os.Remove(dst); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
		return fmt.Errorf("snapshot %s to %s: %w", src, dst, err)
	}
	return nil
}
