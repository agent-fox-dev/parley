package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/store"
)

// ErrNotFound is returned by a write that names a slug no workspace has.
var ErrNotFound = errors.New("workspace not found")

// fieldColumns are the columns SchemaStep adds to the access rule's
// workspaces table, one ALTER TABLE each, as SQLite takes them. A NOT NULL
// column carries a default, so that the records stored before the step get a
// value; the slug stays the table's primary key, unique across repositories
// and forks. Times are UnixNano integers, as the identity tables keep them,
// and the settings document is one JSON object.
var fieldColumns = []string{
	`workspace_mode TEXT NOT NULL DEFAULT 'standard' CHECK (workspace_mode IN ('standard', 'carry_patch'))`,
	`git_url TEXT`,
	`default_branch TEXT`,
	`display_name TEXT NOT NULL DEFAULT ''`,
	`description TEXT NOT NULL DEFAULT ''`,
	`head_sha TEXT`,
	`origin_head_sha TEXT`,
	`clone_error TEXT`,
	`last_sync_at INTEGER`,
	`sync_error TEXT`,
	`upstream_url TEXT`,
	`integration_branch TEXT`,
	`upstream_head_sha TEXT`,
	`fork_point_ref TEXT`,
	`fork_point_sha TEXT`,
	`expires_at INTEGER`,
	`auto_change INTEGER`,
	`purpose TEXT`,
	`session_id TEXT`,
	`settings TEXT NOT NULL DEFAULT '{}'`,
	`created_at INTEGER NOT NULL DEFAULT 0`,
	`updated_at INTEGER NOT NULL DEFAULT 0`,
	`instance_id TEXT NOT NULL DEFAULT ''`,
	`maintenance TEXT NOT NULL DEFAULT '' CHECK (maintenance IN ('', 'reclone', 'archive', 'delete'))`,
}

