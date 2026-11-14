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

	"github.com/agent-fox-dev/parley/internal/scrub"
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
	// Scrubber is the hub's credential scrubber.
	Scrubber *scrub.Scrubber
	// Parents looks up the parent of a task fork at emit time; nil stores no
	// parent unless the caller passes one.
	Parents ForkParents
}

// Component is the hub's event component, one per hub. The hub creates it
// when it is composed, so that other parts can keep it, and binds it with
// Bind once the stores are open; New does both at once.
type Component struct {
	deps Deps
}

// New returns a component bound to d.
func New(d Deps) *Component {
	c := &Component{}
	c.Bind(d)
	return c
}

// Bind sets the component's dependencies. The hub calls it once, from a
// start hook, before any other start hook may emit and before it serves.
func (c *Component) Bind(d Deps) {
	c.deps = d
}

// errNotBound is returned by every call made before Bind.
var errNotBound = errors.New("the event component is not bound to a store")

// Emit stores one event for in inside tx, the caller's transaction of the
// transactional store, and returns the stored record with seq, id, time and
// action filled in. The event commits or rolls back with tx.
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
	return rec, nil
}

// Bounds are the bounds of the event log: the lowest and the highest seq it
// holds, both 0 while no event was ever written.
type Bounds struct {
	OldestSeq int64 `json:"oldest_seq"`
	LastSeq   int64 `json:"last_seq"`
}

// Bounds reads the log's bounds. last_seq is the counter, not the highest
// stored row, so it never falls when events are deleted.
func (c *Component) Bounds(ctx context.Context) (Bounds, error) {
	if c.deps.Store == nil {
		return Bounds{}, errNotBound
	}
	var b Bounds
	err := c.deps.Store.Main().QueryRowContext(ctx, `
SELECT s.last_seq, l.oldest_seq FROM hub_event_seq s, hub_event_log_bound l`).Scan(&b.LastSeq, &b.OldestSeq)
	if err != nil {
		return Bounds{}, fmt.Errorf("read event log bounds: %w", err)
	}
	if b.LastSeq == 0 {
		b.OldestSeq = 0
	}
	return b, nil
}

// ReadAfter reads the events with a seq higher than after, ascending, at
// most limit of them (every one when limit <= 0), and the position the read
// examined: the seq of the last record when the limit was reached, otherwise
// last_seq. Both come from one snapshot of the store, so the position never
// claims an event the read could not see.
func (c *Component) ReadAfter(ctx context.Context, after int64, limit int) ([]Record, int64, error) {
	if c.deps.Store == nil {
		return nil, 0, errNotBound
	}
	// A read-only transaction begins deferred, not with the write lock the
	// store's other transactions take, and sees one snapshot.
	tx, err := c.deps.Store.Main().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, fmt.Errorf("read events: %w", err)
	}
	defer tx.Rollback()
	if limit <= 0 {
		limit = -1
	}
	rows, err := tx.QueryContext(ctx, `
SELECT seq, id, type, time, workspace, actor_type, actor_id, resource_type, resource_id, action, session_id,
	metadata, parent_slug
FROM hub_events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	var recs []Record
	for rows.Next() {
		rec, err := scanRecord(rows.Scan)
		if err != nil {
			return nil, 0, fmt.Errorf("read events: %w", err)
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read events: %w", err)
	}
	if limit > 0 && len(recs) == limit {
		return recs, recs[len(recs)-1].Seq, nil
	}
	var last int64
	if err := tx.QueryRowContext(ctx, `SELECT last_seq FROM hub_event_seq`).Scan(&last); err != nil {
		return nil, 0, fmt.Errorf("read events: %w", err)
	}
	return recs, last, nil
}

// scanRecord reads one row of hub_events in the column order of ReadAfter.
func scanRecord(scan func(dest ...any) error) (Record, error) {
	var rec Record
	var stamp, meta string
	var workspace, actorID, session, parent sql.NullString
	if err := scan(&rec.Seq, &rec.ID, &rec.Type, &stamp, &workspace, &rec.ActorType, &actorID,
		&rec.ResourceType, &rec.ResourceID, &rec.Action, &session, &meta, &parent); err != nil {
		return Record{}, err
	}
	t, err := parseTime(stamp)
	if err != nil {
		return Record{}, fmt.Errorf("event %d: %w", rec.Seq, err)
	}
	rec.Time = t
	rec.Workspace, rec.ActorID, rec.SessionID = nullable(workspace), nullable(actorID), nullable(session)
	rec.Metadata = []byte(meta)
	rec.parent = parent.String
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

// nullable is the value of a nullable column, nil for NULL.
func nullable(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}
