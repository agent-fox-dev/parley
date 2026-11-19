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

// DenialSchemaStep creates the destinations each sandbox has been reported
// denied: one row per (program, host, port) of a sandbox, whose first
// denial wrote a parley.sandbox.deny event. A row names its sandbox without
// a foreign key: the purge hook removes the rows with the record.
var DenialSchemaStep = store.Step{
	Name:   "sandbox_denials",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE sandbox_denials (
	sandbox_id TEXT NOT NULL,
	program    TEXT NOT NULL,
	host       TEXT NOT NULL,
	port       INTEGER NOT NULL,
	PRIMARY KEY (sandbox_id, program, host, port)
);`)
		return err
	},
}

// ProposalSchemaStep creates the proposals of the sandboxes: one row per
// proposal the runtime made for a sandbox and the hub stored, with the
// runtime's id, unique per sandbox, by which a proposal listed again is
// recognised, and the decision once there is one. Findings and the decider
// are JSON; times are Unix seconds. A row names its sandbox without a
// foreign key: the purge hook removes the rows with the record.
var ProposalSchemaStep = store.Step{
	Name:   "sandbox_proposals",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE sandbox_proposals (
	id                TEXT NOT NULL PRIMARY KEY,
	sandbox_id        TEXT NOT NULL,
	runtime_id        TEXT NOT NULL,
	rule              TEXT NOT NULL,
	summary           TEXT NOT NULL,
	findings          TEXT NOT NULL,
	state             TEXT NOT NULL,
	scope             TEXT,
	workspace_rule_id TEXT,
	decided_by        TEXT,
	decided_at        INTEGER,
	comment           TEXT,
	created_at        INTEGER NOT NULL,
	UNIQUE (sandbox_id, runtime_id)
);
CREATE INDEX sandbox_proposals_sandbox ON sandbox_proposals (sandbox_id, state, created_at, id);`)
		return err
	},
}

// PolicyLogSchemaStep creates how far the policy collector has taken the
// policy log of each sandbox: the time of the last entry taken, in Unix
// nanoseconds as the runtime stamps its entries, and how many entries at
// that time were taken. A log opened again, by the same process after the
// sandbox was ready again or by another after a restart, leaves out what
// was taken, so that no denial is counted twice. A row names its sandbox
// without a foreign key: the purge hook removes it with the record.
var PolicyLogSchemaStep = store.Step{
	Name:   "sandbox_policy_log",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE sandbox_policy_log (
	sandbox_id TEXT NOT NULL PRIMARY KEY,
	taken_at   INTEGER NOT NULL,
	taken      INTEGER NOT NULL
);`)
		return err
	},
}
