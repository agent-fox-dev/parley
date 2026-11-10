package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// Snapshot writes a transactionally consistent copy of the store file src to
// the new file dst while other connections keep writing to src. It opens
// src without applying any schema step and changes nothing in it; a src
// that does not exist is refused without being created. The copy is made
// with SQLite's VACUUM INTO, confirmed against modernc.org/sqlite v1.60.1 to
// copy a database in write-ahead-log mode during concurrent commits.
func Snapshot(ctx context.Context, src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("cannot read store %s: %w", src, err)
	}
	if err := checkHeader(src); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", src+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return fmt.Errorf("cannot open store %s: %w", src, err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("cannot copy store %s to %s: %w", src, dst, err)
	}
	return nil
}
