package carrypatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
)

// EventPatchReorder is the event of a patch list renumbered in one step.
const EventPatchReorder = "parley.patch.reorder"

// reorderAnswer is the answer of a reorder: every patch that is not
// deleted, in the new order, on one page.
type reorderAnswer struct {
	Items      []Object `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}

// reorderRoute answers POST /workspaces/:slug/patches/reorder: it gives the
// patches that are not deleted the positions 1 to N in the order of the
// body's patch_ids, which names each of them once, and answers 200 with
// them in that order. The body's shape is checked before the guard, so
// that an invalid body is answered as such on any workspace; the list is
// then read, checked and renumbered in one store transaction, with its
// event.
func (c *Component) reorderRoute(w http.ResponseWriter, r *http.Request) error {
	ids, err := decodeReorderBody(r)
	if err != nil {
		return err
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	by, err := actorOf(r)
	if err != nil {
		return err
	}
	order, err := c.reorder(r.Context(), ws.Slug, ids, by)
	if err != nil {
		return err
	}
	// The items are the list's own, as GET /workspaces/:slug/patches shows
	// them.
	items := make([]Object, len(order))
	for i, p := range order {
		items[i] = ObjectOf(p, "")
	}
	server.WriteJSON(w, http.StatusOK, reorderAnswer{Items: items})
	return nil
}

// decodeReorderBody reads the body, a JSON object whose patch_ids is an
// array of strings, and returns those strings.
func decodeReorderBody(r *http.Request) ([]string, error) {
	var members map[string]json.RawMessage
	if err := server.DecodeJSON(r, &members); err != nil {
		return nil, err
	}
	refused := server.InvalidRequest("patch_ids must be an array of strings")
	v := member(members, "patch_ids")
	var raw []json.RawMessage
	if v == nil || v[0] != '[' || json.Unmarshal(v, &raw) != nil {
		return nil, refused
	}
	ids := make([]string, len(raw))
	for i, e := range raw {
		if e[0] != '"' || json.Unmarshal(e, &ids[i]) != nil {
			return nil, refused
		}
	}
	return ids, nil
}

// reorder renumbers the patches of the workspace slug that are not deleted
// in the order of ids, in one store transaction with one
// parley.patch.reorder, and returns them as they then stand. The list ids
// must name every such patch once: the first of an id given twice, an id
// of no patch of the workspace, the id of a deleted patch and a list of
// the wrong length, checked in that order, is 400 invalid_request and
// changes nothing. The event is written also when the order is the one the
// list had, since the caller asked for it; a workspace with no patch that
// is not deleted takes the empty list and writes none, having no patch to
// name. An event that cannot be written rolls the change back.
func (c *Component) reorder(ctx context.Context, slug string, ids []string, by actor) ([]Patch, error) {
	if c.d.Events == nil {
		return nil, errNotBound
	}
	var out []Patch
	err := c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		live, err := ListLivePatches(ctx, tx, slug)
		if err != nil {
			return err
		}
		order, err := checkOrder(ctx, tx, slug, ids, live)
		if err != nil {
			return err
		}
		if len(order) == 0 {
			out = []Patch{}
			return nil
		}
		if out, err = renumber(ctx, tx, order, c.now().UTC().Truncate(time.Second)); err != nil {
			return err
		}
		named := make([]string, len(out))
		for i, p := range out {
			named[i] = p.ID
		}
		_, err = c.d.Events.Emit(ctx, tx, event.Input{
			Type: EventPatchReorder, Workspace: &slug, ActorType: by.typ, ActorID: by.id,
			ResourceType: "patch", ResourceID: named[0],
			Metadata: map[string]any{"patch_ids": named},
		})
		return err
	})
	return out, err
}

// checkOrder is the patches live of the workspace slug, those that are not
// deleted, in the order of ids, or the 400 invalid_request that names the
// first reason ids is not that list. An id is compared in its canonical
// form, so that one written in capitals is the patch it names.
func checkOrder(ctx context.Context, q Querier, slug string, ids []string, live []Patch) ([]Patch, error) {
	canonical := make([]string, len(ids))
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		canonical[i] = id
		if u, err := uuid.Parse(id); err == nil {
			canonical[i] = u.String()
		}
		if seen[canonical[i]] {
			return nil, server.InvalidRequest(fmt.Sprintf("patch_ids names %s more than once", id))
		}
		seen[canonical[i]] = true
	}
	byID := make(map[string]Patch, len(live))
	for _, p := range live {
		byID[p.ID] = p
	}
	order := make([]Patch, 0, len(ids))
	var deleted string
	for i, id := range canonical {
		if p, ok := byID[id]; ok {
			order = append(order, p)
			continue
		}
		_, found, err := ReadPatch(ctx, q, slug, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, server.InvalidRequest(fmt.Sprintf("patch_ids names %s, which is no patch of %s", ids[i], slug))
		}
		if deleted == "" {
			deleted = ids[i]
		}
	}
	if deleted != "" {
		return nil, server.InvalidRequest(fmt.Sprintf("patch_ids names %s, a deleted patch; restore it before ordering it", deleted))
	}
	if len(order) != len(live) {
		return nil, server.InvalidRequest(fmt.Sprintf("patch_ids names %d patches; %s has %d that are not deleted, and each must be named once",
			len(ids), slug, len(live)))
	}
	return order, nil
}
