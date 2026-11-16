package carrypatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The events of a patch branch reset to origin's tip and of a patch branch
// whose tip the hub replaced, saving it to the backup ref.
const (
	EventPatchReset   = "parley.patch.reset"
	EventPatchReplace = "parley.patch.replace"
)

// The actions of a reset to origin.
const (
	// resetCreated is a branch the copy did not hold, created at origin's
	// tip.
	resetCreated = "created"
	// resetNone is a branch at origin's tip already, left as it is.
	resetNone = "none"
	// resetFastForwarded is a branch whose tip origin's descends from,
	// moved forward to it.
	resetFastForwarded = "fast_forwarded"
	// resetReplaced is any other branch: its tip is saved to the backup ref
	// and the branch is moved to origin's tip.
	resetReplaced = "replaced"
)

// originInSync is the origin_sync_state of a patch whose branch is at
// origin's tip.
const originInSync = "in_sync"

// resetAnswer is the answer of a reset to origin: the patch, the action
// the reset took and whether it queued a rebuild.
type resetAnswer struct {
	Object
	Action           string `json:"action"`
	RebuildTriggered bool   `json:"rebuild_triggered"`
}

// resetRoute answers POST /workspaces/:slug/patches/:id/reset-to-origin: it
// brings the patch's branch in the hub's copy to origin's tip and answers
// 200 with the patch and the action it took. A body is ignored.
func (c *Component) resetRoute(w http.ResponseWriter, r *http.Request) error {
	ws, id, by, err := c.patchCall(r)
	if err != nil {
		return err
	}
	p, action, err := c.resetToOrigin(r.Context(), ws, id, by)
	if err != nil {
		return err
	}
	answer := resetAnswer{Object: ObjectOf(p, c.replacedSHA(r.Context(), ws.Slug, p.BranchName)), Action: action}
	// The rebuild of a reset that moved the branch is asked for here, once
	// the outcome is recorded; until then the answer reports none.
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// resetToOrigin brings the branch of the patch id of ws to origin's tip and
// returns the patch as recorded afterwards and the action taken. Only an
// active, conflict or disabled patch is reset: any other is 409
// patch_status_invalid, before origin is asked. The one branch is fetched
// from origin with the origin credential read now (409 missing_on_origin
// when origin has no such branch, 502 origin_fetch_failed when the fetch
// fails or outlasts its deadline), and the action is the first that
// applies: created, none, fast_forwarded, replaced. A replaced tip is saved
// to the backup ref before the branch moves; the branch moves by a
// compare-and-swap against the tip compared, so that one another writer
// moved, created or deleted in between is 409 ref_moved and left as that
// writer has it. Neither workspace setting of patches changes the action:
// it follows from the two tips alone. A refusal changes no patch field and
// writes no event.
func (c *Component) resetToOrigin(ctx context.Context, ws workspace.Workspace, id string, by actor) (Patch, string, error) {
	if c.d.Events == nil {
		return Patch{}, "", errNotBound
	}
	p, found, err := ReadPatch(ctx, c.d.Store.Main(), ws.Slug, id)
	if err != nil {
		return Patch{}, "", fmt.Errorf("read the patch %s of %s: %w", id, ws.Slug, err)
	}
	if !found {
		return Patch{}, "", errPatchNotFound()
	}
	if p.Status == StatusMergedUpstream || p.Status == StatusDeleted {
		return Patch{}, "", server.NewError("patch_status_invalid", http.StatusConflict,
			fmt.Sprintf("the patch of %s is %s; only an active, conflict or disabled patch is reset to origin", p.BranchName, p.Status))
	}
	branch, head := p.BranchName, headsPrefix+p.BranchName
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(ws.Slug))
	if err != nil {
		return Patch{}, "", fmt.Errorf("reset the branch %s of %s: %w", branch, ws.Slug, err)
	}
	tip, err := c.fetchBranch(ctx, repo, ws, branch, missingOnOrigin)
	if err != nil {
		return Patch{}, "", err
	}
	var local gitrepo.Ref
	var exists, behind bool
	err = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		var err error
		if local, exists, err = repo.LookupRef(ctx, head); err != nil || !exists || local.ID == tip {
			return err
		}
		behind, err = repo.IsAncestor(ctx, local.ID, tip)
		return err
	})
	if err != nil {
		return Patch{}, "", fmt.Errorf("reset the branch %s of %s: %w", branch, ws.Slug, err)
	}
	action := resetReplaced
	switch {
	case !exists:
		action = resetCreated
	case local.ID == tip:
		action = resetNone
	case behind:
		action = resetFastForwarded
	}
	// From here the reset runs to its end also when the caller leaves: a
	// branch moved without its record would be a change no event reports.
	ctx = context.WithoutCancel(ctx)
	if action != resetNone {
		if err := c.moveToOrigin(ctx, repo, ws.Slug, branch, action, local.ID, tip); err != nil {
			return Patch{}, "", err
		}
	}
	p, err = c.recordReset(ctx, ws, id, by, action, local.ID, tip)
	return p, action, err
}

