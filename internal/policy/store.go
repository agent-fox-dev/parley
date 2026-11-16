package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
)

// SchemaStep creates the store schema of the policy revisions: one row per
// revision a workspace's policy was replaced with, keyed by the workspace's
// slug and the revision, holding the document in normal form, when it was
// written and by whom. Revision 0, the never-set document, is never stored.
// Like the access records, the table has no foreign key to the workspaces:
// the teardown of a workspace deletes its rows (WorkspaceDeleted).
var SchemaStep = store.Step{
	Name:   "policy_revisions",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE policy_revisions (
	workspace_slug TEXT NOT NULL,
	revision       INTEGER NOT NULL CHECK (revision > 0),
	document       TEXT NOT NULL,
	updated_at     INTEGER NOT NULL,
	actor_type     TEXT NOT NULL,
	actor_id       TEXT,
	username       TEXT,
	PRIMARY KEY (workspace_slug, revision)
)`)
		return err
	},
}

// Writer is who wrote a revision: the type of the credential (api_key, pat
// or admin_token) and, for a user's credential, the user's id and
// username; both are nil for the admin token.
type Writer struct {
	ActorType string  `json:"actor_type"`
	ActorID   *string `json:"actor_id"`
	Username  *string `json:"username"`
}

// Revision is one revision of a workspace's policy: its number, its
// document in normal form, and when and by whom it was written. Revision 0
// is the never-set document, with UpdatedAt and UpdatedBy nil.
type Revision struct {
	Number    int64
	Document  Document
	UpdatedAt *time.Time
	UpdatedBy *Writer
}

// revisionZero is the revision of a workspace that never set a policy, and
// of every task fork.
var revisionZero = Revision{Document: NeverSet}

// MarshalJSON writes the revision as the policy routes answer it:
// revision, default, rules, updated_at (whole seconds in UTC) and
// updated_by, the last two null for revision 0.
func (r Revision) MarshalJSON() ([]byte, error) {
	var at *timefmt.Time
	if r.UpdatedAt != nil {
		at = new(timefmt.Time(*r.UpdatedAt))
	}
	return json.Marshal(struct {
		Revision  int64         `json:"revision"`
		Default   Require       `json:"default"`
		Rules     []Rule        `json:"rules"`
		UpdatedAt *timefmt.Time `json:"updated_at"`
		UpdatedBy *Writer       `json:"updated_by"`
	}{r.Number, r.Document.Default, r.Document.Rules, at, r.UpdatedBy})
}

// selectRevision reads the columns scanRevision reads.
const selectRevision = `SELECT revision, document, updated_at, actor_type, actor_id, username FROM policy_revisions`

// CurrentRevision reads, through q, the current revision of the workspace
// slug: the highest stored, or revision 0 when none is.
func CurrentRevision(ctx context.Context, q Querier, slug string) (Revision, error) {
	row := q.QueryRowContext(ctx, selectRevision+` WHERE workspace_slug = ? ORDER BY revision DESC LIMIT 1`, slug)
	rev, err := scanRevision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return revisionZero, nil
	}
	if err != nil {
		return Revision{}, fmt.Errorf("read the current policy revision of %s: %w", slug, err)
	}
	return rev, nil
}

// RevisionAt reads, through q, revision n of the workspace slug. Revision 0
// is found only while no revision is stored, since it stops being the
// policy, and is never kept, once one is; any other revision is found
// while it is stored.
func RevisionAt(ctx context.Context, q Querier, slug string, n int64) (Revision, bool, error) {
	if n == 0 {
		cur, err := CurrentRevision(ctx, q, slug)
		return cur, err == nil && cur.Number == 0, err
	}
	rev, err := scanRevision(q.QueryRowContext(ctx, selectRevision+` WHERE workspace_slug = ? AND revision = ?`, slug, n))
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, false, nil
	}
	if err != nil {
		return Revision{}, false, fmt.Errorf("read policy revision %d of %s: %w", n, slug, err)
	}
	return rev, true, nil
}

// InsertRevision stores rev as a revision of the workspace slug in tx. Its
// number must be positive and not stored yet, and it must say when and by
// whom it was written.
func InsertRevision(ctx context.Context, tx *sql.Tx, slug string, rev Revision) error {
	if rev.Number <= 0 || rev.UpdatedAt == nil || rev.UpdatedBy == nil {
		return fmt.Errorf("insert policy revision %d of %s: a stored revision is positive and has a time and a writer", rev.Number, slug)
	}
	doc, err := json.Marshal(rev.Document)
	if err != nil {
		return fmt.Errorf("insert policy revision %d of %s: %w", rev.Number, slug, err)
	}
	by := rev.UpdatedBy
	if _, err := tx.ExecContext(ctx, `INSERT INTO policy_revisions
	(workspace_slug, revision, document, updated_at, actor_type, actor_id, username) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		slug, rev.Number, string(doc), rev.UpdatedAt.UnixNano(), by.ActorType, by.ActorID, by.Username); err != nil {
		return fmt.Errorf("insert policy revision %d of %s: %w", rev.Number, slug, err)
	}
	return nil
}

// scanRevision reads one row of selectRevision.
func scanRevision(row *sql.Row) (Revision, error) {
	var (
		rev   Revision
		doc   string
		nanos int64
		by    Writer
	)
	if err := row.Scan(&rev.Number, &doc, &nanos, &by.ActorType, &by.ActorID, &by.Username); err != nil {
		return Revision{}, err
	}
	if err := json.Unmarshal([]byte(doc), &rev.Document); err != nil {
		return Revision{}, fmt.Errorf("decode the document of revision %d: %w", rev.Number, err)
	}
	rev.UpdatedAt, rev.UpdatedBy = new(time.Unix(0, nanos).UTC()), &by
	return rev, nil
}

// WorkspaceDeleted deletes every policy revision of the workspace slug in
// tx, the teardown's transaction, so that a workspace registered later
// under the same slug starts at revision 0
// (docs/adr/07-register-the-teardown-collaborators-on-the-workspace-service.md).
func (c *Component) WorkspaceDeleted(ctx context.Context, tx *sql.Tx, slug string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM policy_revisions WHERE workspace_slug = ?`, slug); err != nil {
		return fmt.Errorf("delete the policy revisions of %s: %w", slug, err)
	}
	return nil
}
