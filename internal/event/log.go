package event

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// Bounds are the bounds of the event log: the lowest and the highest seq it
// holds, both 0 while no event was ever written.
type Bounds struct {
	OldestSeq int64 `json:"oldest_seq"`
	LastSeq   int64 `json:"last_seq"`
}

// Bounds reads the log's bounds. LastSeq is the counter, not the highest
// stored row, so it never falls when events are deleted; OldestSeq is 1
// until Expire has moved it.
func (c *Component) Bounds(ctx context.Context) (Bounds, error) {
	if c.deps.Store == nil {
		return Bounds{}, errNotBound
	}
	b, err := readBounds(ctx, c.deps.Store.Main())
	if err != nil {
		return Bounds{}, fmt.Errorf("read event log bounds: %w", err)
	}
	return b, nil
}

// readBounds reads both bounds in q, OldestSeq 0 while LastSeq is.
func readBounds(ctx context.Context, q Querier) (Bounds, error) {
	var b Bounds
	err := q.QueryRowContext(ctx, `
SELECT s.last_seq, l.oldest_seq FROM hub_event_seq s, hub_event_log_bound l`).Scan(&b.LastSeq, &b.OldestSeq)
	if err != nil {
		return Bounds{}, err
	}
	if b.LastSeq == 0 {
		b.OldestSeq = 0
	}
	return b, nil
}

// ErrPositionExpired is the error, a *PositionExpiredError, of a read after
// a position from which at least one event has since left the log.
var ErrPositionExpired = errors.New("event log position expired")

// PositionExpiredError reports a read after a position below oldest_seq
// minus 1: the events between the position and OldestSeq have left the log,
// so the reader would miss them. It matches ErrPositionExpired.
type PositionExpiredError struct {
	After     int64
	OldestSeq int64
}

func (e *PositionExpiredError) Error() string {
	return fmt.Sprintf("%s: position %d is below oldest_seq %d minus 1", ErrPositionExpired, e.After, e.OldestSeq)
}

// Is makes the error match ErrPositionExpired.
func (e *PositionExpiredError) Is(target error) bool { return target == ErrPositionExpired }

// ReadAfter reads the events of the log with a seq higher than after,
// ascending, at most limit of them (every one when limit <= 0), and the
// position the read examined: the seq of the last record when the limit was
// reached, otherwise last_seq. A position below oldest_seq minus 1 is never
// read from, since events after it have left the log: the read fails with a
// *PositionExpiredError, matching ErrPositionExpired, that carries
// oldest_seq, so the caller knows it missed events and where the log now
// begins. Everything comes from one snapshot of the store, so the position
// never claims an event the read could not see; and since the store commits
// events in seq order, the records are a gap-free run of the log (ADR 05).
func (c *Component) ReadAfter(ctx context.Context, after int64, limit int) ([]Record, int64, error) {
	recs, examined, _, err := c.readLog(ctx, max(after, 0), limit)
	return recs, examined, err
}

// readLog is ReadAfter that also returns the bounds of the snapshot it read,
// so that a page's position and its bounds agree. An after below 0 reads
// from the oldest event the log holds, whatever oldest_seq is at the read.
func (c *Component) readLog(ctx context.Context, after int64, limit int) ([]Record, int64, Bounds, error) {
	if c.deps.Store == nil {
		return nil, 0, Bounds{}, errNotBound
	}
	var recs []Record
	var b Bounds
	err := c.snapshot(ctx, func(tx *sql.Tx) error {
		var err error
		if b, err = readBounds(ctx, tx); err != nil {
			return err
		}
		if after < 0 {
			after = max(b.OldestSeq-1, 0)
		} else if after < b.OldestSeq-1 {
			return &PositionExpiredError{After: after, OldestSeq: b.OldestSeq}
		}
		recs, err = queryRecords(ctx, tx, after, b.LastSeq, limit)
		return err
	})
	if err != nil {
		return nil, 0, Bounds{}, fmt.Errorf("read events: %w", err)
	}
	examined := b.LastSeq
	if limit > 0 && len(recs) == limit {
		examined = recs[len(recs)-1].Seq
	}
	return recs, examined, b, nil
}

// Expire makes the events older than cutoff leave the event log and returns
// the new oldest_seq: the lowest seq whose time is not older than cutoff, or
// last_seq when every event is older, so the newest event always stays. The
// bound never moves back, and no event row is deleted: the rows stay stored
// for the audit query until retention deletes them. On a log without events
// it does nothing and returns 0.
func (c *Component) Expire(ctx context.Context, cutoff time.Time) (int64, error) {
	if c.deps.Store == nil {
		return 0, errNotBound
	}
	// Event times are whole seconds, so an event is not older than cutoff
	// exactly when it is not older than cutoff rounded up to the second.
	at := cutoff.UTC()
	if whole := at.Truncate(time.Second); !whole.Equal(at) {
		at = whole.Add(time.Second)
	}
	var oldest int64
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		b, err := readBounds(ctx, tx)
		if err != nil || b.LastSeq == 0 {
			return err
		}
		var first sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(seq) FROM hub_events WHERE time >= ?`,
			timefmt.Format(at)).Scan(&first); err != nil {
			return err
		}
		oldest = b.LastSeq
		if first.Valid && first.Int64 < oldest {
			oldest = first.Int64
		}
		if oldest <= b.OldestSeq {
			oldest = b.OldestSeq
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE hub_event_log_bound SET oldest_seq = ? WHERE only = 1`, oldest)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("expire events before %s: %w", timefmt.Format(at), err)
	}
	return oldest, nil
}

// snapshot runs fn in a read-only transaction of the store, which begins
// deferred, not with the write lock the store's other transactions take,
// and sees one snapshot.
func (c *Component) snapshot(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := c.deps.Store.Main().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// queryRecords reads the stored events with after < seq <= upTo in q,
// ascending, at most limit of them (every one when limit <= 0).
func queryRecords(ctx context.Context, q Querier, after, upTo int64, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := q.QueryContext(ctx, `
SELECT seq, id, type, time, workspace, actor_type, actor_id, resource_type, resource_id, action, session_id,
	metadata, parent_slug
FROM hub_events WHERE seq > ? AND seq <= ? ORDER BY seq LIMIT ?`, after, upTo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var recs []Record
	for rows.Next() {
		rec, err := scanRecord(rows.Scan)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	return recs, rows.Err()
}

// scanRecord reads one row of hub_events in the column order of
// queryRecords.
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

// nullable is the value of a nullable column, nil for NULL.
func nullable(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}
