package secret

import (
	"database/sql"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// varScopeWrite is the PAT scope a change to variables needs.
const varScopeWrite = "vars:write"

// varCollections are the collection path of each variable scope and the
// marks of its changes: the scope the scope check resolves, so that the
// user and org scopes answer exactly as the secret routes do, and on a
// workspace the need write, so that its owner and the admin token change
// its variables.
var varCollections = []struct {
	pattern string
	marks   map[string]string
}{
	{"/user/vars", map[string]string{MarkScope: string(ScopeUser)}},
	{"/orgs/:slug/vars", map[string]string{MarkScope: string(ScopeOrg)}},
	{"/workspaces/:slug/vars", map[string]string{MarkScope: string(ScopeWorkspace), access.MarkNeed: string(access.NeedWrite)}},
}

// VarRoutes returns the variable routes of the REST API: a create on each
// collection. Variables are a second collection of the component, beside
// its secrets: each route carries its scope mark, so that ScopeCheck
// resolves its scope before the handler runs, and the PAT scope it needs. A
// create is a create route, so that a retried request with its
// Idempotency-Key creates nothing twice. No route needs an encryption key.
func (c *Component) VarRoutes() []server.Route {
	routes := make([]server.Route, 0, len(varCollections))
	for _, col := range varCollections {
		routes = append(routes,
			server.Route{Method: http.MethodPost, Pattern: col.pattern, Scope: varScopeWrite, Marks: col.marks, Handler: c.createVars, Create: true},
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

// varItemsOf are the stored variables as an answer gives them, in their
// order.
func varItemsOf(stored []Variable) []varItem {
	items := make([]varItem, len(stored))
	for i, v := range stored {
		items[i] = varItem{Key: v.Key, Value: v.Value, CreatedAt: timefmt.Time(v.CreatedAt), UpdatedAt: timefmt.Time(v.UpdatedAt)}
	}
	return items
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