// moveToOrigin moves the branch of the workspace slug in repo from local,
// "" for none, to origin's tip, saving local to the backup ref first when
// action is replaced. The backup stays when the move then loses to another
// writer, which is 409 ref_moved.
func (c *Component) moveToOrigin(ctx context.Context, repo *gitrepo.Repo, slug, branch, action, local, tip string) error {
	if c.d.BeforeRefSwap != nil {
		c.d.BeforeRefSwap(slug, branch)
	}
	if action == resetReplaced {
		if err := WriteBackupRef(ctx, repo, branch, local); err != nil {
			return fmt.Errorf("reset the branch %s of %s: %w", branch, slug, err)
		}
	}
	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return repo.CompareAndSwapRef(ctx, headsPrefix+branch, tip, local)
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitrepo.ErrRefMoved):
		return refMovedNow(ctx, repo, branch)
	case errors.Is(err, gitrepo.ErrRefNameConflict):
		return nameConflict(ctx, repo, branch, headsPrefix+branch)
	}
	return fmt.Errorf("reset the branch %s of %s: %w", branch, slug, err)
}

// recordReset records the reset of the patch id of ws in one store
// transaction: origin's tip as the patch's origin state when the
// workspace's patch_branch_source is origin, and parley.patch.reset, with
// parley.patch.replace for a replaced branch. local is the branch's tip
// before the reset, "" when it had none. It returns the patch as recorded.
func (c *Component) recordReset(ctx context.Context, ws workspace.Workspace, id string, by actor, action, local, tip string) (Patch, error) {
	var out Patch
	err := c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		p, found, err := ReadPatch(ctx, tx, ws.Slug, id)
		if err != nil {
			return err
		}
		if !found {
			return errPatchNotFound()
		}
		if c.fromOrigin(ws) {
			now := c.now().UTC().Truncate(time.Second)
			if _, err := tx.ExecContext(ctx, `UPDATE patches SET origin_sync_state = ?, origin_sha = ?, origin_synced_at = ?
WHERE workspace_slug = ? AND id = ?`, originInSync, tip, now.UnixNano(), ws.Slug, id); err != nil {
				return fmt.Errorf("record the origin state of the patch %s of %s: %w", p.BranchName, ws.Slug, err)
			}
			p.OriginSyncState, p.OriginSHA, p.OriginSyncedAt = new(originInSync), &tip, &now
		}
		var localSHA, replacedSHA any // null unless set
		if local != "" {
			localSHA = local
		}
		if action == resetReplaced {
			replacedSHA = local
		}
		events := []event.Input{{
			Type: EventPatchReset, Workspace: &ws.Slug, ActorType: by.typ, ActorID: by.id,
			ResourceType: "patch", ResourceID: p.ID,
			Metadata: map[string]any{"branch_name": p.BranchName, "action": action, "local_sha": localSHA,
				"origin_sha": tip, "replaced_sha": replacedSHA},
		}}
		if action == resetReplaced {
			events = append(events, event.Input{
				Type: EventPatchReplace, Workspace: &ws.Slug, ActorType: by.typ, ActorID: by.id,
				ResourceType: "patch", ResourceID: p.ID,
				Metadata: map[string]any{"branch_name": p.BranchName, "replaced_sha": local, "origin_sha": tip,
					"trigger": "reset_to_origin"},
			})
		}
		for _, in := range events {
			if _, err := c.d.Events.Emit(ctx, tx, in); err != nil {
				return err
			}
		}
		out = p
		return nil
	})
	return out, err
}

// refMovedNow is 409 ref_moved for branch of repo, which another writer
// moved, created or deleted between the reset's read and its
// compare-and-swap, with the commit it is at now as details.current_sha,
// null when the writer deleted it.
func refMovedNow(ctx context.Context, repo *gitrepo.Repo, branch string) error {
	var ref gitrepo.Ref
	var found bool
	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		var err error
		ref, found, err = repo.LookupRef(ctx, headsPrefix+branch)
		return err
	})
	if err != nil {
		return fmt.Errorf("read the branch %s after it moved: %w", branch, err)
	}
	var current *string
	if found {
		current = &ref.ID
	}
	return server.NewError("ref_moved", http.StatusConflict,
		fmt.Sprintf("the branch %q moved while it was reset to origin; read it again and retry", branch)).
		WithDetails(map[string]any{"current_sha": current})
}

// missingOnOrigin is 409 missing_on_origin for branch, which origin does
// not have.
func missingOnOrigin(branch string) error {
	return server.NewError("missing_on_origin", http.StatusConflict, fmt.Sprintf("origin has no branch %q", branch))
}
