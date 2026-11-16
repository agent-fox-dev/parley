package carrypatch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/server"
)

// ReplacedRefPrefix is the prefix of the backup ref of a patch branch:
// refs/parley/replaced/<branch> holds the tip the hub discarded when it
// replaced the branch.
const ReplacedRefPrefix = "refs/parley/replaced/"

// listPosition is a patch's place in the list's one total order, the
// keyset a cursor carries: the patches that are not deleted by position,
// then, when the list includes them, the deleted ones by deleted_at
// descending and id ascending. One order across both groups lets a cursor
// continue over the boundary between them.
type listPosition struct {
	Deleted   bool   `json:"d,omitempty"`
	Position  int    `json:"p,omitempty"`
	DeletedAt int64  `json:"t,omitempty"`
	ID        string `json:"i,omitempty"`
}

// positionOf is p's place in the list's order.
func positionOf(p Patch) listPosition {
	if p.Position != nil {
		return listPosition{Position: *p.Position}
	}
	pos := listPosition{Deleted: true, ID: p.ID}
	if p.DeletedAt != nil {
		pos.DeletedAt = p.DeletedAt.UnixNano()
	}
	return pos
}

// listRoute answers GET /workspaces/:slug/patches: a page of the patches
// of the workspace that are not deleted, in position order, followed by
// the deleted ones when include_deleted is true. The query is checked
// before the guard, so that an invalid query is answered as such on any
// workspace; the guard's read form serves in every state of the workspace,
// from the stored list.
func (c *Component) listRoute(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	withDeleted, err := includeDeleted(r.URL.Query())
	if err != nil {
		return err
	}
	var after listPosition
	paged, err := page.After(&after)
	if err != nil {
		return err
	}
	if paged && after.Deleted && !withDeleted {
		return server.InvalidRequest("cursor is malformed or belongs to another query")
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	var from *listPosition
	if paged {
		from = &after
	}
	patches, err := listPage(r.Context(), c.d.Store.Main(), ws.Slug, withDeleted, from, page.Limit+1)
	if err != nil {
		return err
	}
	items := make([]Object, 0, len(patches))
	keys := make(map[string]listPosition, len(patches))
	for _, p := range patches {
		items = append(items, ObjectOf(p, ""))
		keys[p.ID] = positionOf(p)
	}
	return server.WritePage(w, page, items, func(o Object) any { return keys[o.ID] })
}

// includeDeleted reads the query parameter include_deleted: absent or
// false leaves the deleted patches out, true adds them; anything else,
// the empty value and a repeated parameter included, is 400
// invalid_request rather than a guess.
func includeDeleted(query url.Values) (bool, error) {
	values, ok := query["include_deleted"]
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
	return false, server.InvalidRequest("include_deleted must be given once, as true or false")
}

// listPage reads up to limit patches of the workspace after the position
// from (nil for the start) in the list's order: those that are not
// deleted, then, when withDeleted, the deleted ones.
func listPage(ctx context.Context, q Querier, workspace string, withDeleted bool, from *listPosition, limit int) ([]Patch, error) {
	var out []Patch
	if from == nil || !from.Deleted {
		after := 0
		if from != nil {
			after = from.Position
		}
		live, err := listPatches(ctx, q, `WHERE workspace_slug = ? AND status <> 'deleted' AND position > ?
ORDER BY position LIMIT ?`, workspace, after, limit)
		if err != nil {
			return nil, err
		}
		out = live
		from = nil
	}
	if !withDeleted || len(out) >= limit {
		return out, nil
	}
	where, args := `WHERE workspace_slug = ? AND status = 'deleted'`, []any{workspace}
	if from != nil {
		where += ` AND (deleted_at < ? OR (deleted_at = ? AND id > ?))`
		args = append(args, from.DeletedAt, from.DeletedAt, from.ID)
	}
	deleted, err := listPatches(ctx, q, where+` ORDER BY deleted_at DESC, id LIMIT ?`, append(args, limit-len(out))...)
	if err != nil {
		return nil, err
	}
	return append(out, deleted...), nil
}

// getRoute answers GET /workspaces/:slug/patches/:id: the patch, deleted
// or not, with replaced_sha when its backup ref exists in the copy. An id
// that is not a UUID is answered as an unknown one, and so is the id of
// another workspace's patch.
func (c *Component) getRoute(w http.ResponseWriter, r *http.Request) error {
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(server.Param(r, "id"))
	if err != nil {
		return errPatchNotFound()
	}
	p, found, err := ReadPatch(r.Context(), c.d.Store.Main(), ws.Slug, id.String())
	if err != nil {
		return fmt.Errorf("read the patch %s of %s: %w", id, ws.Slug, err)
	}
	if !found {
		return errPatchNotFound()
	}
	server.WriteJSON(w, http.StatusOK, ObjectOf(p, c.replacedSHA(r.Context(), ws.Slug, p.BranchName)))
	return nil
}

// errPatchNotFound is the 404 of a patch the workspace does not have.
func errPatchNotFound() error { return server.NotFound("patch not found") }

// replacedSHA is the commit the backup ref of branch reaches in the copy of
// the workspace slug, "" when there is no such ref, no copy, or git fails:
// the member is extra information, and a read of the patch list answers
// from stored state whatever the copy's condition.
func (c *Component) replacedSHA(ctx context.Context, slug, branch string) string {
	if c.d.RepoPath == nil {
		return ""
	}
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(slug))
	if err != nil {
		return ""
	}
	ref, found, err := repo.LookupRef(ctx, ReplacedRefPrefix+branch)
	if err != nil || !found {
		return ""
	}
	return ref.Commit
}
