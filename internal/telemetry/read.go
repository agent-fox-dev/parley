package telemetry

import (
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// filter is a query parameter of a list that selects the records whose
// column of the same name equals its value; read reads and checks it.
type filter struct {
	name string
	read func(q *Query, name string) *string
}

// The filters of the lists.
var (
	byNodeID    = filter{"node_id", (*Query).String}
	bySessionID = filter{"session_id", (*Query).UUID}
	byStatus    = filter{"status", (*Query).String}
	byToolName  = filter{"tool_name", (*Query).String}
	byEventType = filter{"event_type", (*Query).String}
	bySeverity  = filter{"severity", func(q *Query, name string) *string { return q.OneOf(name, Severities()...) }}
	// A trace's event type is one of a closed set, so a filter on another
	// value is a mistake, not a filter that matches nothing.
	byTraceType = filter{"event_type", func(q *Query, name string) *string { return q.OneOf(name, traceTypes...) }}
)

// readRoutes are the routes that read the records of a run.
func (c *Component) readRoutes() []server.Route {
	return []server.Route{
		readRoute("/events", listOf(c, runEvents, byEventType, bySeverity, byNodeID, bySessionID)),
		readRoute("/sessions/outcomes", listOf(c, outcomes, byNodeID, bySessionID, byStatus)),
		readRoute("/tools/calls", listOf(c, toolCalls, byNodeID, bySessionID, byToolName)),
		readRoute("/tools/errors", listOf(c, toolErrors, byNodeID, bySessionID, byToolName)),
		readRoute("/traces", listOf(c, traces, byTraceType, byNodeID, bySessionID)),
		readRoute("/postmortem", c.servePostmortem),
		readRoute("/transcript", c.serveTranscript),
	}
}

// readRoute is the read route GET runPrefix+suffix. The pipeline checks the
// scope audit:read of a PAT, which audit:write satisfies too, then read
// access to the workspace, so that a caller who cannot read it gets the
// same 404 as for a slug no workspace has. A workspace in any status is
// read, an archived one included: its records stay its history.
func readRoute(suffix string, h server.HandlerFunc) server.Route {
	return server.Route{
		Method:  http.MethodGet,
		Pattern: runPrefix + suffix,
		Scope:   "audit:read",
		Marks:   map[string]string{access.MarkNeed: string(access.NeedRead)},
		Handler: h,
	}
}

// listOf serves the list of the records of the kind k of the path's
// workspace and run, which filters, the time range since/until and order
// select and order, a page at a time. Every parameter is checked before
// the store is read. A run without records is an empty list.
func listOf[R record](c *Component, k kind[R], filters ...filter) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if _, ok := access.DecisionOf(r); !ok {
			return errNoDecision
		}
		q, page, err := readList(r, filters)
		if err != nil {
			return err
		}
		recs, err := list(r.Context(), c.deps.DB, k, q)
		if err != nil {
			return fmt.Errorf("list the %ss of run %s in workspace %s: %w", k.name, q.runID, q.workspace, err)
		}
		return server.WritePage(w, page, recs, func(rec R) any { return positionOf(rec.common()) })
	}
}

// readList reads the list request r of a route with filters: the run id
// of the path, the filters, the time range, the order, then the limit and
// the cursor, whose position is (timestamp, id). The cursor belongs to the
// route, both path parameters and every other query parameter, order
// included, so a cursor of another list or of other filters is refused.
func readList(r *http.Request, filters []filter) (listQuery, server.Page, error) {
	lq := listQuery{workspace: server.Param(r, "slug"), runID: server.Param(r, "run_id")}
	if !ValidRunID(lq.runID) {
		return listQuery{}, server.Page{}, server.InvalidRequest(msgRunID)
	}
	q := ReadQuery(r)
	for _, f := range filters {
		if v := f.read(q, f.name); v != nil {
			lq.matches = append(lq.matches, match{column: f.name, value: *v})
		}
	}
	lq.since, lq.until = q.Range()
	lq.desc = q.Descending()
	if err := q.Err(); err != nil {
		return listQuery{}, server.Page{}, err
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return listQuery{}, server.Page{}, err
	}
	var after position
	ok, err := page.After(&after)
	if err != nil {
		return listQuery{}, server.Page{}, err
	}
	if ok {
		lq.after = &after
	}
	lq.limit = page.Limit + 1
	return lq, page, nil
}
