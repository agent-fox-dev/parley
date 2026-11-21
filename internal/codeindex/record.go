package codeindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/store"
)

// SchemaStep creates the table of the records: one row per workspace that
// has, or had a failed build of, a code index.
var SchemaStep = store.Step{
	Name:   "code_index_records",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE code_index_records (
	workspace_slug TEXT PRIMARY KEY,
	indexed_sha    TEXT,
	partial        INTEGER NOT NULL DEFAULT 0,
	files_indexed  INTEGER NOT NULL DEFAULT 0,
	built_at       INTEGER,
	failed_sha     TEXT,
	error          TEXT,
	failed_at      INTEGER
)`)
		return err
	},
}

// Record is what the hub knows of a workspace's code index: the index there
// is, and the last build that failed since it was built.
type Record struct {
	Workspace string
	// IndexedSHA is the commit the index holds, "" when there is none.
	IndexedSHA string
	// Partial says that a bound stopped the index's build.
	Partial bool
	// FilesIndexed is how many files the index holds.
	FilesIndexed int
	// BuiltAt is when the index was built, nil when there is none.
	BuiltAt *time.Time
	// FailedSHA is the commit whose build failed last, "" when no build
	// failed since the index was built; Error is that failure and FailedAt
	// its time.
	FailedSHA string
	Error     string
	FailedAt  *time.Time
}

// Record is the record of the workspace slug; found is false when it has
// none.
func (c *Component) Record(ctx context.Context, slug string) (Record, bool, error) {
	return readRecord(ctx, c.deps.Store.Main(), slug)
}

func readRecord(ctx context.Context, q access.Querier, slug string) (Record, bool, error) {
	r := Record{Workspace: slug}
	var indexed, failed, text sql.NullString
	var built, failedAt sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT indexed_sha, partial, files_indexed, built_at, failed_sha, error, failed_at
FROM code_index_records WHERE workspace_slug = ?`, slug).Scan(&indexed, &r.Partial, &r.FilesIndexed, &built, &failed, &text, &failedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read the code index record of %s: %w", slug, err)
	}
	r.IndexedSHA, r.FailedSHA, r.Error = indexed.String, failed.String, text.String
	if built.Valid {
		r.BuiltAt = new(time.Unix(0, built.Int64).UTC())
	}
	if failedAt.Valid {
		r.FailedAt = new(time.Unix(0, failedAt.Int64).UTC())
	}
	return r, true, nil
}

// recordBuilt records, in tx, the index of the commit sha of the workspace
// slug, and forgets the failure before it.
func recordBuilt(ctx context.Context, tx *sql.Tx, slug, sha string, partial bool, files int, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO code_index_records (workspace_slug, indexed_sha, partial, files_indexed, built_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (workspace_slug) DO UPDATE SET indexed_sha = excluded.indexed_sha, partial = excluded.partial,
	files_indexed = excluded.files_indexed, built_at = excluded.built_at, failed_sha = NULL, error = NULL, failed_at = NULL`,
		slug, sha, partial, files, at.UnixNano())
	if err != nil {
		return fmt.Errorf("record the code index of %s: %w", slug, err)
	}
	return nil
}

// recordFailed records, in tx, that the build of the commit sha of the
// workspace slug failed with text, and leaves the index there is as it is.
func recordFailed(ctx context.Context, tx *sql.Tx, slug, sha, text string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO code_index_records (workspace_slug, failed_sha, error, failed_at) VALUES (?, ?, ?, ?)
ON CONFLICT (workspace_slug) DO UPDATE SET failed_sha = excluded.failed_sha, error = excluded.error, failed_at = excluded.failed_at`,
		slug, sha, text, at.UnixNano())
	if err != nil {
		return fmt.Errorf("record the failed build of %s: %w", slug, err)
	}
	return nil
}
