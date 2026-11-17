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
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// rollbackAnswer is the answer of a rollback: the commit the integration
// branch now points at, and the head it replaced, null when the rollback
// created the branch.
type rollbackAnswer struct {
	RolledBackTo    string  `json:"rolled_back_to"`
	PreviousHeadSHA *string `json:"previous_head_sha"`
}

// rollbackRoute answers POST /workspaces/:slug/rebuilds/:id/rollback,
// whatever its body: it moves the workspace's integration branch back to
// the head it had before the completed rebuild id, and answers 200 with
// the two heads. After the guard and the job's lookup, a job that is not
// completed, recorded no previous head, or whose previous head is not a
// commit of the copy is 409 rollback_unavailable. The branch moves by one
// compare-and-swap against the head read, so that one another writer
// moved in between is 409 ref_moved and left as that writer has it.
// Nothing else changes: no patch, no job, no other ref, nothing on origin;
// the workspace's settings are not read and no rebuild is queued.
func (c *Component) rollbackRoute(w http.ResponseWriter, r *http.Request) error {
	if c.d.Store == nil || c.d.Events == nil {
		return errNotBound
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	j, err := c.rebuildJob(ctx, ws.Slug, server.Param(r, "id"))
	if err != nil {
		return err
	}
	by, err := actorOf(r)
	if err != nil {
		return err
	}
	answer, err := c.rollback(ctx, ws, j, by)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// rollback moves the integration branch of ws to the previous head the
// completed rebuild job j recorded, creating the branch when it does not
// exist, and writes parley.rebuild.rollback with by as its actor once the
// branch has moved.
func (c *Component) rollback(ctx context.Context, ws workspace.Workspace, j jobs.Job, by actor) (rollbackAnswer, error) {
	previous := RebuildRecordOf(j).PreviousIntegrationHeadSHA
	switch {
	case j.Status != jobs.StatusCompleted:
		return rollbackAnswer{}, rollbackUnavailable(fmt.Sprintf("rebuild job %s is %s; only a completed rebuild can be rolled back", j.ID, j.Status))
	case previous == nil || *previous == "":
		return rollbackAnswer{}, rollbackUnavailable(fmt.Sprintf("rebuild job %s recorded no previous head of the integration branch", j.ID))
	}
	branch := IntegrationBranch(ws)
	ref := headsPrefix + branch
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(ws.Slug))
	if err != nil {
		return rollbackAnswer{}, fmt.Errorf("roll back the branch %s of %s: %w", branch, ws.Slug, err)
	}
	var target gitrepo.Object
	var current gitrepo.Ref
	var exists bool
	err = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		// ObjectInfo takes a full id alone, checked before git runs, so the
		// recorded value is never read as an option or a ref.
		var err error
		if target, err = repo.ObjectInfo(ctx, *previous); err != nil {
			return err
		}
		current, exists, err = repo.LookupRef(ctx, ref)
		return err
	})
	if errors.Is(err, gitrepo.ErrNotFound) || err == nil && target.Type != gitrepo.ObjectCommit {
		return rollbackAnswer{}, rollbackUnavailable(fmt.Sprintf("the previous head %s of rebuild job %s is not a commit of the repository", *previous, j.ID))
	}
	if err != nil {
		return rollbackAnswer{}, fmt.Errorf("roll back the branch %s of %s: %w", branch, ws.Slug, err)
	}
	var replaced *string
	if exists {
		replaced = &current.ID
	}
	// From here the rollback runs to its end also when the caller leaves: a
	// branch moved without its event would be a change nothing reports.
	ctx = context.WithoutCancel(ctx)
	if c.d.BeforeRollbackSwap != nil {
		c.d.BeforeRollbackSwap(ws.Slug, ref)
	}
	err = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		// The branch's absence is the expected value when it does not exist.
		return repo.CompareAndSwapRef(ctx, ref, target.ID, current.ID)
	})
	switch {
	case errors.Is(err, gitrepo.ErrRefMoved):
		return rollbackAnswer{}, refMovedNow(ctx, repo, branch, "rolled back")
	case errors.Is(err, gitrepo.ErrRefNameConflict):
		return rollbackAnswer{}, nameConflict(ctx, repo, branch, ref)
	case err != nil:
		return rollbackAnswer{}, fmt.Errorf("roll back the branch %s of %s: %w", branch, ws.Slug, err)
	}
	// The event follows the move: a ref is not part of the store's
	// transaction, and an event written first could report a rollback that
	// then lost its compare-and-swap.
	err = c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := c.d.Events.Emit(ctx, tx, event.Input{
			Type: EventRebuildRollback, Workspace: &ws.Slug, ActorType: by.typ, ActorID: by.id,
			ResourceType: rebuildResource, ResourceID: j.ID,
			Metadata: map[string]any{"integration_branch": branch, "rolled_back_to": target.ID, "previous_head_sha": replaced},
		})
		return err
	})
	if err != nil {
		c.undoRollback(ctx, repo, ws.Slug, ref, current.ID, target.ID)
		return rollbackAnswer{}, err
	}
	return rollbackAnswer{RolledBackTo: target.ID, PreviousHeadSHA: replaced}, nil
}

// undoRollback puts ref of the workspace slug in repo back from target to
// previous, removing it when previous is "", after the event of the
// rollback that moved it failed to commit. It moves the ref only from
// target, so that one another writer moved since is left as that writer
// has it, and logs an undo that fails.
func (c *Component) undoRollback(ctx context.Context, repo *gitrepo.Repo, slug, ref, previous, target string) {
	back := previous
	if back == "" {
		// git's null id as the new value removes the ref.
		back = strings.Repeat("0", len(target))
	}
	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return repo.CompareAndSwapRef(ctx, ref, back, target)
	})
	if err != nil {
		c.logger().LogAttrs(ctx, slog.LevelError, "put back an integration branch whose rollback was not recorded",
			slog.String("slug", slug), slog.String("ref", ref), slog.String("back_to", previous), slog.String("error", err.Error()))
	}
}

// rollbackUnavailable is 409 rollback_unavailable with the reason why.
func rollbackUnavailable(why string) error {
	return server.NewError("rollback_unavailable", http.StatusConflict, why)
}
