package webhook

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/store"
)

// SchemaStep creates the event webhook tables in the transactional store.
//
// A subscription row holds every member of the subscription object, the
// signing secret sealed under the operator's key (never in the clear) and
// its position, the seq up to which it has examined the event log. types is
// the JSON list as given; times are nanoseconds since the Unix epoch in UTC,
// whole seconds, so that a position built from a served object finds the
// row; a nullable member is NULL while it has no value.
//
// A delivery row is one event sent to one subscription across all its
// attempts: the members of the delivery record, the subscription it belongs
// to and the time of its first attempt, from which its retries are counted.
// The store enables foreign keys, so a subscription's deliveries go with it.
var SchemaStep = store.Step{
	Name:   "event_webhooks",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE webhook_subscriptions (
	id               TEXT    NOT NULL PRIMARY KEY,
	workspace        TEXT    NOT NULL,
	url              TEXT    NOT NULL,
	types            TEXT    NOT NULL,
	include_forks    INTEGER NOT NULL CHECK (include_forks IN (0, 1)),
	description      TEXT    NOT NULL,
	state            TEXT    NOT NULL CHECK (state IN ('active', 'failing', 'disabled')),
	created_by       TEXT,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL,
	last_delivery_at INTEGER,
	last_status      TEXT CHECK (last_status IN ('succeeded', 'failed')),
	failing_since    INTEGER,
	secret_sealed    BLOB    NOT NULL,
	position         INTEGER NOT NULL
);
CREATE INDEX webhook_subscriptions_workspace ON webhook_subscriptions (workspace, created_at, id);
CREATE INDEX webhook_subscriptions_state ON webhook_subscriptions (state);
CREATE TABLE webhook_deliveries (
	id               TEXT    NOT NULL PRIMARY KEY,
	subscription_id  TEXT    NOT NULL REFERENCES webhook_subscriptions (id) ON DELETE CASCADE,
	seq              INTEGER NOT NULL,
	type             TEXT    NOT NULL,
	status           TEXT    NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
	attempts         INTEGER NOT NULL,
	response_code    INTEGER,
	error            TEXT CHECK (error IN ('timeout', 'connection_failed', 'tls_error', 'destination_refused', 'redirect')),
	duration_ms      INTEGER,
	created_at       INTEGER NOT NULL,
	first_attempt_at INTEGER,
	last_attempt_at  INTEGER,
	next_attempt_at  INTEGER
);
CREATE INDEX webhook_deliveries_subscription ON webhook_deliveries (subscription_id, created_at, id);
CREATE INDEX webhook_deliveries_pending ON webhook_deliveries (subscription_id, status);
CREATE INDEX webhook_deliveries_last_attempt ON webhook_deliveries (status, last_attempt_at);`)
		return err
	},
}
