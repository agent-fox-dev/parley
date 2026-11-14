package fork

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// scopeRead is the PAT scope the fork list needs; a PAT that holds
// workspaces:write satisfies it too.
const scopeRead = "workspaces:read"

// listRoute is GET /workspaces/:slug/forks. Reading the workspace is enough
// to list its forks, in every status of the workspace.
func (c *Component) listRoute() server.Route {
	return server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/forks", Scope: scopeRead,
		Marks: map[string]string{access.MarkNeed: string(access.NeedRead)}, Handler: c.list}
}

// list answers a page of the forks of the route's workspace that the caller
// can read and the query selects, newest first, each as its workspace
// object with the caller's level on it. The forks are cut to the caller's
// readable set, which for a PAT bound to a workspace is its binding's
// covered set, so that the list never shows a fork that then answers 404.
// owner=me keeps the forks of the caller's user, none for the admin token,
// which acts for no user; session_id keeps the forks created in that
// session. A fork, and a workspace without forks, list nothing.
func (c *Component) list(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	var f ListFilter
	owner, err := oneParam(query, "owner", func(v string) bool { return v == "me" }, "me")
	if err != nil {
		return err
	}
	if f.SessionID, err = oneParam(query, "session_id", func(v string) bool { _, err := uuid.Parse(v); return err == nil }, "a UUID"); err != nil {
		return err
	}
	var pos Position
	if ok, err := page.After(&pos); err != nil {
		return err
	} else if ok {
		f.After = &pos
	}
	caller, ok := server.CallerOf(ctx)
	if !ok {
		return server.Internal("the fork list found no caller")
	}
	if owner != nil {
		if caller.UserID == "" {
			return server.WritePage(w, page, []workspace.Object{}, nil)
		}
		f.OwnerID = &caller.UserID
	}
	slug := server.Param(r, "slug")
	set, err := c.deps.Readable.Readable(ctx, access.CallerFrom(caller))
	if err != nil {
		return fmt.Errorf("list the forks of %q: %w", slug, err)
	}
	if !set.All {
		f.Slugs = slices.AppendSeq(make([]string, 0, len(set.Entries)), maps.Keys(set.Entries))
	}
	f.Limit = page.Limit + 1
	forks, err := ListOf(ctx, c.deps.Store.Main(), slug, f)
	if err != nil {
		return err
	}
	items := make([]workspace.Object, 0, len(forks))
	created := make(map[string]int64, len(forks))
	for _, fk := range forks {
		obj, err := c.deps.Workspaces.RenderWorkspace(ctx, fk.Workspace, set.Level(fk.Slug))
		if err != nil {
			return err
		}
		items, created[fk.Slug] = append(items, obj), fk.CreatedAt.UnixNano()
	}
	return server.WritePage(w, page, items, func(o workspace.Object) any { return Position{created[o.Slug], o.Slug} })
}

// oneParam reads the query parameter name: nil when it is absent, its value
// when it is given once and valid accepts it, and otherwise 400
// invalid_request saying it must be want, rather than the parameter
// ignored and the list longer than asked.
func oneParam(query url.Values, name string, valid func(string) bool, want string) (*string, error) {
	values, ok := query[name]
	if !ok {
		return nil, nil
	}
	if len(values) == 1 && valid(values[0]) {
		return new(strings.ToLower(values[0])), nil
	}
	return nil, server.InvalidRequest(fmt.Sprintf("%s must be given once, as %s", name, want))
}
