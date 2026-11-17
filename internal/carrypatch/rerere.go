package carrypatch

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// scopeWorkspacesRead is the scope of the remembered-resolution list: the
// resolutions are the workspace's own data, not the patch list's.
const scopeWorkspacesRead = "workspaces:read"

// rerereItem is a remembered-resolution entry as the list answers it.
type rerereItem struct {
	ID         string       `json:"id"`
	Path       string       `json:"path"`
	Resolved   bool         `json:"resolved"`
	RecordedAt timefmt.Time `json:"recorded_at"`

	// pos is the entry's place in the list's order, for the cursor.
	pos rererePosition
}

// rererePosition is an entry's place in the list's order, the keyset a
// cursor carries: recorded_at descending, then id ascending, which makes
// the order total.
type rererePosition struct {
	At int64  `json:"t"`
	ID string `json:"i"`
}

// positionOfEntry is e's place in the list's order.
func positionOfEntry(e RerereEntry) rererePosition {
	return rererePosition{At: e.RecordedAt.UnixNano(), ID: e.ID}
}

// compareEntries orders two entries as the list does.
func compareEntries(a, b rererePosition) int {
	return cmp.Or(cmp.Compare(b.At, a.At), strings.Compare(a.ID, b.ID))
}

// rerereRoutes are the routes of a workspace's remembered resolutions: the
// list, a read route that serves in every state of the workspace.
func (c *Component) rerereRoutes() []server.Route {
	list := Route(ReadRoute, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/rerere", Handler: c.listRerereRoute})
	list.Scope = scopeWorkspacesRead
	return []server.Route{list}
}

// listRerereRoute answers GET /workspaces/:slug/rerere: a page of the
// workspace's remembered-resolution entries, resolved or not, newest
// first. The query is checked before the guard, so that an invalid one is
// answered as such on any workspace. The entries are few, one per conflict
// a rebuild met, so the page is cut from all of them in memory. The list
// reads the store and the copy's rr-cache and starts no git process.
func (c *Component) listRerereRoute(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after rererePosition
	paged, err := page.After(&after)
	if err != nil {
		return err
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	entries, err := c.RerereEntries(r.Context(), ws.Slug)
	if err != nil {
		return err
	}
	slices.SortFunc(entries, func(a, b RerereEntry) int { return compareEntries(positionOfEntry(a), positionOfEntry(b)) })
	if paged {
		entries = slices.DeleteFunc(entries, func(e RerereEntry) bool { return compareEntries(positionOfEntry(e), after) <= 0 })
	}
	if len(entries) > page.Limit+1 {
		entries = entries[:page.Limit+1]
	}
	items := make([]rerereItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, rerereItem{ID: e.ID, Path: e.Path, Resolved: e.Resolved, RecordedAt: timefmt.Time(e.RecordedAt), pos: positionOfEntry(e)})
	}
	return server.WritePage(w, page, items, func(it rerereItem) any { return it.pos })
}
