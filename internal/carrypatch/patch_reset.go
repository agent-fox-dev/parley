package carrypatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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

// autoRebuildAfterSyncKey is the workspace setting that tells whether a
// patch branch moved to origin's tip queues a rebuild.
const autoRebuildAfterSyncKey = "auto_rebuild_after_sync"

// resetAnswer is the answer of a reset to origin: the patch, the action
// the reset took and whether it queued a rebuild, with the queued job's
// id when it did.
type resetAnswer struct {
	Object
	Action           string `json:"action"`
	RebuildTriggered bool   `json:"rebuild_triggered"`
	RebuildJobID     string `json:"rebuild_job_id,omitempty"`
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
	answer.RebuildTriggered, answer.RebuildJobID = c.rebuildAfterReset(r.Context(), ws, p, action, by)
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// rebuildAfterReset queues the rebuild of ws that a recorded reset taking
// action on the patch p calls for, and reports whether a job was queued
// and its id. It asks the registered RebuildEnqueuer only when the branch
// moved, the patch is active or conflict, and the workspace's
// auto_rebuild_after_sync, read now, is on. The caller submits the rebuild:
// a user as its id, the admin token, which has none, as its actor type.
// The reset stands whatever the enqueue answers: a workspace whose rebuild
// is waiting already gets none, and an enqueue that fails is logged.
func (c *Component) rebuildAfterReset(ctx context.Context, ws workspace.Workspace, p Patch, action string, by actor) (bool, string) {
	// A reset that moved the branch asks for its rebuild also when the
	// caller has left.
	ctx = context.WithoutCancel(ctx)
	if action == resetNone || p.Status != StatusActive && p.Status != StatusConflict || !c.autoRebuildAfterSync(ctx, ws) {
		return false, ""
	}
	res, err := c.Rebuilds().EnqueueRebuild(ctx, ws.Slug, RebuildOptions{SubmittedBy: by.submitter(), ActorType: by.typ, ActorID: by.id})
	if err != nil {
		c.logger().LogAttrs(ctx, slog.LevelError, "queue the rebuild after a reset to origin",
			slog.String("slug", ws.Slug), slog.String("branch", p.BranchName), slog.String("error", err.Error()))
		return false, ""
	}
	if !res.Queued {
		return false, ""
	}
	return true, res.JobID
}

// autoRebuildAfterSync reports whether the effective
// auto_rebuild_after_sync of ws is on, read from the workspace's record
// now, since a reset may have waited on origin for minutes; the record ws
// was read from stands in when it cannot be read again. A missing key or
// registry reads as the setting's default, on.
func (c *Component) autoRebuildAfterSync(ctx context.Context, ws workspace.Workspace) bool {
	if c.d.Settings == nil {
		return true
	}
	if now, found, err := workspace.BySlug(ctx, c.d.Store.Main(), ws.Slug); err == nil && found {
		ws = now
	}
	on, isBool := c.d.Settings.EffectiveSettings(ws.Settings, ws.DefaultBranch)[autoRebuildAfterSyncKey].(bool)
	return on || !isBool
}

// resetToOrigin brings the branch of the patch id of ws to origin's tip and
// returns the patch as recorded afterwards and the action taken. Only an
// active, conflict or disabled patch is reset: any other is 409
// patch_status_invalid, before origin is asked and again as the reset is
// recorded, for a patch deleted or changed meanwhile. The one branch is
// fetched from origin with the origin credential read now (409
// missing_on_origin when origin has no such branch, 502
// origin_fetch_failed when the fetch fails or outlasts its deadline), and
// the action is the first that applies: created, none, fast_forwarded,
// replaced. A replaced tip is saved to the backup ref before the branch
// moves; the branch moves by a compare-and-swap against the tip compared,
// so that one another writer moved, created or deleted in between is 409
// ref_moved and left as that writer has it. Neither workspace setting of patches changes the action:
// it follows from the two tips alone. A refusal, or a record that fails,
// changes no patch field, writes no event and leaves the branch as it was.
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
	if err := resettable(p); err != nil {
		return Patch{}, "", err
	}
	branch := p.BranchName
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(ws.Slug))
	if err != nil {
		return Patch{}, "", fmt.Errorf("reset the branch %s of %s: %w", branch, ws.Slug, err)
	}
	tip, err := c.fetchBranch(ctx, repo, ws, branch, missingOnOrigin)
	if err != nil {
		return Patch{}, "", err
	}
	local, action, err := compareWithOrigin(ctx, repo, branch, tip)
	if err != nil {
		return Patch{}, "", fmt.Errorf("reset the branch %s of %s: %w", branch, ws.Slug, err)
	}
	// From here the reset runs to its end also when the caller leaves: a
	// branch moved without its record would be a change no event reports.
	ctx = context.WithoutCancel(ctx)
	if c.d.BeforeRefSwap != nil {
		// Outside the transaction below: a test writes the store through
		// the hook, and a Store.Tx inside another would wait for its own
		// turn forever.
		c.d.BeforeRefSwap(ws.Slug, branch)
	}
	// The record and the move are one transaction, the move last in it. A
	// delete or a status change of the patch commits before it or after
	// it, never in between, so a patch the reset no longer applies to
	// moves nothing; a record that fails moves nothing; and a lost swap
	// rolls the record back. The git work held in it is local and short,
	// two update-refs at most, which is why it may hold the store's write
	// turn.
	var out Patch
	var moved bool
	err = c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		p, err := c.recordReset(ctx, tx, ws, id, by, action, local, tip)
		if err != nil {
			return err
		}
		if action != resetNone {
			if err := c.moveToOrigin(ctx, repo, ws.Slug, branch, action, local, tip); err != nil {
				return err
			}
			moved = true
		}
		out = p
		return nil
	})
	if err != nil {
		if moved {
			// The commit failed after the branch moved: nothing records
			// the move, so it is taken back.
			c.undoMove(ctx, repo, ws.Slug, branch, local, tip)
		}
		return Patch{}, "", err
	}
	return out, action, nil
}

