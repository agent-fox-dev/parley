// Package event records every significant state change of the hub as one
// hub event, written in the same store transaction as the change it reports,
// and numbers the events with a hub-wide seq that rises by one per committed
// event and is never reused.
//
// Every package that changes state emits through the one Component the hub
// builds: Emit inside the caller's transaction, EmitNow in a transaction of
// its own. Every way of reading the events returns the same Record.
package event

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
)

// Clock is the component's time: the time an event is stamped with, and the
// timers its waits run on, so that a test controls both.
type Clock interface {
	// Now is the current time.
	Now() time.Time
	// NewTimer returns a timer that fires once, d from now.
	NewTimer(d time.Duration) Timer
}

// Timer is a timer of a Clock.
type Timer interface {
	// C is the channel the timer sends the time on when it fires.
	C() <-chan time.Time
	// Stop prevents the timer from firing; it reports whether the timer was
	// still pending.
	Stop() bool
}

// SystemClock is the Clock of a running hub: now for the time (time.Now when
// nil) and the runtime's timers.
func SystemClock(now func() time.Time) Clock {
	if now == nil {
		now = time.Now
	}
	return systemClock{now: now}
}

type systemClock struct{ now func() time.Time }

func (c systemClock) Now() time.Time { return c.now() }

func (c systemClock) NewTimer(d time.Duration) Timer { return systemTimer{time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.t.C }

func (t systemTimer) Stop() bool { return t.t.Stop() }

// Querier is what a read of the store goes through: the store's *sql.DB or
// an open *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ForkParents looks up the parent of a task fork. The hub fills it from the
// workspace records, which this package does not own.
type ForkParents interface {
	// ForkParent returns the slug of the parent when slug names a task fork,
	// and "" when it names a repository or no workspace; q is the emitting
	// transaction, so the lookup sees what the transaction has written.
	ForkParent(ctx context.Context, q Querier, slug string) (string, error)
}

// ForkParentsFunc adapts a function to ForkParents.
type ForkParentsFunc func(ctx context.Context, q Querier, slug string) (string, error)

// ForkParent calls f.
func (f ForkParentsFunc) ForkParent(ctx context.Context, q Querier, slug string) (string, error) {
	return f(ctx, q, slug)
}

// Deps are what the component is built from.
type Deps struct {
	// Store is the transactional store the events live in.
	Store *store.Store
	// Clock stamps the events and runs the component's timers.
	Clock Clock
	// Scrubber is the hub's credential scrubber, which every string of an
	// event's metadata passes through before it is stored; nil stores the
	// metadata as given.
	Scrubber *scrub.Scrubber
	// Parents looks up the parent of a task fork at emit time; nil stores no
	// parent unless the caller passes one.
	Parents ForkParents
	// Access is the hub's access rule, from which the visibility decision
	// reads a user's level on a workspace; nil lets no user see an event of
	// a workspace.
	Access Access
	// Memberships is the membership question the access rule asks, which
	// decides who sees an org-scoped event; nil lets no user see one.
	Memberships access.Memberships
	// Shutdown is the hub's shutdown notification, closed once when the hub
	// begins to stop; every held request returns when it closes. nil never
	// closes.
	Shutdown <-chan struct{}
	// Authenticate checks a request's credential again, as the hub's
	// pipeline does; a stream asks it at every heartbeat and ends when the
	// credential no longer authenticates. nil checks nothing.
	Authenticate server.Authenticator
	// MaxStreams is the number of event streams that may be open at once,
	// [events].max_sse_connections; 0 is no limit.
	MaxStreams int
	// Retention is how long an event stays in the event log,
	// [events].retention; 0 is DefaultRetention.
	Retention time.Duration
}

// Component is the hub's event component, one per hub. The hub creates it
// when it is composed, so that other parts can keep it, and binds it with
// Bind once the stores are open; New does both at once.
type Component struct {
	deps     Deps
	notify   notifier
	injected injected
}

// New returns a component bound to d.
func New(d Deps) (*Component, error) {
	c := &Component{}
	return c, c.Bind(d)
}

// Bind sets the component's dependencies and reads last_seq, from which
// Counters counts the events written since the start. The hub calls it
// once, from a start hook, before any other start hook may emit and before
// it serves, and runs Run once it serves.
func (c *Component) Bind(d Deps) error {
	c.deps = d
	if d.Store == nil {
		return nil
	}
	b, err := readBounds(context.Background(), d.Store.Main())
	if err != nil {
		return fmt.Errorf("bind the event component: read last_seq: %w", err)
	}
	c.notify.start(b.LastSeq)
	return nil
}

// errNotBound is returned by every call made before Bind.
var errNotBound = errors.New("the event component is not bound to a store")

// Emit stores one event for in inside tx, the caller's transaction of the
// transactional store, and returns the stored record with seq, id, time and
// action filled in. The event commits or rolls back with tx. Every string of
// the metadata, at any depth, is stored as the hub's scrubber returns it.
//
// An input that breaks the record contract returns an error wrapping
// ErrInvalidEvent and writes nothing. Any failure to write returns another
// error; the caller returns it from its transaction, which then rolls back,
// and a route answers it as 500 internal.
func (c *Component) Emit(ctx context.Context, tx *sql.Tx, in Input) (Record, error) {
	if c.deps.Store == nil {
		return Record{}, errNotBound
	}
	rec, err := in.check()
	if err != nil {
		return Record{}, err
	}
	// Metadata leaves the hub through the stream, the log and webhooks, so
	// no credential may be stored in it.
	if rec.Metadata, err = scrubMetadata(rec.Metadata, c.deps.Scrubber); err != nil {
		return Record{}, fmt.Errorf("emit event %s: %w", rec.Type, err)
	}
	if rec.parent == "" && rec.Workspace != nil && c.deps.Parents != nil {
		rec.parent, err = c.deps.Parents.ForkParent(ctx, tx, *rec.Workspace)
		if err != nil {
			return Record{}, fmt.Errorf("emit event %s: read the workspace record: %w", rec.Type, err)
		}
	}
	// The counter row is written inside tx, and the store runs one write
	// transaction at a time, so commit order is seq order.
	if err := tx.QueryRowContext(ctx,
		`UPDATE hub_event_seq SET last_seq = last_seq + 1 WHERE only = 1 RETURNING last_seq`).Scan(&rec.Seq); err != nil {
		return Record{}, fmt.Errorf("emit event %s: assign seq: %w", rec.Type, err)
	}
	rec.ID = uuid.NewV4().String()
	stamp := timefmt.Format(c.deps.Clock.Now())
	if rec.Time, err = parseTime(stamp); err != nil {
		return Record{}, fmt.Errorf("emit event %s: %w", rec.Type, err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO hub_events (seq, id, type, time, workspace, actor_type, actor_id, resource_type, resource_id,
	action, session_id, metadata, parent_slug)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Seq, rec.ID, rec.Type, stamp, rec.Workspace, rec.ActorType, rec.ActorID, rec.ResourceType,
		rec.ResourceID, rec.Action, rec.SessionID, string(rec.Metadata),
		sql.NullString{String: rec.parent, Valid: rec.parent != ""}); err != nil {
		return Record{}, fmt.Errorf("emit event %s: write: %w", rec.Type, err)
	}
	// The transaction commits after this returns, so Run may not see the
	// event yet; while anything waits it reads again within pollInterval.
	c.notify.poke()
	return rec, nil
}

// EmitNow stores one event for in in a transaction of its own, for what is
// not a store change; time is the time of writing. It may be called from a
// start hook, before the hub listens. Its errors are those of Emit.
func (c *Component) EmitNow(ctx context.Context, in Input) (Record, error) {
	if c.deps.Store == nil {
		return Record{}, errNotBound
	}
	var rec Record
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = c.Emit(ctx, tx, in)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	c.notify.poke()
	return rec, nil
}

// parseTime reads a time written by timefmt.Format.
func parseTime(stamp string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("event time %q: %w", stamp, err)
	}
	return t.UTC(), nil
}
