package policy

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/store"
)

// ReviewStep creates the store schema of the evidence policy counts: the
// verdicts users gave on versions of changes, one per user and version, and
// the check results runners reported, one per version and check name. A
// verdict keeps the username and the credential it was given with, since
// the counting rule compares both; carried_from names the version a copy
// was carried from, null for a verdict given on its own version. Times are
// in nanoseconds.
//
// The credential that created each version is not kept here: the change
// component stores it with the version, also when a raced write
// re-attributes it, and the counting reads it from there (ReadVersions).
// Like the policy revisions, the tables have no foreign key to the
// changes: the teardown of a workspace deletes its rows (WorkspaceDeleted),
// so that a change numbered again after a delete starts with none.
var ReviewStep = store.Step{
	Name:   "change_approvals_checks",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range []string{
			`CREATE TABLE change_approvals (
	id              TEXT PRIMARY KEY,
	workspace_slug  TEXT NOT NULL,
	number          INTEGER NOT NULL,
	version         INTEGER NOT NULL CHECK (version > 0),
	user_id         TEXT NOT NULL,
	username        TEXT NOT NULL,
	credential_type TEXT NOT NULL,
	credential_id   TEXT NOT NULL,
	verdict         TEXT NOT NULL CHECK (verdict IN ('approve', 'reject')),
	comment         TEXT NOT NULL,
	carried_from    INTEGER,
	created_at      INTEGER NOT NULL,
	UNIQUE (workspace_slug, number, version, user_id)
)`,
			`CREATE TABLE change_checks (
	workspace_slug TEXT NOT NULL,
	number         INTEGER NOT NULL,
	version        INTEGER NOT NULL CHECK (version > 0),
	name           TEXT NOT NULL,
	status         TEXT NOT NULL CHECK (status IN ('pending', 'passed', 'failed')),
	summary        TEXT NOT NULL,
	url            TEXT,
	details        TEXT,
	actor_type     TEXT NOT NULL,
	actor_id       TEXT,
	username       TEXT,
	updated_at     INTEGER NOT NULL,
	PRIMARY KEY (workspace_slug, number, version, name)
)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	},
}