// compareWithOrigin reads the branch of repo and compares its tip with
// tip, origin's, "" when origin has none, for a reset to origin and a
// sync's origin refresh alike. It returns the hub's tip, "" when the hub
// has no such branch, and the action that brings the branch to tip, the
// first that applies: created, none, fast_forwarded, replaced; without a
// tip, the action is "".
func compareWithOrigin(ctx context.Context, repo *gitrepo.Repo, branch, tip string) (local, action string, err error) {
	err = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		ref, exists, err := repo.LookupRef(ctx, headsPrefix+branch)
		if err != nil {
			return err
		}
		local, action = ref.ID, ""
		switch {
		case tip == "":
		case !exists:
			action = resetCreated
		case local == tip:
			action = resetNone
		default:
			behind, err := repo.IsAncestor(ctx, local, tip)
			if err != nil {
				return err
			}
			action = resetReplaced
			if behind {
				action = resetFastForwarded
			}
		}
		return nil
	})
	return local, action, err
}

// resettable is nil for a patch a reset to origin applies to, active,
// conflict or disabled, and 409 patch_status_invalid for any other.
func resettable(p Patch) error {
	if p.Status == StatusMergedUpstream || p.Status == StatusDeleted {
		return server.NewError("patch_status_invalid", http.StatusConflict,
			fmt.Sprintf("the patch of %s is %s; only an active, conflict or disabled patch is reset to origin", p.BranchName, p.Status))
	}
	return nil
}

// undoMove puts the branch of the workspace slug in repo back from origin's
// tip to local, removing it when local is "", after the record of the
// reset that moved it failed to commit. It moves the branch only from tip,
// so that one another writer moved since is left as that writer has it,
// and logs an undo that fails. The backup ref a replace wrote stays, as
// after a lost swap.
func (c *Component) undoMove(ctx context.Context, repo *gitrepo.Repo, slug, branch, local, tip string) {
	back := local
	if back == "" {
		// git's null id as the new value removes the ref.
		back = strings.Repeat("0", len(tip))
	}
	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return repo.CompareAndSwapRef(ctx, headsPrefix+branch, back, tip)
	})
	if err != nil {
		c.logger().LogAttrs(ctx, slog.LevelError, "put back a branch whose reset to origin was not recorded",
			slog.String("slug", slug), slog.String("branch", branch), slog.String("back_to", local), slog.String("error", err.Error()))
	}
}

