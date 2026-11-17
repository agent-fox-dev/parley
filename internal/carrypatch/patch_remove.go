package carrypatch

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The events of a patch soft-deleted by a caller and of a deleted patch
// brought back.
const (
	EventPatchDelete  = "parley.patch.delete"
	EventPatchRestore = "parley.patch.restore"
)

// deleteRoute answers DELETE /workspaces/:slug/patches/:id: it soft-deletes
// the patch and answers 204 with no body. The patch keeps its row, its
// branch and its backup ref, so that a restore can bring it back; a patch
// that is deleted already is answered the same way and left alone. A body
// is ignored.
func (c *Component) deleteRoute(w http.ResponseWriter, r *http.Request) error {
	ws, id, by, err := c.patchCall(r)
	if err != nil {
		return err
	}
	if err := c.softDelete(r.Context(), ws.Slug, id, by); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// restoreRoute answers POST /workspaces/:slug/patches/:id/restore: it brings
// a deleted patch back as the last patch of the list, active, and answers
// 200 with it. A body is ignored.
func (c *Component) restoreRoute(w http.ResponseWriter, r *http.Request) error {
	ws, id, by, err := c.patchCall(r)
	if err != nil {
		return err
	}
	p, err := c.restore(r.Context(), ws.Slug, id, by)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, ObjectOf(p, c.replacedSHA(r.Context(), ws.Slug, p.BranchName)))
	return nil
}

// patchCall checks a write route that names a patch and takes no body: the
// guard, then the id, of which one that is not a UUID is a patch the
// workspace does not have, and the caller the event names.
func (c *Component) patchCall(r *http.Request) (workspace.Workspace, string, actor, error) {
	ws, err := c.Guard(r)
	if err != nil {
		return workspace.Workspace{}, "", actor{}, err
	}
	id, err := uuid.Parse(server.Param(r, "id"))
	if err != nil {
		return workspace.Workspace{}, "", actor{}, errPatchNotFound()
	}
	by, err := actorOf(r)
	if err != nil {
		return workspace.Workspace{}, "", actor{}, err
	}
	return ws, id.String(), by, nil
}

// softDelete marks the patch id of the workspace slug deleted, without a
// position and with no conflict files, renumbers the patches after it, and
// writes one parley.patch.delete, in one store transaction; an event that
// cannot be written rolls it all back. A patch the workspace does not have
// is 404; one that is deleted already is left as it is, with no event. No
// git ref is touched and no rebuild is queued: the next rebuild leaves the
// patch out because it is deleted.
func (c *Component) softDelete(ctx context.Context, slug, id string, by actor) error {
	if c.d.Events == nil {
		return errNotBound
	}
	return c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		p, found, err := ReadPatch(ctx, tx, slug, id)
		if err != nil {
			return err
		}
		if !found {
			return errPatchNotFound()
		}
		if p.Status == StatusDeleted {
			return nil
		}
		now := c.now().UTC().Truncate(time.Second)
		if err := markDeleted(ctx, tx, p, now); err != nil {
			return err
		}
		live, err := ListLivePatches(ctx, tx, slug)
		if err != nil {
			return err
		}
		if _, err := renumber(ctx, tx, live, now); err != nil {
			return err
		}
		_, err = c.d.Events.Emit(ctx, tx, event.Input{
			Type: EventPatchDelete, Workspace: &slug, ActorType: by.typ, ActorID: by.id,
			ResourceType: "patch", ResourceID: p.ID,
			Metadata: map[string]any{"branch_name": p.BranchName, "reason": "api"},
		})
		return err
	})
}

// markDeleted marks the stored patch p deleted at now in tx, without a
// position and with no conflict files; the caller renumbers the patches
// after it.
func markDeleted(ctx context.Context, tx *sql.Tx, p Patch, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE patches SET status = ?, position = NULL, conflict_files = '[]', deleted_at = ?, updated_at = ?
WHERE workspace_slug = ? AND id = ?`, StatusDeleted, now.UnixNano(), now.UnixNano(), p.Workspace, p.ID); err != nil {
		return fmt.Errorf("delete the patch %s of %s: %w", p.BranchName, p.Workspace, err)
	}
	return nil
}

// restore brings the deleted patch id of the workspace slug back, active,
// at the end of the list, with no conflict files, and writes one
// parley.patch.restore naming its position, in one store transaction; an
// event that cannot be written rolls it back. Why the patch was deleted
// does not matter: one a rebuild removed as merged upstream comes back the
// same way. A patch the workspace does not have is 404, one that is not
// deleted 409 patch_not_deleted, and one whose branch another patch that
// is not deleted holds 409 patch_exists.
func (c *Component) restore(ctx context.Context, slug, id string, by actor) (Patch, error) {
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
		if p.Status != StatusDeleted {
			return server.NewError("patch_not_deleted", http.StatusConflict,
				fmt.Sprintf("the patch of %s is %s, not deleted", p.BranchName, p.Status))
		}
		if _, held, err := LivePatchOfBranch(ctx, tx, slug, p.BranchName); err != nil {
			return err
		} else if held {
			return errPatchExists(p.BranchName)
		}
		n, err := CountLivePatches(ctx, tx, slug)
		if err != nil {
			return err
		}
		now, pos := c.now().UTC().Truncate(time.Second), n+1
		// The list holds 1..n, so the end is a place no patch takes and
		// nothing else moves.
		_, err = tx.ExecContext(ctx, `UPDATE patches SET status = ?, position = ?, conflict_files = '[]', deleted_at = NULL, updated_at = ?
WHERE workspace_slug = ? AND id = ?`, StatusActive, pos, now.UnixNano(), slug, id)
		if isDuplicateBranch(err) {
			return errPatchExists(p.BranchName)
		}
		if err != nil {
			return fmt.Errorf("restore the patch %s of %s: %w", p.BranchName, slug, err)
		}
		if _, err := c.d.Events.Emit(ctx, tx, event.Input{
			Type: EventPatchRestore, Workspace: &slug, ActorType: by.typ, ActorID: by.id,
			ResourceType: "patch", ResourceID: p.ID,
			Metadata: map[string]any{"branch_name": p.BranchName, "position": pos},
		}); err != nil {
			return err
		}
		p.Status, p.Position, p.ConflictFiles, p.DeletedAt, p.UpdatedAt = StatusActive, &pos, []string{}, nil, now
		out = p
		return nil
	})
	return out, err
}
