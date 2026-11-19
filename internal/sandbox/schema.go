package sandbox

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/store"
)

// RuleSchemaStep creates the workspace sandbox rules: one row per rule of a
// repository workspace, with the rule's body as the runtime gave it, its
// canonical form, by which two rules are compared, and the summary and
// findings of the proposal it was approved from. A workspace holds a rule
// once. Lists and objects are JSON; times are Unix seconds. A rule names its
// workspace without a foreign key: the workspace's remover deletes its
// rules with it.
var RuleSchemaStep = store.Step{
	Name:   "sandbox_rules",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE sandbox_rules (
	id            TEXT NOT NULL PRIMARY KEY,
	workspace     TEXT NOT NULL,
	rule          TEXT NOT NULL,
	canonical     TEXT NOT NULL,
	summary       TEXT NOT NULL,
	findings      TEXT NOT NULL,
	from_proposal TEXT,
	created_by    TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE INDEX sandbox_rules_workspace ON sandbox_rules (workspace, created_at, id);
CREATE UNIQUE INDEX sandbox_rules_canonical ON sandbox_rules (workspace, canonical);`)
		return err
	},
}

// ApprovedRuleSchemaStep creates the rules approved for a sandbox: one row
// per approved proposal, numbered in the order of storage, which breaks the
// ties of decisions made in the same second. A row names its sandbox
// without a foreign key: the purge hook removes the rows with the record.
var ApprovedRuleSchemaStep = store.Step{
	Name:   "sandbox_approved_rules",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE sandbox_approved_rules (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	sandbox_id  TEXT NOT NULL,
	proposal_id TEXT NOT NULL UNIQUE,
	rule        TEXT NOT NULL,
	decided_at  INTEGER NOT NULL
);
CREATE INDEX sandbox_approved_rules_sandbox ON sandbox_approved_rules (sandbox_id, decided_at, seq);`)
		return err
	},
}
