// Package access is the one place that decides what a caller may do on a
// workspace, so that every route and every git request that names a
// workspace reaches the same answer for an owner, an organization member, the
// admin token and a stranger.
//
// It keeps the minimal workspace record the rule reads (slug, owner, org,
// kind, parent and status) in the transactional store, before the routes
// that create workspaces exist.
package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/agent-fox-dev/parley/internal/store"
)

// The kinds of workspace: a registered repository, or a task fork created
// from a repository.
const (
	KindRepository = "repository"
	KindFork       = "fork"
)

// The statuses of a workspace. The rule reads none of them; they are kept so
// that the record is whole for the routes that change them.
const (
	StatusPending  = "pending"
	StatusCloning  = "cloning"
	StatusReady    = "ready"
	StatusFailed   = "failed"
	StatusArchived = "archived"
)

// Record is the minimal workspace record. ParentSlug is "" for a repository
// and the slug of the repository a fork was created from for a fork; the
// store keeps "" as NULL.
type Record struct {
	Slug       string `json:"slug"`
	OwnerID    string `json:"owner_id"`
	OrgID      string `json:"org_id"`
	Kind       string `json:"kind"`
	ParentSlug string `json:"parent_slug"`
	Status     string `json:"status"`
}

// ErrDuplicateSlug is returned by InsertRecord when a record with the slug
// exists already.
var ErrDuplicateSlug = errors.New("a workspace record with this slug exists")

// ErrInvalidRecord is returned, wrapped with the reason, by InsertRecord for
// a record it refuses to store.
var ErrInvalidRecord = errors.New("invalid workspace record")

// SchemaStep creates the workspace records in the transactional store. The
// owner and the org carry no foreign key: the workspaces of a deleted org or
// user stay, and a dangling id simply matches no caller. Nor does the parent,
// so that a fork's record never blocks its parent's removal. The slug is
// compared byte for byte, so a slug in another letter case is another slug.
var SchemaStep = store.Step{
	Name:   "workspace_records",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE workspaces (
	slug        TEXT NOT NULL PRIMARY KEY,
	owner_id    TEXT NOT NULL,
	org_id      TEXT NOT NULL,
	kind        TEXT NOT NULL CHECK (kind IN ('repository', 'fork')),
	parent_slug TEXT,
	status      TEXT NOT NULL CHECK (status IN ('pending', 'cloning', 'ready', 'failed', 'archived'))
);
CREATE INDEX workspaces_owner ON workspaces (owner_id);
CREATE INDEX workspaces_org ON workspaces (org_id);
CREATE INDEX workspaces_parent ON workspaces (parent_slug);`)
		return err
	},
}

// Querier is what a read of records goes through: the store's *sql.DB or an
// open *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// InsertRecord stores rec in tx. It returns an error wrapping
// ErrInvalidRecord, and writes nothing, when rec has an empty slug, owner or
// org, an unknown kind or status, a parent although it is a repository, or no
// parent although it is a fork, or when a fork's parent has no record, is a
// fork itself or belongs to another org; it returns ErrDuplicateSlug when the
// slug has a record. The parent is read in tx, so that it cannot change
// between the check and the write.
func InsertRecord(ctx context.Context, tx *sql.Tx, rec Record) error {
	if err := validate(rec); err != nil {
		return err
	}
	if rec.Kind == KindFork {
		parent, found, err := RecordBySlug(ctx, tx, rec.ParentSlug)
		switch {
		case err != nil:
			return fmt.Errorf("insert workspace record %q: %w", rec.Slug, err)
		case !found:
			return fmt.Errorf("%w: parent %q has no record", ErrInvalidRecord, rec.ParentSlug)
		case parent.Kind != KindRepository:
			return fmt.Errorf("%w: parent %q is not a repository", ErrInvalidRecord, rec.ParentSlug)
		case parent.OrgID != rec.OrgID:
			return fmt.Errorf("%w: org %q differs from the parent's org", ErrInvalidRecord, rec.OrgID)
		}
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO workspaces (slug, owner_id, org_id, kind, parent_slug, status)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (slug) DO NOTHING`,
		rec.Slug, rec.OwnerID, rec.OrgID, rec.Kind, sql.NullString{String: rec.ParentSlug, Valid: rec.ParentSlug != ""}, rec.Status)
	if err != nil {
		return fmt.Errorf("insert workspace record %q: %w", rec.Slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("insert workspace record %q: %w", rec.Slug, err)
	}
	if n == 0 {
		return fmt.Errorf("insert workspace record %q: %w", rec.Slug, ErrDuplicateSlug)
	}
	return nil
}