// SchemaStep gives the workspace records every field of a workspace. It
// extends the table of access.SchemaStep rather than keeping a second one,
// so that the access rule and the workspace routes read one row and a
// workspace is recorded by one insert; it applies after
// access.CreatingTokenStep. Origin heads get no column: they are refs of the
// workspace's copy.
var SchemaStep = store.Step{
	Name:   "workspace_fields",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, col := range fieldColumns {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE workspaces ADD COLUMN `+col); err != nil {
				return err
			}
		}
		return nil
	},
}

// Execer is what a write goes through: an open *sql.Tx, or the store's
// *sql.DB for a write of its own.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Insert stores w in tx. The access rule's columns go through
// access.InsertRecord, so a taken slug is access.ErrDuplicateSlug and a
// record that rule refuses is access.ErrInvalidRecord; the other fields are
// written in the same transaction. A record without an instance id is
// refused too, before anything is written. A nil Settings is stored as the
// empty document.
func Insert(ctx context.Context, tx *sql.Tx, w Workspace) error {
	if w.InstanceID == "" {
		return fmt.Errorf("%w: empty instance id", access.ErrInvalidRecord)
	}
	settings, err := encodeSettings(w.Settings)
	if err != nil {
		return fmt.Errorf("insert workspace %q: %w", w.Slug, err)
	}
	if err := access.InsertRecord(ctx, tx, access.Record{
		Slug: w.Slug, OwnerID: w.OwnerID, OrgID: w.OrgID, Kind: w.Kind,
		ParentSlug: deref(w.ParentSlug), Status: string(w.Status), CreatingTokenID: w.CreatingTokenID,
	}); err != nil {
		return err
	}
	var forkRef, forkSHA *string
	if w.ForkPoint != nil {
		forkRef, forkSHA = &w.ForkPoint.Ref, &w.ForkPoint.SHA
	}
	_, err = tx.ExecContext(ctx, `
UPDATE workspaces SET
	workspace_mode = ?, git_url = ?, default_branch = ?, display_name = ?, description = ?,
	head_sha = ?, origin_head_sha = ?, clone_error = ?, last_sync_at = ?, sync_error = ?,
	upstream_url = ?, integration_branch = ?, upstream_head_sha = ?,
	fork_point_ref = ?, fork_point_sha = ?, expires_at = ?, auto_change = ?, purpose = ?, session_id = ?,
	settings = ?, created_at = ?, updated_at = ?, instance_id = ?, maintenance = ?
WHERE slug = ?`,
		w.Mode, w.GitURL, w.DefaultBranch, w.DisplayName, w.Description,
		w.HeadSHA, w.OriginHeadSHA, w.CloneError, nanos(w.LastSyncAt), w.SyncError,
		w.UpstreamURL, w.IntegrationBranch, w.UpstreamHeadSHA,
		forkRef, forkSHA, nanos(w.ExpiresAt), w.AutoChange, w.Purpose, w.SessionID,
		settings, w.CreatedAt.UnixNano(), w.UpdatedAt.UnixNano(), w.InstanceID, string(w.Marker),
		w.Slug)
	if err != nil {
		return fmt.Errorf("insert workspace %q: %w", w.Slug, err)
	}
	return nil
}

// CloneFields are the fields a clone changes as it runs and when it ends.
type CloneFields struct {
	Status        Status
	DefaultBranch *string
	HeadSHA       *string
	OriginHeadSHA *string
	CloneError    *string
	UpdatedAt     time.Time
}

// UpdateClone writes c over the clone fields of the workspace slug through
// ex, the caller's transaction or the store. Every field of c is written, a
// nil one as null, so the caller passes the values it keeps. A slug no
// workspace has is ErrNotFound.
func UpdateClone(ctx context.Context, ex Execer, slug string, c CloneFields) error {
	res, err := ex.ExecContext(ctx, `
UPDATE workspaces SET status = ?, default_branch = ?, head_sha = ?, origin_head_sha = ?, clone_error = ?, updated_at = ?
WHERE slug = ?`,
		string(c.Status), c.DefaultBranch, c.HeadSHA, c.OriginHeadSHA, c.CloneError, c.UpdatedAt.UnixNano(), slug)
	if err != nil {
		return fmt.Errorf("update clone of workspace %q: %w", slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update clone of workspace %q: %w", slug, err)
	}
	if n == 0 {
		return fmt.Errorf("update clone of workspace %q: %w", slug, ErrNotFound)
	}
	return nil
}

// StartClone moves the workspace slug from pending to cloning and clears
// its clone_error, in one write through ex, so that no one reads a clone in
// progress with the error of an earlier one. A workspace already in cloning
// gets the same write, since its clone is taken up again. started is false,
// with no error, for a workspace in any other status or no workspace.
func StartClone(ctx context.Context, ex Execer, slug string, now time.Time) (started bool, err error) {
	res, err := ex.ExecContext(ctx, `
UPDATE workspaces SET status = ?, clone_error = NULL, updated_at = ?
WHERE slug = ? AND status IN (?, ?)`,
		string(StatusCloning), now.UnixNano(), slug, string(StatusPending), string(StatusCloning))
	if err != nil {
		return false, fmt.Errorf("start the clone of workspace %q: %w", slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("start the clone of workspace %q: %w", slug, err)
	}
	return n == 1, nil
}

// CloneWork lists through q the slugs of the workspaces with clone work
// outstanding, those in pending or cloning, in the order the work was
// recorded: by created_at, then by slug, so that two workspaces created in
// the same second come in a fixed order.
func CloneWork(ctx context.Context, q access.Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT slug FROM workspaces WHERE status IN (?, ?) ORDER BY created_at, slug`,
		string(StatusPending), string(StatusCloning))
	if err != nil {
		return nil, fmt.Errorf("list the clone work: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("list the clone work: %w", err)
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the clone work: %w", err)
	}
	return slugs, nil
}

// workspaceColumns are the columns scanWorkspace reads, in its order.
const workspaceColumns = `slug, kind, workspace_mode, git_url, default_branch, owner_id, org_id, status,
	display_name, description, head_sha, origin_head_sha, clone_error, last_sync_at, sync_error,
	upstream_url, integration_branch, upstream_head_sha, parent_slug, fork_point_ref, fork_point_sha,
	expires_at, auto_change, purpose, session_id, settings, created_at, updated_at,
	instance_id, maintenance, creating_token_id`

// BySlug reads the workspace whose slug is exactly slug through q, the store
// or the caller's transaction. A slug no workspace has is found = false with
// a nil error.
func BySlug(ctx context.Context, q access.Querier, slug string) (Workspace, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces WHERE slug = ?`, slug)
	w, err := scanWorkspace(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, false, nil
	}
	if err != nil {
		return Workspace{}, false, fmt.Errorf("read workspace %q: %w", slug, err)
	}
	return w, true, nil
}

// scanWorkspace reads one row of workspaceColumns.
func scanWorkspace(scan func(dest ...any) error) (Workspace, error) {
	var (
		w                         Workspace
		status, marker, settings  string
		forkRef, forkSHA, creator sql.NullString
		lastSync, expires         sql.NullInt64
		createdAt, updatedAt      int64
	)
	if err := scan(&w.Slug, &w.Kind, &w.Mode, &w.GitURL, &w.DefaultBranch, &w.OwnerID, &w.OrgID, &status,
		&w.DisplayName, &w.Description, &w.HeadSHA, &w.OriginHeadSHA, &w.CloneError, &lastSync, &w.SyncError,
		&w.UpstreamURL, &w.IntegrationBranch, &w.UpstreamHeadSHA, &w.ParentSlug, &forkRef, &forkSHA,
		&expires, &w.AutoChange, &w.Purpose, &w.SessionID, &settings, &createdAt, &updatedAt,
		&w.InstanceID, &marker, &creator); err != nil {
		return Workspace{}, err
	}
	if err := json.Unmarshal([]byte(settings), &w.Settings); err != nil {
		return Workspace{}, fmt.Errorf("settings of workspace %q: %w", w.Slug, err)
	}
	if w.Settings == nil {
		w.Settings = map[string]json.RawMessage{}
	}
	if forkRef.Valid {
		w.ForkPoint = &ForkPoint{Ref: forkRef.String, SHA: forkSHA.String}
	}
	w.Status, w.Marker, w.CreatingTokenID = Status(status), Marker(marker), creator.String
	w.LastSyncAt, w.ExpiresAt = fromNanos(lastSync), fromNanos(expires)
	w.CreatedAt, w.UpdatedAt = time.Unix(0, createdAt).UTC(), time.Unix(0, updatedAt).UTC()
	return w, nil
}

// encodeSettings is the stored form of a settings document.
func encodeSettings(settings map[string]json.RawMessage) (string, error) {
	if settings == nil {
		return "{}", nil
	}
	b, err := json.Marshal(settings)
	return string(b), err
}

// nanos is t as a query argument, NULL when t is nil.
func nanos(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixNano(), Valid: true}
}

// fromNanos is a stored time, nil when it is NULL.
func fromNanos(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64).UTC()
	return &t
}

// deref is *s, or "" when s is nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
