package session

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// statuses and credentialTypes are the values the status and
// credential_type filters of the session list accept.
var (
	statuses        = []string{StatusActive, StatusCompleted, StatusFailed, StatusTimeout, StatusTerminated}
	credentialTypes = []string{CredentialAdminToken, CredentialAPIKey, CredentialPAT}
)

// listFilter is what a session list request selects besides the caller's
// readable sessions: each member that is set must match.
type listFilter struct {
	// workspace is the slug of the workspace filter ("" for none); with
	// includeForks the sessions whose stored parent is that slug match too.
	workspace    string
	includeForks bool
	// runID, status and credentialType match their column exactly.
	runID, status, credentialType string
	// since and until bound started_at (since inclusive, until exclusive)
	// as stored timestamps; "" for no bound.
	since, until string
	ascending    bool
}

// listPosition is the place of a session in the session list: its
// started_at as stored, then its id.
type listPosition struct {
	StartedAt string `json:"s"`
	ID        string `json:"i"`
}

// listRoute serves GET /sessions: the sessions the caller may read that
// match every filter given, ordered by (started_at, id), descending unless
// order is asc, paged by limit and cursor. Every query parameter is checked
// before anything is read; then the workspace filter must name a workspace
// the caller may read (and a bound token's binding covers), with one answer
// for every other slug, so that the answer does not tell whether it exists.
// The readable sessions are selected in the query itself, so that every
// page is exact and holds no session GET /sessions/:id would hide from the
// same caller.
func (c *Component) listRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	sc, ok := server.CallerOf(ctx)
	if !ok {
		return errNoPrincipal
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	f, err := listFilterOf(r.URL.Query())
	if err != nil {
		return err
	}
	var after listPosition
	continuing, err := page.After(&after)
	if err != nil {
		return err
	}
	if continuing && (after.StartedAt == "" || after.ID == "") {
		return server.InvalidRequest("cursor is malformed or belongs to another query")
	}
	caller := access.CallerFrom(sc)
	if f.workspace != "" {
		if err := c.decideRead(ctx, caller, f.workspace); err != nil {
			return err
		}
	}
	conds, args, err := c.readableSessions(ctx, caller, f)
	if err != nil {
		return err
	}
	positions, err := c.listPositions(ctx, conds, args, f.ascending, continuing, after, page.Limit+1)
	if err != nil {
		return err
	}
	// The next cursor comes from the positions the query read, not from
	// the sessions read after it, so that a session removed in between
	// makes the page shorter but never ends the list early.
	var next *string
	if len(positions) > page.Limit {
		positions = positions[:page.Limit]
		cur, err := page.Cursor(positions[len(positions)-1])
		if err != nil {
			return err
		}
		next = &cur
	}
	db := c.deps.Store.Main()
	items := make([]Object, 0, len(positions))
	for _, p := range positions {
		s, found, err := readRow(ctx, db, p.ID)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		o, err := object(ctx, db, s)
		if err != nil {
			return err
		}
		items = append(items, o)
	}
	return server.WriteList(w, items, next)
}

// listFilterOf reads the filters and the order of a session list request
// from its query q, refusing with 400 invalid_request, naming the
// parameter, any value outside a parameter's form rather than ignoring it.
func listFilterOf(q url.Values) (listFilter, error) {
	var f listFilter
	var err error
	if f.status, err = oneOf(q, "status", statuses); err != nil {
		return listFilter{}, err
	}
	if f.credentialType, err = oneOf(q, "credential_type", credentialTypes); err != nil {
		return listFilter{}, err
	}
	order, err := oneOf(q, "order", []string{"asc", "desc"})
	if err != nil {
		return listFilter{}, err
	}
	f.ascending = order == "asc"
	includeForks, err := oneOf(q, "include_forks", []string{"true", "false"})
	if err != nil {
		return listFilter{}, err
	}
	f.includeForks = includeForks == "true"
	if f.runID, err = single(q, "run_id"); err != nil {
		return listFilter{}, err
	}
	if q.Has("run_id") && !runIDForm.MatchString(f.runID) {
		return listFilter{}, server.InvalidRequest("run_id must have the form YYYYMMDD_HHMMSS_ and six lower-case hexadecimal digits")
	}
	if f.since, err = boundOf(q, "since"); err != nil {
		return listFilter{}, err
	}
	if f.until, err = boundOf(q, "until"); err != nil {
		return listFilter{}, err
	}
	if f.workspace, err = single(q, "workspace"); err != nil {
		return listFilter{}, err
	}
	if q.Has("workspace") && f.workspace == "" {
		return listFilter{}, server.InvalidRequest("workspace must be a non-empty slug")
	}
	if q.Has("include_forks") && !q.Has("workspace") {
		return listFilter{}, server.InvalidRequest("include_forks needs the workspace parameter")
	}
	return f, nil
}

