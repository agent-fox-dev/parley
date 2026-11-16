package carrypatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
)

// EventPatchUpdate is the event of a patch whose status, position,
// description or upstream pull request URL an update changed.
const EventPatchUpdate = "parley.patch.update"

// updateRequest is the body of PATCH /workspaces/:slug/patches/:id, as
// validated: a member that was absent or null is nil, or 0 for position,
// and leaves its field as it is; an empty description or upstream_pr_url
// clears the field.
type updateRequest struct {
	status      *string
	position    int
	description *string
	upstreamPR  *string
}

// updatableStatuses are the statuses an update may set; deleted is set by
// the delete route alone.
var updatableStatuses = []string{StatusActive, StatusDisabled, StatusMergedUpstream, StatusConflict}

// updateRoute answers PATCH /workspaces/:slug/patches/:id: it changes the
// members of the patch the body names, and answers 200 with the patch as
// it then stands. The body's shape is checked before the guard, so that an
// invalid body is answered as such on any workspace; the patch is then
// read, checked and changed in one store transaction, with its event.
func (c *Component) updateRoute(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeUpdateBody(r)
	if err != nil {
		return err
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(server.Param(r, "id"))
	if err != nil {
		return errPatchNotFound()
	}
	by, err := actorOf(r)
	if err != nil {
		return err
	}
	p, err := c.updatePatch(r.Context(), ws.Slug, id.String(), req, by)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, ObjectOf(p, c.replacedSHA(r.Context(), ws.Slug, p.BranchName)))
	return nil
}

// decodeUpdateBody reads the body, a JSON object: status one of
// updatableStatuses, position a JSON integer of at least 1, description and
// upstream_pr_url strings. A null member is absent; branch_name and the
// members the object does not define are ignored.
func decodeUpdateBody(r *http.Request) (updateRequest, error) {
	var members map[string]json.RawMessage
	if err := server.DecodeJSON(r, &members); err != nil {
		return updateRequest{}, err
	}
	var req updateRequest
	var err error
	if req.status, err = stringMember(members, "status"); err != nil {
		return updateRequest{}, err
	}
	if req.status != nil && !slices.Contains(updatableStatuses, *req.status) {
		return updateRequest{}, server.InvalidRequest(fmt.Sprintf("status %q is not one of %v", *req.status, updatableStatuses))
	}
	// A position too large for any list is still a position: it is refused
	// against the list's length, after the patch's own checks.
	if req.position, err = positionMember(members); err != nil {
		return updateRequest{}, err
	}
	if req.description, err = stringMember(members, "description"); err != nil {
		return updateRequest{}, err
	}
	if req.upstreamPR, err = stringMember(members, "upstream_pr_url"); err != nil {
		return updateRequest{}, err
	}
	return req, nil
}

// updatePatch applies req to the patch id of the workspace slug in one
// store transaction and returns the patch as it then stands. A patch the
// workspace does not have is 404, a deleted one 409 patch_deleted, and a
// position past the patches that are not deleted 400 invalid_request. A
// member equal to the stored value is no change; when nothing changes,
// nothing is written. Otherwise the fields are set, conflict_files is
// emptied when the status becomes anything but conflict, a move renumbers
// the list, and one parley.patch.update names the fields that changed, in
// the order status, position, description, upstream_pr_url; an event that
// cannot be written rolls the change back.
func (c *Component) updatePatch(ctx context.Context, slug, id string, req updateRequest, by actor) (Patch, error) {
	if c.d.Events == nil {
		return Patch{}, errNotBound
	}
	var out Patch
	err := c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		p, found, err := ReadPatch(ctx, tx, slug, id)
		if err != nil {
			return err
		}
		if !found {
			return errPatchNotFound()
		}
		if p.Status == StatusDeleted {
			return server.NewError("patch_deleted", http.StatusConflict,
				fmt.Sprintf("the patch of %s is deleted; restore it before changing it", p.BranchName))
		}
		if req.position != 0 {
			n, err := CountLivePatches(ctx, tx, slug)
			if err != nil {
				return err
			}
			if req.position > n {
				return server.InvalidRequest(fmt.Sprintf("position must be between 1 and %d, the number of patches", n))
			}
		}
		next, changed := p, []string{}
		if req.status != nil && *req.status != p.Status {
			next.Status = *req.status
			if next.Status != StatusConflict {
				next.ConflictFiles = []string{}
			}
			changed = append(changed, "status")
		}
		if req.position != 0 && req.position != *p.Position {
			changed = append(changed, "position")
		}
		if req.description != nil && !sameText(p.Description, *req.description) {
			next.Description = nonEmpty(req.description)
			changed = append(changed, "description")
		}
		if req.upstreamPR != nil && !sameText(p.UpstreamPRURL, *req.upstreamPR) {
			next.UpstreamPRURL = nonEmpty(req.upstreamPR)
			changed = append(changed, "upstream_pr_url")
		}
		if len(changed) == 0 {
			out = p
			return nil
		}
		now := c.now().UTC().Truncate(time.Second)
		next.UpdatedAt = now
		if err := writeFields(ctx, tx, next); err != nil {
			return err
		}
		if req.position != 0 && req.position != *p.Position {
			live, err := ListLivePatches(ctx, tx, slug)
			if err != nil {
				return err
			}
			order, err := renumber(ctx, tx, moveTo(live, id, req.position), now)
			if err != nil {
				return err
			}
			next.Position = order[req.position-1].Position
		}
		meta := map[string]any{"branch_name": p.BranchName, "changed": changed}
		if next.Status != p.Status {
			meta["from"], meta["to"], meta["reason"] = p.Status, next.Status, "api"
		}
		if _, err := c.d.Events.Emit(ctx, tx, event.Input{
			Type: EventPatchUpdate, Workspace: &slug, ActorType: by.typ, ActorID: by.id,
			ResourceType: "patch", ResourceID: p.ID, Metadata: meta,
		}); err != nil {
			return err
		}
		out = next
		return nil
	})
	return out, err
}

// writeFields stores the status, conflict files, text fields and updated_at
// of p.
func writeFields(ctx context.Context, tx *sql.Tx, p Patch) error {
	files, err := json.Marshal(append([]string{}, p.ConflictFiles...))
	if err != nil {
		return fmt.Errorf("update the patch %s of %s: %w", p.BranchName, p.Workspace, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE patches SET status = ?, conflict_files = ?, upstream_pr_url = ?, description = ?, updated_at = ?
WHERE workspace_slug = ? AND id = ?`,
		p.Status, string(files), p.UpstreamPRURL, p.Description, p.UpdatedAt.UnixNano(), p.Workspace, p.ID); err != nil {
		return fmt.Errorf("update the patch %s of %s: %w", p.BranchName, p.Workspace, err)
	}
	return nil
}

// sameText reports whether the requested value of a text field, where ""
// means null, is the stored value.
func sameText(stored *string, requested string) bool {
	if stored == nil {
		return requested == ""
	}
	return *stored == requested
}
