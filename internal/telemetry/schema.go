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
	{Name: "telemetry_session_outcomes", Target: store.Analytics, Apply: createSessionOutcomes},
	{Name: "telemetry_tool_calls", Target: store.Analytics, Apply: createToolCalls},
	{Name: "telemetry_tool_errors", Target: store.Analytics, Apply: createToolErrors},
	{Name: "telemetry_traces", Target: store.Analytics, Apply: createTraces},
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

// createSessionOutcomes creates the session outcomes, indexed like the run
// events. token_usage is any JSON value, as JSON text.
func createSessionOutcomes(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE session_outcomes (
	id          TEXT NOT NULL PRIMARY KEY,
	workspace   TEXT NOT NULL,
	run_id      TEXT NOT NULL,
	timestamp   TEXT NOT NULL,
	ingested_at TEXT NOT NULL,
	session_id  TEXT NOT NULL,
	status      TEXT NOT NULL,
	node_id     TEXT,
	duration_ms INTEGER,
	token_usage TEXT
);
CREATE INDEX session_outcomes_run ON session_outcomes (workspace, run_id, timestamp, id);`)
	return err
}

// createToolCalls creates the tool calls, indexed like the run events.
// input and output are any JSON values, as JSON text.
func createToolCalls(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE tool_calls (
	id          TEXT NOT NULL PRIMARY KEY,
	workspace   TEXT NOT NULL,
	run_id      TEXT NOT NULL,
	timestamp   TEXT NOT NULL,
	ingested_at TEXT NOT NULL,
	tool_name   TEXT NOT NULL,
	node_id     TEXT,
	session_id  TEXT,
	duration_ms INTEGER,
	input       TEXT,
	output      TEXT
);
CREATE INDEX tool_calls_run ON tool_calls (workspace, run_id, timestamp, id);`)
	return err
}

// createToolErrors creates the tool errors, indexed like the run events.
func createToolErrors(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE tool_errors (
	id          TEXT NOT NULL PRIMARY KEY,
	workspace   TEXT NOT NULL,
	run_id      TEXT NOT NULL,
	timestamp   TEXT NOT NULL,
	ingested_at TEXT NOT NULL,
	tool_name   TEXT NOT NULL,
	error_msg   TEXT NOT NULL,
	error_code  TEXT,
	node_id     TEXT,
	session_id  TEXT
);
CREATE INDEX tool_errors_run ON tool_errors (workspace, run_id, timestamp, id);`)
	return err
}

// createTraces creates the trace records, from which a run's transcript is
// rebuilt, indexed like the run events. data is any JSON value, as JSON
// text.
func createTraces(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE traces (
	id          TEXT NOT NULL PRIMARY KEY,
	workspace   TEXT NOT NULL,
	run_id      TEXT NOT NULL,
	timestamp   TEXT NOT NULL,
	ingested_at TEXT NOT NULL,
	event_type  TEXT NOT NULL CHECK (event_type IN ('session.init', 'assistant.message', 'tool.use', 'tool.error', 'session.result')),
	node_id     TEXT,
	session_id  TEXT,
	sequence    INTEGER,
	data        TEXT
);
CREATE INDEX traces_run ON traces (workspace, run_id, timestamp, id);`)
	return err
}
