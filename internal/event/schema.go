package event

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/store"
)

// SchemaStep creates the hub events in the transactional store, so that an
// event commits or rolls back with the state change it reports.
//
// hub_events holds one row per committed event, keyed by seq. time is the
// text timefmt.Format writes, which sorts as the time does. parent_slug is
// the parent of a task fork the event concerns, never served.
//
// hub_event_seq is the one-row counter of the last seq assigned. Emit
// increments it inside the caller's transaction: a rolled-back transaction
// gives its number back, and deleting event rows never lowers it, so a seq
// is never assigned twice, across restarts too.
//
// hub_event_log_bound is the one-row lower bound of the event log: the
// lowest seq still addressable by position. It starts at 1 and only rises.
var SchemaStep = store.Step{
	Name:   "hub_events",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE hub_events (
	seq           INTEGER NOT NULL PRIMARY KEY,
	id            TEXT NOT NULL UNIQUE,
	type          TEXT NOT NULL,
	time          TEXT NOT NULL,
	workspace     TEXT,
	actor_type    TEXT NOT NULL,
	actor_id      TEXT,
	resource_type TEXT NOT NULL,
	resource_id   TEXT NOT NULL,
	action        TEXT NOT NULL,
	session_id    TEXT,
	metadata      TEXT NOT NULL,
	parent_slug   TEXT
);
CREATE INDEX hub_events_workspace ON hub_events (workspace, seq);
CREATE INDEX hub_events_time ON hub_events (time);
CREATE TABLE hub_event_seq (
	only     INTEGER NOT NULL PRIMARY KEY CHECK (only = 1),
	last_seq INTEGER NOT NULL
);
INSERT INTO hub_event_seq (only, last_seq) VALUES (1, 0);
CREATE TABLE hub_event_log_bound (
	only       INTEGER NOT NULL PRIMARY KEY CHECK (only = 1),
	oldest_seq INTEGER NOT NULL
);
INSERT INTO hub_event_log_bound (only, oldest_seq) VALUES (1, 1);`)
		return err
	},
}
