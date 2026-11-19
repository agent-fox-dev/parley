package event

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
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
// log, and never one of type parley.webhook.ping. It reads only the events
// of the workspaces and scopes the caller may see, through the indexes of
// ReadIndexStep, so that a page for a caller who sees few events does not
// read the whole table. It reads in batches until it holds q.Limit events
// or has read the last one, and decides the visibility of all of them with
// one viewer, so access is read once per call.
func (c *Component) ReadNewest(ctx context.Context, caller Caller, q NewestQuery) ([]Record, error) {
	if c.deps.Store == nil {
		return nil, errNotBound
	}
	return c.readVisible(ctx, c.deps.Store.Main(), caller, q)
}

// readVisible is ReadNewest reading the stored events from db.
func (c *Component) readVisible(ctx context.Context, db Querier, caller Caller, q NewestQuery) ([]Record, error) {
	if q.Limit <= 0 {
		return nil, nil
	}
	v := c.newViewer(caller)
	sees, err := v.candidates(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the newest events: %w", err)
	}
	if sees.none {
		return nil, nil
	}
	batch := max(q.Limit, newestBatch)
	var found []Record
	for {
		recs, err := readNewest(ctx, db, q, sees, batch)
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

// readNewest reads at most limit of the stored events q asks for in db
// that meet sees, newest first.
func readNewest(ctx context.Context, db Querier, q NewestQuery, sees candidates, limit int) ([]Record, error) {
	conds, args := []string{"type <> ?"}, []any{pingType}
	if len(sees.ors) > 0 {
		conds, args = append(conds, "("+strings.Join(sees.ors, " OR ")+")"), append(args, sees.args...)
	}
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

// candidates is a condition on a stored event that every event a caller
// may see meets: one of its terms, ors, with their arguments; every event
// when there is none, and no event when none is set. It only narrows what
// a read reads, so that the read uses the indexes of ReadIndexStep instead
// of walking the table: Visible still decides every event read.
type candidates struct {
	none bool
	ors  []string
	args []any
}

// add adds the term cond with its arguments.
func (c *candidates) add(cond string, args ...any) {
	c.ors, c.args = append(c.ors, cond), append(c.args, args...)
}

// in adds the term that column holds one of slugs, given as one JSON array
// so that a set of any size is one argument, and, when members is set, that
// the event's type is not one only owners see. No slug adds no term.
func (c *candidates) in(column string, slugs []string, members bool) {
	if len(slugs) == 0 {
		return
	}
	list, _ := json.Marshal(slugs)
	cond := column + " IN (SELECT value FROM json_each(?))"
	if members {
		cond = "(" + cond + " AND NOT " + ownerOnlyCondition + ")"
	}
	c.add(cond, string(list))
}

// ownerOnlyCondition is the condition that an event's type is one of
// ownerOnly. The prefixes are constants without quotes, so they are
// written into the text.
var ownerOnlyCondition = func() string {
	ors := make([]string, len(ownerOnly))
	for i, p := range ownerOnly {
		ors[i] = fmt.Sprintf("substr(type, 1, %d) = '%s'", len(p), p)
	}
	return "(" + strings.Join(ors, " OR ") + ")"
}()

// scopeCondition is the condition that an event of no workspace has the
// metadata.scope and metadata.scope_id given as its arguments, written as
// ReadIndexStep indexes it. It compares with = and not IN, which the
// planner cannot look up in an index of an expression.
const scopeCondition = `(workspace IS NULL AND json_extract(metadata, '$.scope') = ?
	AND json_extract(metadata, '$.scope_id') = ?)`

// candidates is the condition every event the viewer's caller may see
// meets, from the rule Visible applies: an event of a workspace needs a
// level on the workspace or on the fork parent stored with it, owner or
// above for a type of ownerOnly; an event of no workspace needs the user's
// own scope or one of their active orgs; a bound token sees only the
// workspaces its binding covers, which are the bound workspace and forks
// of it, and the change events of the parent of the fork it is bound to.
func (v *viewer) candidates(ctx context.Context) (candidates, error) {
	var c candidates
	switch {
	case v.caller.Admin:
		return c, nil
	case v.caller.UserID == "", v.caller.Bound() && v.coverage == nil:
		return candidates{none: true}, nil
	}
	readable, err := v.readableSet(ctx)
	if err != nil {
		return c, fmt.Errorf("read the readable set of the caller: %w", err)
	}
	if readable.All {
		return c, nil
	}
	slugs := slices.Sorted(maps.Keys(readable.Entries))
	if v.caller.Bound() {
		covered := []string{v.caller.Workspace}
		for _, slug := range slugs {
			if slug == v.caller.Workspace {
				continue
			}
			ok, err := v.covers(ctx, slug)
			if err != nil {
				return c, err
			}
			if ok {
				covered = append(covered, slug)
			}
		}
		c.in("workspace", covered, false)
		// The forks a binding covers are forks of the bound workspace, so
		// their events are stored with it as their parent.
		c.in("parent_slug", []string{v.caller.Workspace}, false)
		if v.sources != nil {
			parent, err := v.boundParent(ctx)
			if err != nil {
				return c, err
			}
			if parent != "" {
				c.add("(workspace = ? AND resource_type = 'change')", parent)
			}
		}
	} else {
		var owners, members []string
		for _, slug := range slugs {
			switch readable.Level(slug) {
			case access.LevelAdmin, access.LevelOwner:
				owners = append(owners, slug)
			case access.LevelMember:
				members = append(members, slug)
			}
		}
		c.in("workspace", owners, false)
		c.in("parent_slug", owners, false)
		c.in("workspace", members, true)
		c.in("parent_slug", members, true)
		c.add(scopeCondition, "user", v.caller.UserID)
		orgs, err := v.activeOrgs(ctx)
		if err != nil {
			return c, err
		}
		for _, org := range orgs {
			c.add(scopeCondition, "org", org)
		}
	}
	if len(c.ors) == 0 {
		return candidates{none: true}, nil
	}
	return c, nil
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