// single reads the query parameter name of q: "" when it is absent, and
// 400 naming it when it is given more than once, rather than one of its
// values chosen.
func single(q url.Values, name string) (string, error) {
	values := q[name]
	if len(values) > 1 {
		return "", server.InvalidRequest(name + " must be given once")
	}
	if len(values) == 0 {
		return "", nil
	}
	return values[0], nil
}

// oneOf reads the query parameter name of q, which must be one of allowed
// when it is given; "" when it is absent.
func oneOf(q url.Values, name string, allowed []string) (string, error) {
	v, err := single(q, name)
	if err != nil {
		return "", err
	}
	if q.Has(name) && !slices.Contains(allowed, v) {
		return "", server.InvalidRequest(fmt.Sprintf("%s must be one of %s", name, strings.Join(allowed, ", ")))
	}
	return v, nil
}

// boundOf reads the query parameter name of q, an RFC 3339 timestamp with
// any offset, as the stored timestamp it bounds started_at with; "" when
// it is absent. Stored timestamps are whole seconds in UTC, so a bound
// with a fraction of a second is moved up to the next whole second: no
// stored timestamp lies between the two, and "since" stays inclusive and
// "until" exclusive of the instant given.
func boundOf(q url.Values, name string) (string, error) {
	v, err := single(q, name)
	if err != nil || !q.Has(name) {
		return "", err
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", server.InvalidRequest(name + " must be an RFC 3339 timestamp, such as 2026-01-02T03:04:05Z")
	}
	if whole := t.Truncate(time.Second); !whole.Equal(t) {
		t = whole.Add(time.Second)
	}
	return timefmt.Format(t), nil
}

// decideRead decides whether caller may filter the session list by the
// workspace slug: a bound token's binding must cover it, and the caller
// needs read access. A slug the caller may not read, one the binding does
// not cover and one that names no workspace are refused with one answer.
func (c *Component) decideRead(ctx context.Context, caller access.Caller, slug string) error {
	if c.deps.Coverage != nil {
		bound, covered, err := c.deps.Coverage.Covers(ctx, caller, slug)
		if err != nil {
			return fmt.Errorf("ask the coverage of workspace %s: %w", slug, err)
		}
		if bound && !covered {
			return server.NotFound(msgWorkspaceNotFound)
		}
	}
	d, err := c.deps.Access.Decide(ctx, caller, slug, access.NeedRead)
	if err != nil {
		return fmt.Errorf("decide read access to workspace %s: %w", slug, err)
	}
	if d.Outcome != access.Granted {
		return server.NotFound(msgWorkspaceNotFound)
	}
	return nil
}