// moveToOrigin moves the branch of the workspace slug in repo from local,
// "" for none, to origin's tip, saving local to the backup ref first when
// action is replaced (swapToOrigin). The backup stays when the move then
// loses to another writer, which is 409 ref_moved. It runs inside the
// reset's transaction.
func (c *Component) moveToOrigin(ctx context.Context, repo *gitrepo.Repo, slug, branch, action, local, tip string) error {
	err := swapToOrigin(ctx, repo, branch, action, local, tip, nil)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitrepo.ErrRefMoved):
		return refMovedNow(ctx, repo, branch, "reset to origin")
	case errors.Is(err, gitrepo.ErrRefNameConflict):
		return nameConflict(ctx, repo, branch, headsPrefix+branch)
	}
	return fmt.Errorf("reset the branch %s of %s: %w", branch, slug, err)
}

// swapToOrigin moves branch of repo by a compare-and-swap from local, ""
// for none, to tip, origin's, saving local to the backup ref first when
// action is replaced, so that a replaced tip stays reachable even when the
// swap then loses. With backupWas nil the backup is written whatever the
// backup ref holds (WriteBackupRef); else only while it holds *backupWas
// (SwapBackupRef), and a lost backup swap moves nothing.
func swapToOrigin(ctx context.Context, repo *gitrepo.Repo, branch, action, local, tip string, backupWas *string) error {
	if action == resetReplaced {
		var err error
		if backupWas == nil {
			err = WriteBackupRef(ctx, repo, branch, local)
		} else {
			err = SwapBackupRef(ctx, repo, branch, local, *backupWas)
		}
		if err != nil {
			return err
		}
	}
	return RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return repo.CompareAndSwapRef(ctx, headsPrefix+branch, tip, local)
	})
}

// recordReset records the reset of the patch id of ws in tx: origin's tip
// as the patch's origin state when the workspace's patch_branch_source is
// origin, and parley.patch.reset, with parley.patch.replace for a replaced
// branch. local is the branch's tip before the reset, "" when it had none.
// The patch is read again first: one deleted, or whose status no longer
// allows a reset, since the reset first read it is refused as before the
// fetch. It returns the patch as recorded.
func (c *Component) recordReset(ctx context.Context, tx *sql.Tx, ws workspace.Workspace, id string, by actor, action, local, tip string) (Patch, error) {
	p, found, err := ReadPatch(ctx, tx, ws.Slug, id)
	if err != nil {
		return Patch{}, err
	}
	if !found {
		return Patch{}, errPatchNotFound()
	}
	if err := resettable(p); err != nil {
		return Patch{}, err
	}
	if c.fromOrigin(ws) {
		now := c.now().UTC().Truncate(time.Second)
		if _, err := tx.ExecContext(ctx, `UPDATE patches SET origin_sync_state = ?, origin_sha = ?, origin_synced_at = ?
WHERE workspace_slug = ? AND id = ?`, originInSync, tip, now.UnixNano(), ws.Slug, id); err != nil {
			return Patch{}, fmt.Errorf("record the origin state of the patch %s of %s: %w", p.BranchName, ws.Slug, err)
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
			return Patch{}, err
		}
	}
	return p, nil
}

// refMovedNow is 409 ref_moved for branch of repo, which another writer
// moved, created or deleted between the read and the compare-and-swap of
// the operation that was doing what doing says, with the commit it is at
// now as details.current_sha, null when the writer deleted it.
func refMovedNow(ctx context.Context, repo *gitrepo.Repo, branch, doing string) error {
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
		fmt.Sprintf("the branch %q moved while it was %s; read it again and retry", branch, doing)).
		WithDetails(map[string]any{"current_sha": current})
}

// missingOnOrigin is 409 missing_on_origin for branch, which origin does
// not have.
func missingOnOrigin(branch string) error {
	return server.NewError("missing_on_origin", http.StatusConflict, fmt.Sprintf("origin has no branch %q", branch))
}
