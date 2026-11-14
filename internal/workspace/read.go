package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// scopeRead is the PAT scope the workspace reads need; a PAT that holds
// workspaces:write satisfies it too.
const scopeRead = "workspaces:read"

// readRoute is GET /workspaces/:slug. The scope check and then the access
// check run before the handler, so that a PAT without the scope is refused
// before any lookup and a caller who cannot read the workspace gets the same
// 404 as for a slug no workspace has. It answers in every status and for
// both kinds.
func (s *Service) readRoute() server.Route {
	return server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug", Scope: scopeRead,
		Marks: map[string]string{access.MarkNeed: string(access.NeedRead)}, Handler: s.read}
}

// read answers the workspace object of the route's workspace, with the
// caller's level as the access check decided it.
func (s *Service) read(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the workspace read found no access decision")
	}
	rec, found, err := BySlug(r.Context(), s.deps.Store.Main(), server.Param(r, "slug"))
	if err != nil {
		return err
	}
	if !found {
		// Deleted between the access check and this read: the answer is the
		// one the check gives for a slug no workspace has.
		return server.NotFound(ErrNotFound.Error())
	}
	obj, err := s.RenderWorkspace(r.Context(), rec, d.Level)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, obj)
	return nil
}

// listRoute is GET /workspaces: the workspaces the caller can read, newest
// first, in pages. It names no workspace, so no access check runs before
// the handler; the handler confines the list to the caller's readable set,
// which for a PAT bound to a workspace is already its binding's covered
// set, so that the binding guard, which lets such a PAT through to this
// route, has nothing more to cut.
func (s *Service) listRoute() server.Route {
	return server.Route{Method: http.MethodGet, Pattern: "/workspaces", Scope: scopeRead, Handler: s.list}
}

// listFilter is what a list selects beside the readable set: archived
// workspaces only when archived is set, forks only when forks is set, and
// with parent only the forks of the repository workspace *parent.
type listFilter struct {
	archived, forks bool
	parent          *string
}

// listPosition is a workspace's place in the list's order, which the
// cursor carries: its created_at in UnixNano, then its slug among those
// created at the same instant.
type listPosition struct {
	CreatedAt int64  `json:"created_at"`
	Slug      string `json:"slug"`
}

// list answers a page of the workspaces the caller can read that the query
// selects, each with the caller's level on it.
func (s *Service) list(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	var f listFilter
	if f.archived, err = boolParam(query, "include_archived"); err != nil {
		return err
	}
	if f.forks, err = boolParam(query, "include_forks"); err != nil {
		return err
	}
	if query.Has("parent") {
		f.parent = new(query.Get("parent"))
	}
	var after *listPosition
	var pos listPosition
	if ok, err := page.After(&pos); err != nil {
		return err
	} else if ok {
		after = &pos
	}
	caller, ok := server.CallerOf(r.Context())
	if !ok {
		return server.Internal("the workspace list found no caller")
	}
	set, err := s.deps.Access.Readable(r.Context(), access.CallerFrom(caller))
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	var recs []Workspace
	// The forks of a parent the caller cannot read are not listed, even
	// those the caller could read on their own.
	if f.parent == nil || set.Contains(*f.parent) {
		if recs, err = s.readList(r.Context(), set, f, after, page.Limit+1); err != nil {
			return err
		}
	}
	items := make([]Object, 0, len(recs))
	created := make(map[string]int64, len(recs))
	// A page's workspaces mostly share a few orgs: each is read once.
	orgs := map[string]identity.Org{}
	for _, rec := range recs {
		org, ok := orgs[rec.OrgID]
		if !ok {
			if org, err = identity.OrgByID(r.Context(), s.deps.Store.Main(), rec.OrgID); err != nil {
				return fmt.Errorf("list workspaces: the org of %q: %w", rec.Slug, err)
			}
			orgs[rec.OrgID] = org
		}
		obj, err := s.render(r.Context(), rec, set.Level(rec.Slug), org)
		if err != nil {
			return err
		}
		items = append(items, obj)
		created[rec.Slug] = rec.CreatedAt.UnixNano()
	}
	return server.WritePage(w, page, items, func(o Object) any { return listPosition{created[o.Slug], o.Slug} })
}

// boolParam reads the query parameter name: absent or false is false, true
// is true, and any other value is refused rather than ignored, so that a
// misspelt true does not silently list less. A parameter given more than
// once is refused too, rather than one of its values chosen.
func boolParam(query url.Values, name string) (bool, error) {
	values, ok := query[name]
	if !ok {
		return false, nil
	}
	if len(values) == 1 {
		switch values[0] {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, server.InvalidRequest(name + " must be given once, as true or false")
}

// readList reads up to limit workspaces of the readable set set that f
// selects, after the position after when it is not nil, in the list's
// order. The set, the filter, the order and the position are all in the
// query, so a page reads only its own rows; the set's slugs go in as one
// JSON array, whatever their number.
func (s *Service) readList(ctx context.Context, set access.ReadableSet, f listFilter, after *listPosition, limit int) ([]Workspace, error) {
	var conds []string
	var args []any
	if !set.All {
		slugs := slices.AppendSeq(make([]string, 0, len(set.Entries)), maps.Keys(set.Entries))
		b, err := json.Marshal(slugs)
		if err != nil {
			return nil, err
		}
		conds, args = append(conds, `slug IN (SELECT value FROM json_each(?))`), append(args, string(b))
	}
	if !f.archived {
		conds, args = append(conds, `status <> ?`), append(args, string(StatusArchived))
	}
	switch {
	case f.parent != nil:
		// A parent that is itself a fork, or has no record, has no forks to
		// list.
		conds = append(conds, `kind = ? AND parent_slug = ? AND EXISTS (SELECT 1 FROM workspaces p WHERE p.slug = ? AND p.kind = ?)`)
		args = append(args, access.KindFork, *f.parent, *f.parent, access.KindRepository)
	case !f.forks:
		conds, args = append(conds, `kind = ?`), append(args, access.KindRepository)
	}
	if after != nil {
		conds = append(conds, `(created_at < ? OR (created_at = ? AND slug > ?))`)
		args = append(args, after.CreatedAt, after.CreatedAt, after.Slug)
	}
	q := `SELECT ` + workspaceColumns + ` FROM workspaces`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	rows, err := s.deps.Store.Main().QueryContext(ctx, q+` ORDER BY created_at DESC, slug ASC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Workspace
	for rows.Next() {
		rec, err := scanWorkspace(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list workspaces: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return out, nil
}
