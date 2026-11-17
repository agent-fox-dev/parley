package secret

import (
	"database/sql"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The PAT scopes of the variable routes: reading a list needs vars:read,
// which vars:write implies; a change needs vars:write.
const (
	varScopeRead  = "vars:read"
	varScopeWrite = "vars:write"
)

// varCollections are the collection path of each variable scope and the
// scope the scope check resolves for its routes, so that the user and org
// scopes answer exactly as the secret routes do.
var varCollections = []struct {
	pattern string
	scope   Scope
}{
	{"/user/vars", ScopeUser},
	{"/orgs/:slug/vars", ScopeOrg},
	{"/workspaces/:slug/vars", ScopeWorkspace},
}

// varMarks are the marks of a variable route of scope: its scope mark and,
// on a workspace, what the route needs of its caller there: read for a
// list, so that every member reads a workspace's variables, and write for a
// change, so that only its owner and the admin token change them.
func varMarks(scope Scope, need access.Need) map[string]string {
	marks := map[string]string{MarkScope: string(scope)}
	if scope == ScopeWorkspace {
		marks[access.MarkNeed] = string(need)
	}
	return marks
}

// VarRoutes returns the variable routes of the REST API: a list and a
// create on each collection, an update and a delete on each of its
// variables. Variables are a second collection of the component, beside
// its secrets: each route carries its scope mark, so that ScopeCheck
// resolves its scope before the handler runs, and the PAT scope it needs. A
// create is a create route, so that a retried request with its
// Idempotency-Key creates nothing twice. No route needs an encryption key.
// A variable has no GET of its own: the router answers one 405 with the
// methods its path has.
func (c *Component) VarRoutes() []server.Route {
	routes := make([]server.Route, 0, 4*len(varCollections))
	for _, col := range varCollections {
		read, write := varMarks(col.scope, access.NeedRead), varMarks(col.scope, access.NeedWrite)
		one := col.pattern + "/:key"
		routes = append(routes,
			server.Route{Method: http.MethodGet, Pattern: col.pattern, Scope: varScopeRead, Marks: read, Handler: c.listVars},
			server.Route{Method: http.MethodPost, Pattern: col.pattern, Scope: varScopeWrite, Marks: write, Handler: c.createVars, Create: true},
			server.Route{Method: http.MethodPatch, Pattern: one, Scope: varScopeWrite, Marks: write, Handler: c.updateVar},
			server.Route{Method: http.MethodDelete, Pattern: one, Scope: varScopeWrite, Marks: write, Handler: c.removeVar},
		)
	}
	return routes
}

// varItem is a variable as the routes answer it: its key, its value and
// its times, never its owner.
type varItem struct {
	Key       string       `json:"key"`
	Value     string       `json:"value"`
	CreatedAt timefmt.Time `json:"created_at"`
	UpdatedAt timefmt.Time `json:"updated_at"`
}

func varItemOf(v Variable) varItem {
	return varItem{Key: v.Key, Value: v.Value, CreatedAt: timefmt.Time(v.CreatedAt), UpdatedAt: timefmt.Time(v.UpdatedAt)}
}

// varItemsOf are the stored variables as an answer gives them, in their
// order.
func varItemsOf(stored []Variable) []varItem {
	items := make([]varItem, len(stored))
	for i, v := range stored {
		items[i] = varItemOf(v)
	}
	return items
}

// listVars answers GET on a variables collection with a page of the
// scope's own variables, with their values, ordered by key with A to Z
// lowered. A fork's scope is its own: it lists none of its parent's.
func (c *Component) listVars(w http.ResponseWriter, r *http.Request) error {
	t, err := targetOf(r)
	if err != nil {
		return err
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after string
	if _, err := page.After(&after); err != nil {
		return err
	}
	stored, err := c.ListVars(r.Context(), t.scope, t.id, after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, varItemsOf(stored), func(it varItem) any { return asciiLower(it.Key) })
}

// createVars answers POST on a variables collection: it stores every entry
// of the body or none, in one transaction, and answers the entries in
// request order. The body is checked first, then the scope's limit, then
// whether it holds a key already, so that a request with several faults is
// always answered with the same one.
func (c *Component) createVars(w http.ResponseWriter, r *http.Request) error {
	t, err := targetOf(r)
	if err != nil {
		return err
	}
	entries, err := decodeEntries(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var stored []Variable
	err = c.change(ctx, func(tx *sql.Tx) error {
		var err error
		stored, err = c.CreateVars(ctx, tx, t.scope, t.id, entries)
		return err
	})
	if err != nil {
		return routeError(err)
	}
	server.WriteJSON(w, http.StatusCreated, struct {
		Items []varItem `json:"items"`
	}{varItemsOf(stored)})
	return nil
}

// updateVar answers PATCH on a variable: it replaces the value of the
// variable whose key matches the path's ignoring case, which keeps the
// casing it was first stored with and its creation time. The body is
// checked before the variable is looked up, so a bad body is answered 400
// for a key the scope does not hold too. The path's key is not checked
// against the key rules: one no variable can have is not found.
func (c *Component) updateVar(w http.ResponseWriter, r *http.Request) error {
	t, err := targetOf(r)
	if err != nil {
		return err
	}
	name := server.Param(r, "key")
	value, err := decodeValue(r, name)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var v Variable
	err = c.change(ctx, func(tx *sql.Tx) error {
		var err error
		v, err = c.UpdateVar(ctx, tx, t.scope, t.id, name, value)
		return err
	})
	if err != nil {
		return routeError(err)
	}
	server.WriteJSON(w, http.StatusOK, varItemOf(v))
	return nil
}

// removeVar answers DELETE on a variable with 204 and no body.
func (c *Component) removeVar(w http.ResponseWriter, r *http.Request) error {
	t, err := targetOf(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = c.change(ctx, func(tx *sql.Tx) error {
		_, err := c.DeleteVar(ctx, tx, t.scope, t.id, server.Param(r, "key"))
		return err
	})
	if err != nil {
		return routeError(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