// readableSessions builds the conditions, joined by AND, that select the
// sessions the caller may read and the filter f selects. They follow the
// session visibility rule (see canRead): the admin token reads every
// session; any other caller the sessions it created and those whose
// workspace, or stored parent, is in its readable set, the sessions of
// removed forks included. A bound token reads only the sessions whose
// workspace its binding covers, its own included. The readable set's slugs
// go in as one JSON array, whatever their number.
func (c *Component) readableSessions(ctx context.Context, caller access.Caller, f listFilter) ([]string, []any, error) {
	var conds []string
	var args []any
	set, err := c.deps.Access.Readable(ctx, caller)
	if err != nil {
		return nil, nil, fmt.Errorf("read the readable set: %w", err)
	}
	if !set.All {
		slugs, err := jsonArray(slices.Sorted(maps.Keys(set.Entries)))
		if err != nil {
			return nil, nil, err
		}
		cond := `workspace_slug IN (SELECT value FROM json_each(?)) OR parent_slug IN (SELECT value FROM json_each(?))`
		args = append(args, slugs, slugs)
		if caller.UserID != "" {
			cond = `user_id = ? OR ` + cond
			args = append([]any{caller.UserID}, args...)
		}
		conds = append(conds, `(`+cond+`)`)
	}
	switch {
	case f.workspace != "" && f.includeForks:
		conds, args = append(conds, `(workspace_slug = ? OR parent_slug = ?)`), append(args, f.workspace, f.workspace)
	case f.workspace != "":
		conds, args = append(conds, `workspace_slug = ?`), append(args, f.workspace)
	}
	for _, eq := range []struct{ column, value string }{
		{"run_id", f.runID}, {"status", f.status}, {"credential_type", f.credentialType},
	} {
		if eq.value != "" {
			conds, args = append(conds, eq.column+` = ?`), append(args, eq.value)
		}
	}
	if f.since != "" {
		conds, args = append(conds, `started_at >= ?`), append(args, f.since)
	}
	if f.until != "" {
		conds, args = append(conds, `started_at < ?`), append(args, f.until)
	}
	if c.deps.Coverage == nil {
		return conds, args, nil
	}
	covered, bound, err := c.coveredWorkspaces(ctx, caller, conds, args)
	if err != nil || !bound {
		return conds, args, err
	}
	slugs, err := jsonArray(covered)
	if err != nil {
		return nil, nil, err
	}
	return append(conds, `workspace_slug IN (SELECT value FROM json_each(?))`), append(args, slugs), nil
}

// coveredWorkspaces asks the coverage, for each workspace of the sessions
// conds selects, whether caller is bound and whether its binding covers
// it, and returns the covered ones. bound is false, and covered nil, for a
// caller that is not bound, and for one that conds selects no session for.
func (c *Component) coveredWorkspaces(ctx context.Context, caller access.Caller, conds []string, args []any) (covered []string, bound bool, err error) {
	var slugs []string
	err = eachRow(ctx, c.deps.Store.Main(), func(scan func(...any) error) error {
		var slug string
		if err := scan(&slug); err != nil {
			return err
		}
		slugs = append(slugs, slug)
		return nil
	}, `SELECT DISTINCT workspace_slug FROM sessions`+where(conds), args...)
	if err != nil {
		return nil, false, fmt.Errorf("read the workspaces of the listed sessions: %w", err)
	}
	covered = []string{}
	for _, slug := range slugs {
		b, cov, err := c.deps.Coverage.Covers(ctx, caller, slug)
		if err != nil {
			return nil, false, fmt.Errorf("ask the coverage of workspace %s: %w", slug, err)
		}
		if !b {
			return nil, false, nil
		}
		if cov {
			covered = append(covered, slug)
		}
	}
	return covered, len(slugs) > 0, nil
}

// listPositions reads up to limit positions of the sessions conds selects,
// ordered by (started_at, id), ascending or descending, after the position
// after when continuing.
func (c *Component) listPositions(ctx context.Context, conds []string, args []any, ascending, continuing bool, after listPosition, limit int) ([]listPosition, error) {
	cmp, dir := "<", "DESC"
	if ascending {
		cmp, dir = ">", "ASC"
	}
	if continuing {
		conds = append(slices.Clip(conds), `(started_at `+cmp+` ? OR started_at = ? AND id `+cmp+` ?)`)
		args = append(slices.Clip(args), after.StartedAt, after.StartedAt, after.ID)
	}
	var out []listPosition
	err := eachRow(ctx, c.deps.Store.Main(), func(scan func(...any) error) error {
		var p listPosition
		if err := scan(&p.ID, &p.StartedAt); err != nil {
			return err
		}
		out = append(out, p)
		return nil
	}, `SELECT id, started_at FROM sessions`+where(conds)+` ORDER BY started_at `+dir+`, id `+dir+` LIMIT ?`, append(slices.Clip(args), limit)...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out, nil
}

// where is the WHERE clause joining conds with AND, or "" for none.
func where(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return ` WHERE ` + strings.Join(conds, ` AND `)
}

// jsonArray is slugs as one JSON array, for json_each.
func jsonArray(slugs []string) (string, error) {
	if slugs == nil {
		slugs = []string{}
	}
	b, err := json.Marshal(slugs)
	if err != nil {
		return "", fmt.Errorf("encode the workspace slugs: %w", err)
	}
	return string(b), nil
}
