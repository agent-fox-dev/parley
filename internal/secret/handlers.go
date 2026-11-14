package secret

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The PAT scopes of the secret routes: reading a list needs secrets:read,
// which secrets:write implies.
const scopeRead = "secrets:read"

// collections are the collection path of each secret scope and the marks
// its routes carry: the scope the scope check resolves, and on a workspace
// the need owner, so that only its owner and the admin token reach its
// secrets.
var collections = []struct {
	pattern string
	marks   map[string]string
}{
	{"/user/secrets", map[string]string{MarkScope: string(ScopeUser)}},
	{"/orgs/:slug/secrets", map[string]string{MarkScope: string(ScopeOrg)}},
	{"/workspaces/:slug/secrets", map[string]string{MarkScope: string(ScopeWorkspace), access.MarkNeed: string(access.NeedOwner)}},
}

// Routes returns the secret routes of the REST API. Each carries its scope
// mark, so that ScopeCheck resolves its scope before the handler runs, and
// the PAT scope it needs.
func (c *Component) Routes() []server.Route {
	var routes []server.Route
	for _, col := range collections {
		routes = append(routes, server.Route{
			Method: http.MethodGet, Pattern: col.pattern, Scope: scopeRead, Marks: col.marks, Handler: c.list,
		})
	}
	return routes
}

// item is a secret as a list answers it: its key and its times, never its
// value nor its owner.
type item struct {
	Key       string       `json:"key"`
	CreatedAt timefmt.Time `json:"created_at"`
	UpdatedAt timefmt.Time `json:"updated_at"`
}

func itemOf(it Item) item {
	return item{Key: it.Key, CreatedAt: timefmt.Time(it.CreatedAt), UpdatedAt: timefmt.Time(it.UpdatedAt)}
}

// list answers GET on a secrets collection with a page of the scope's
// secrets, ordered by key with A to Z lowered. It reads no value, so it
// lists every secret without a key and those no key opens.
func (c *Component) list(w http.ResponseWriter, r *http.Request) error {
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
	stored, err := c.ListPage(r.Context(), t.scope, t.id, after, page.Limit+1)
	if err != nil {
		return err
	}
	items := make([]item, len(stored))
	for i, it := range stored {
		items[i] = itemOf(it)
	}
	return server.WritePage(w, page, items, func(it item) any { return asciiLower(it.Key) })
}
