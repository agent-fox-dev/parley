// Package audit answers the audit query, GET /audit: one list of the hub
// events and the agents' run events a caller may see, newest first, paged
// across both sources.
package audit

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// Deps are what the query reads: the hub events, the run events, the
// readable set of a caller and the task forks that exist.
type Deps struct {
	Hub    HubEvents
	Agent  RunEvents
	Access Readable
	Forks  Forks
}

// Component is the audit query. The hub creates its zero value while it
// composes, registers its route and binds it in a start hook.
type Component struct {
	deps Deps
}

// Bind gives the component its dependencies.
func (c *Component) Bind(d Deps) { c.deps = d }

// Routes are the audit routes, for the server's router.
func (c *Component) Routes() []server.Route {
	return []server.Route{
		// No workspace in the path: what a caller sees is decided per
		// record, so a workspace-bound PAT may call it too.
		{Method: http.MethodGet, Pattern: "/audit", Scope: "audit:read", Handler: c.serveQuery},
	}
}

// errNoCaller is a request at the audit route that nobody was
// authenticated for, which the route's scope never lets through.
var errNoCaller = errors.New("no authenticated caller at the audit route")

// errBadCursor refuses a cursor whose position the list cannot continue
// from, as the list helper refuses one that does not decode.
var errBadCursor = server.InvalidRequest("cursor is malformed or belongs to another query")

// serveQuery answers GET /audit: the hub events the visibility rule lets
// the caller see and the run events of the workspaces the caller may see,
// in one list ordered by time, then id, both descending, then a hub event
// before a run event, that match every filter given, a page at a time.
// Every parameter is checked before anything is read, and the filters are
// applied by the stores, so that a page is never cut short by a filter.
// The query applies no age limit of its own: it answers what is stored.
func (c *Component) serveQuery(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	sc, ok := server.CallerOf(ctx)
	if !ok {
		return errNoCaller
	}
	f, err := readFilters(r)
	if err != nil {
		return err
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var pos position
	after, err := page.After(&pos)
	if err != nil {
		return err
	}
	// Each source gives the first limit+1 items after the position, so the
	// first limit+1 of the merge are the list's, and the helper tells from
	// the one past the page whether another page follows.
	fetch := page.Limit + 1
	hubQ, agentQ := f.hubQuery(fetch), f.agentQuery(fetch)
	if after {
		at, err := time.Parse(time.RFC3339Nano, pos.Time)
		if err != nil || (pos.Source != sourceHub && pos.Source != sourceAgent) {
			return errBadCursor
		}
		// A hub event at the position's time and id came before it; a run
		// event there comes after it when the position is a hub event's.
		hubQ.Before = &event.Place{Time: at, ID: pos.ID}
		agentQ.Before = &telemetry.RunEventPlace{Timestamp: at, ID: pos.ID, Inclusive: pos.Source == sourceHub}
	}
	caller := event.CallerOf(r)
	if err := c.checkWorkspace(ctx, caller, f.workspace); err != nil {
		return err
	}
	items := []item{}
	if f.readsHub() {
		recs, err := c.deps.Hub.ReadNewest(ctx, caller, hubQ)
		if err != nil {
			return fmt.Errorf("read the hub events of the audit query: %w", err)
		}
		for _, rec := range recs {
			items = append(items, hubItem(rec))
		}
	}
	if f.readsAgent() {
		if agentQ.Scope, err = c.runScope(ctx, sc); err != nil {
			return err
		}
		recs, err := c.deps.Agent.NewestRunEvents(ctx, agentQ)
		if err != nil {
			return fmt.Errorf("read the run events of the audit query: %w", err)
		}
		for _, rec := range recs {
			items = append(items, agentItem(rec))
		}
	}
	slices.SortFunc(items, compareItems)
	return server.WritePage(w, page, items[:min(len(items), fetch)], positionOf)
}
