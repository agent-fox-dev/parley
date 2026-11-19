package telemetry

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/store"
)

// SchemaSteps create the telemetry records in the analytical store, one
// step per kind of record, in the order the hub applies them. A step's name
// is permanent once applied: a later kind adds a step of its own.
//
// Each kind has its own table keyed by its id, so the same id may name
// records of two kinds. Ids are stored in lower case; a client timestamp in
// the fixed-width form of storedLayout and ingested_at as RFC 3339 in UTC
// with whole seconds, so that their text sorts as their time does; JSON
// members as JSON text. No table refers to the workspaces, which live in
// the other store: telemetry outlives its workspace.
var SchemaSteps = []store.Step{
	{Name: "telemetry_run_events", Target: store.Analytics, Apply: createRunEvents},
}

// createRunEvents creates the run events, indexed for the reads of a run in
// time order.
func createRunEvents(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE run_events (
	id          TEXT NOT NULL PRIMARY KEY,
	workspace   TEXT NOT NULL,
	run_id      TEXT NOT NULL,
	timestamp   TEXT NOT NULL,
	ingested_at TEXT NOT NULL,
	event_type  TEXT NOT NULL,
	severity    TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error', 'critical')),
	node_id     TEXT,
	session_id  TEXT,
	payload     TEXT
);
CREATE INDEX run_events_run ON run_events (workspace, run_id, timestamp, id);`)
	return err
}
