package event

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// pingType is the type of the test delivery of an event webhook, which
// tells nothing about the hub, so the audit never shows it.
const pingType = "parley.webhook.ping"

// newestBatch is how many stored events ReadNewest reads at a time at
// least while it skips the events the caller may not see.
const newestBatch = 200

// Place is the place of an event in the order newest first: its time, then
// its id, both descending.
type Place struct {
	Time time.Time
	ID   string
}

// NewestQuery is a read of the stored hub events, newest first: those
// whose time is at or after Since and before Until, when given, and that
// come after Before in the order, when given, at most Limit of them.
type NewestQuery struct {
	Before       *Place
	Since, Until *time.Time
	Limit        int
}

// ReadNewest reads the stored hub events q asks for that caller may see
// (Visible), ordered by time, then id, both descending, at most q.Limit of
// them. It reads every stored event, also those that have left the event
// log, and never one of type parley.webhook.ping. It reads in batches until
// it holds q.Limit events or has read the last one, and decides the
// visibility of all of them with one viewer, so access is read once per
// call.
func (c *Component) ReadNewest(ctx context.Context, caller Caller, q NewestQuery) ([]Record, error) {
	if c.deps.Store == nil {
		return nil, errNotBound
	}
	if q.Limit <= 0 {
		return nil, nil
	}
	v := c.newViewer(caller)
	batch := max(q.Limit, newestBatch)
	var found []Record
	for {
		recs, err := readNewest(ctx, c.deps.Store.Main(), q, batch)
		if err != nil {
			return nil, fmt.Errorf("read the newest events: %w", err)
		}
		for _, rec := range recs {
			ok, err := v.visible(ctx, rec)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if found = append(found, rec); len(found) == q.Limit {
				return found, nil
			}
		}
		if len(recs) < batch {
			return found, nil
		}
		last := recs[len(recs)-1]
		q.Before = &Place{Time: last.Time, ID: last.ID}
	}
}

// readNewest reads at most limit of the stored events q asks for in db,
// newest first, whoever may see them.
func readNewest(ctx context.Context, db Querier, q NewestQuery, limit int) ([]Record, error) {
	conds, args := []string{"type <> ?"}, []any{pingType}
	// Event times are whole seconds, so an event is at or after an instant,
	// or before it, exactly when it is so of the instant rounded up to the
	// second.
	if q.Since != nil {
		conds, args = append(conds, "time >= ?"), append(args, timefmt.Format(ceilSecond(*q.Since)))
	}
	if q.Until != nil {
		conds, args = append(conds, "time < ?"), append(args, timefmt.Format(ceilSecond(*q.Until)))
	}
	if b := q.Before; b != nil {
		if at := b.Time.UTC(); at.Equal(at.Truncate(time.Second)) {
			conds, args = append(conds, "(time, id) < (?, ?)"), append(args, timefmt.Format(at), b.ID)
		} else {
			// No event has the time of a place between two seconds.
			conds, args = append(conds, "time < ?"), append(args, timefmt.Format(ceilSecond(at)))
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT `+recordColumns+` FROM hub_events WHERE `+strings.Join(conds, " AND ")+`
ORDER BY time DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	return scanRecords(rows)
}

// ceilSecond is t in UTC rounded up to the whole second.
func ceilSecond(t time.Time) time.Time {
	t = t.UTC()
	if whole := t.Truncate(time.Second); !whole.Equal(t) {
		return whole.Add(time.Second)
	}
	return t
}
