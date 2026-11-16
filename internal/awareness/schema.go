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
