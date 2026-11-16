package awareness

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/store"
)

// The phases of an indexed change: in flight while it is draft or open,
// landed once it landed, out while it is abandoned.
const (
	PhaseInFlight = "in_flight"
	PhaseLanded   = "landed"
	PhaseOut      = "out"
)

// schema is the index: awareness_changes holds each change the worker has
// seen, under its workspace and current target branch, with its phase and,
// once landed, its landed commit and time; awareness_files the complete set
// of files its current version touches; awareness_pairs the pairs of
// changes of a workspace announced as overlapping, the lower number first;
// awareness_position the seq of the last event the worker is done with, one
// row. Times are UnixNano integers, as the other tables keep them.
var schema = []string{
	`CREATE TABLE awareness_changes (
	workspace_slug TEXT NOT NULL,
	number INTEGER NOT NULL,
	target_branch TEXT NOT NULL,
	phase TEXT NOT NULL CHECK (phase IN ('in_flight', 'landed', 'out')),
	landed_sha TEXT,
	landed_at INTEGER,
	session_id TEXT,
	PRIMARY KEY (workspace_slug, number)
)`,
	`CREATE INDEX awareness_changes_target ON awareness_changes (workspace_slug, target_branch, phase)`,
	`CREATE TABLE awareness_files (
	workspace_slug TEXT NOT NULL,
	number INTEGER NOT NULL,
	path TEXT NOT NULL,
	PRIMARY KEY (workspace_slug, number, path)
)`,
	`CREATE INDEX awareness_files_path ON awareness_files (workspace_slug, path)`,
	`CREATE TABLE awareness_pairs (
	workspace_slug TEXT NOT NULL,
	low INTEGER NOT NULL,
	high INTEGER NOT NULL,
	PRIMARY KEY (workspace_slug, low, high),
	CHECK (low < high)
)`,
	`CREATE TABLE awareness_position (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	seq INTEGER NOT NULL
)`,
	`INSERT INTO awareness_position (id, seq) VALUES (1, 0)`,
}

// SchemaStep adds the index of the changes by touched file and the index
// worker's position.
var SchemaStep = store.Step{
	Name:   "awareness_index",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range schema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	},
}

// claimSchema holds the claims: each with its globs as a JSON array, who
// made it (actor type, id and username), the session and the change it is
// linked to, its stored state, when it was created, when it expires and,
// once it stopped being active, when it ended, which the cleanup removes it
// by. The indexes serve the reads by workspace and target branch, by
// session and by the time a claim expires or ended.
var claimSchema = []string{
	`CREATE TABLE awareness_claims (
	id TEXT PRIMARY KEY,
	workspace_slug TEXT NOT NULL,
	target_branch TEXT NOT NULL,
	paths TEXT NOT NULL,
	intent TEXT NOT NULL,
	actor_type TEXT NOT NULL,
	actor_id TEXT,
	username TEXT,
	session_id TEXT,
	change_number INTEGER,
	state TEXT NOT NULL CHECK (state IN ('active', 'released', 'expired', 'fulfilled')),
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	ended_at INTEGER
)`,
	`CREATE INDEX awareness_claims_target ON awareness_claims (workspace_slug, target_branch, state, created_at)`,
	`CREATE INDEX awareness_claims_session ON awareness_claims (session_id, state)`,
	`CREATE INDEX awareness_claims_expiry ON awareness_claims (state, expires_at)`,
	`CREATE INDEX awareness_claims_ended ON awareness_claims (ended_at)`,
}

// ClaimSchemaStep adds the claims.
var ClaimSchemaStep = store.Step{
	Name:   "awareness_claims",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range claimSchema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	},
}
