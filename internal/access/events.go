package access

import (
	"context"
	"database/sql"
)

// Event is a hub event this package emits: the minting of a bound token and
// the refusal of one. It holds what the event component takes, in this
// package's own type, because the event component imports this package and
// cannot be imported by it.
type Event struct {
	// Type is the event type, such as parley.token.bind.
	Type string
	// Workspace is the slug of the workspace the event concerns; "" for
	// none.
	Workspace string
	// ActorType is the type of the credential that acted, as the event
	// component names it; ActorID the user it acts for, "" for none.
	ActorType string
	ActorID   string
	// ResourceType and ResourceID name what the event is about.
	ResourceType string
	ResourceID   string
	// Metadata is the event's metadata; nil for none.
	Metadata map[string]any
}

// EventEmitter is the hub's event component as this package calls it. The
// hub passes the real component behind it; there is no discarding default,
// so an emitter that is missing is a fault of the wiring, not a silent loss
// of events.
type EventEmitter interface {
	// Emit records ev in tx, so that the event commits or rolls back with
	// the change it reports.
	Emit(ctx context.Context, tx *sql.Tx, ev Event) error
	// EmitNow records ev in a transaction of its own, for an event that
	// reports no store change.
	EmitNow(ctx context.Context, ev Event) error
}
