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
// whose time is at or after Since and before Until, when given, that come
// after Before in the order, when given, and that match every filter given,
// at most Limit of them.
type NewestQuery struct {
	Before       *Place
	Since, Until *time.Time
	// Types are the entries an event's type must match one of: an exact
	// type, or a prefix ending in * (ParseTypes); nil for any type.
	Types []string
	// The other filters each give the value the event's field of the same
	// name must equal; nil for any. A field the event does not have
	// (null) equals no value. SessionID is compared as it is, so it is
	// given in lower case, as session ids are stored.
	Workspace, SessionID                                 *string
	ActorType, ActorID, ResourceType, ResourceID, Action *string
	Limit                                                int
}

// equal is a filter that a column holds a value: none when value is nil.
type equal struct {
	column string
	value  *string
}

// equals are the filters of q that compare a column with a value.
func (q NewestQuery) equals() []equal {
	return []equal{
		{"workspace", q.Workspace}, {"session_id", q.SessionID}, {"actor_type", q.ActorType}, {"actor_id", q.ActorID},
		{"resource_type", q.ResourceType}, {"resource_id", q.ResourceID}, {"action", q.Action},
	}
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
	for _, f := range q.equals() {
		if f.value != nil {
			conds, args = append(conds, f.column+" = ?"), append(args, *f.value)
		}
	}
	if cond, targs := typesCondition(q.Types); cond != "" {
		conds, args = append(conds, cond), append(args, targs...)
	}
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

// typesCondition is the condition that an event's type matches one of the
// entries of types, and its arguments; "" when every type matches. A
// prefix is compared as text, not with LIKE, whose _ and % a type may hold.
func typesCondition(types []string) (string, []any) {
	var ors []string
	var args []any
	for _, entry := range types {
		prefix, ok := strings.CutSuffix(entry, "*")
		switch {
		case !ok:
			ors, args = append(ors, "type = ?"), append(args, entry)
		case prefix == "":
			return "", nil
		default:
			ors, args = append(ors, "substr(type, 1, length(?)) = ?"), append(args, prefix, prefix)
		}
	}
	if len(ors) == 0 {
		return "", nil
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// ceilSecond is t in UTC rounded up to the whole second.
func ceilSecond(t time.Time) time.Time {
	t = t.UTC()
	if whole := t.Truncate(time.Second); !whole.Equal(t) {
		return whole.Add(time.Second)
	}
	return t
}
