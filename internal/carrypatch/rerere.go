package carrypatch

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The scopes of the remembered-resolution routes: the resolutions are the
// workspace's own data, not the patch list's.
const (
	scopeWorkspacesRead  = "workspaces:read"
	scopeWorkspacesWrite = "workspaces:write"
)

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
// list, a read route that serves in every state of the workspace, and the
// forget, a write route whose pathspec is the rest of the path, slashes
// and all, and reaches the handler empty too, so that the handler refuses
// it.
func (c *Component) rerereRoutes() []server.Route {
	list := Route(ReadRoute, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/rerere", Handler: c.listRerereRoute})
	list.Scope = scopeWorkspacesRead
	forget := Route(WriteRoute, server.Route{Method: http.MethodDelete, Pattern: "/workspaces/:slug/rerere/*pathspec",
		EmptyCatchAll: true, Handler: c.forgetRerereRoute})
	forget.Scope = scopeWorkspacesWrite
	return []server.Route{list, forget}
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

// forgetRerereRoute answers DELETE /workspaces/:slug/rerere/*pathspec: it
// removes the entry whose id is the pathspec, or else every entry whose
// path is the pathspec, byte for byte, and answers 204, or 404 when none
// is. An empty pathspec, or one git would read as an option, is refused
// before the guard and before anything is read. The pathspec is only
// compared with the entries; each selected entry is removed by its own id,
// so that no caller's value names a file. The forget writes no event and
// queues no rebuild: the next rebuild that meets the conflict records it
// again, unresolved.
func (c *Component) forgetRerereRoute(w http.ResponseWriter, r *http.Request) error {
	pathspec := server.Param(r, "pathspec")
	if pathspec == "" {
		return server.InvalidRequest("the pathspec is empty; give the id or the path of a remembered resolution")
	}
	if strings.HasPrefix(pathspec, "-") {
		return server.InvalidRequest("the pathspec must not start with '-'")
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	entries, err := c.RerereEntries(r.Context(), ws.Slug)
	if err != nil {
		return err
	}
	selected := slices.DeleteFunc(slices.Clone(entries), func(e RerereEntry) bool { return e.ID != pathspec })
	if len(selected) == 0 {
		selected = slices.DeleteFunc(entries, func(e RerereEntry) bool { return e.Path != pathspec })
	}
	if len(selected) == 0 {
		return server.NotFound("resolution not found")
	}
	// What has begun is finished when the caller leaves: git's record of
	// an entry is not left removed while the entry stays.
	if err := c.forgetRerere(context.WithoutCancel(r.Context()), ws.Slug, selected); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// forgetRerere removes the entries of the workspace slug: git's record of
// each conflict first, then the stored entries in one transaction, so that
// a forget cut short in between leaves an entry without a resolution,
// listed unresolved and forgotten by the next forget, and never a
// resolution git would replay without an entry that shows it. An entry
// already gone, from the store or from git, counts as removed: of two
// forgets of one entry at once, neither fails.
func (c *Component) forgetRerere(ctx context.Context, slug string, entries []RerereEntry) error {
	if c.d.Store == nil || c.d.RepoPath == nil {
		return errNotBound
	}
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(slug))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No copy, so git holds no resolution of the workspace.
	case err != nil:
		return err
	default:
		for _, e := range entries {
			if err := RunGit(ctx, 0, gitrepo.Credential{}, func(context.Context, time.Duration) error {
				return repo.ForgetRerere(e.ID)
			}); err != nil {
				return err
			}
		}
	}
	return c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, e := range entries {
			if _, err := RemoveRerereEntry(ctx, tx, slug, e.ID); err != nil {
				return err
			}
		}
		return nil
	})
}