// validate checks what InsertRecord can tell without reading the store.
func validate(rec Record) error {
	switch {
	case rec.Slug == "":
		return fmt.Errorf("%w: empty slug", ErrInvalidRecord)
	case rec.OwnerID == "":
		return fmt.Errorf("%w: empty owner_id", ErrInvalidRecord)
	case rec.OrgID == "":
		return fmt.Errorf("%w: empty org_id", ErrInvalidRecord)
	}
	switch rec.Status {
	case StatusPending, StatusCloning, StatusReady, StatusFailed, StatusArchived:
	default:
		return fmt.Errorf("%w: unknown status %q", ErrInvalidRecord, rec.Status)
	}
	switch rec.Kind {
	case KindRepository:
		if rec.ParentSlug != "" {
			return fmt.Errorf("%w: a repository has no parent", ErrInvalidRecord)
		}
	case KindFork:
		if rec.ParentSlug == "" {
			return fmt.Errorf("%w: a fork has a parent", ErrInvalidRecord)
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidRecord, rec.Kind)
	}
	return nil
}

// recordColumns are the columns scanRecord reads, in its order.
const recordColumns = `slug, owner_id, org_id, kind, parent_slug, status`

// scanRecord reads one row of recordColumns.
func scanRecord(scan func(dest ...any) error) (Record, error) {
	var rec Record
	var parent sql.NullString
	if err := scan(&rec.Slug, &rec.OwnerID, &rec.OrgID, &rec.Kind, &parent, &rec.Status); err != nil {
		return Record{}, err
	}
	rec.ParentSlug = parent.String
	return rec, nil
}

// RecordBySlug reads the record whose slug is exactly slug. A slug no record
// has, whatever the string, is found = false with a nil error, not a
// failure.
func RecordBySlug(ctx context.Context, q Querier, slug string) (Record, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM workspaces WHERE slug = ?`, slug)
	rec, err := scanRecord(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read workspace record: %w", err)
	}
	return rec, true, nil
}

// RecordsOwnedOrIn lists, by slug, the records owned by ownerID or in one of
// orgIDs.
func RecordsOwnedOrIn(ctx context.Context, q Querier, ownerID string, orgIDs []string) ([]Record, error) {
	where := `owner_id = ?`
	if len(orgIDs) > 0 {
		where += ` OR org_id IN (` + placeholders(len(orgIDs)) + `)`
	}
	return listRecords(ctx, q, where, append([]any{ownerID}, anys(orgIDs)...))
}

// ForksOf lists, by slug, the forks whose parent is one of parents. No
// parents list no forks.
func ForksOf(ctx context.Context, q Querier, parents []string) ([]Record, error) {
	if len(parents) == 0 {
		return nil, nil
	}
	return listRecords(ctx, q, `parent_slug IN (`+placeholders(len(parents))+`)`, anys(parents))
}

// listRecords reads the records that match where, ordered by slug.
func listRecords(ctx context.Context, q Querier, where string, args []any) ([]Record, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+recordColumns+` FROM workspaces WHERE `+where+` ORDER BY slug`, args...)
	if err != nil {
		return nil, fmt.Errorf("list workspace records: %w", err)
	}
	defer rows.Close()
	var recs []Record
	for rows.Next() {
		rec, err := scanRecord(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list workspace records: %w", err)
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workspace records: %w", err)
	}
	return recs, nil
}

// placeholders returns n comma-separated parameters.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// anys converts ss to query arguments.
func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// RecordReader is what the rule reads records through, so that a test can
// make a read fail.
type RecordReader interface {
	// Record reads the record with exactly this slug; see RecordBySlug.
	Record(ctx context.Context, slug string) (Record, bool, error)
	// OwnedOrIn lists the records owned by ownerID or in one of orgIDs; see
	// RecordsOwnedOrIn.
	OwnedOrIn(ctx context.Context, ownerID string, orgIDs []string) ([]Record, error)
	// ForksOf lists the forks of parents; see ForksOf.
	ForksOf(ctx context.Context, parents []string) ([]Record, error)
}

// StoreRecords returns the RecordReader that reads the records through q.
func StoreRecords(q Querier) RecordReader {
	return storeRecords{q: q}
}

// storeRecords reads the records with the functions of this file.
type storeRecords struct{ q Querier }

func (s storeRecords) Record(ctx context.Context, slug string) (Record, bool, error) {
	return RecordBySlug(ctx, s.q, slug)
}

func (s storeRecords) OwnedOrIn(ctx context.Context, ownerID string, orgIDs []string) ([]Record, error) {
	return RecordsOwnedOrIn(ctx, s.q, ownerID, orgIDs)
}

func (s storeRecords) ForksOf(ctx context.Context, parents []string) ([]Record, error) {
	return ForksOf(ctx, s.q, parents)
}
