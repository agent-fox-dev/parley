package carrypatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// RebuildJobType is the type of a rebuild job in the hub's job queue.
const RebuildJobType = "rebuild"

// EventRebuildEnqueue is the event of a queued rebuild job.
const EventRebuildEnqueue = "parley.rebuild.enqueue"

// rebuildResource is the resource type of the events of a rebuild job.
const rebuildResource = "rebuild"

// The rebuild strategies and fail modes, and the settings that choose them
// when a source of a rebuild does not.
const (
	strategyRebase     = "rebase"
	strategyMerge      = "merge"
	failModeFailFast   = "fail_fast"
	failModeContinue   = "continue"
	rebuildStrategyKey = "rebuild_strategy"
	rebuildFailModeKey = "rebuild_fail_mode"
	defaultStrategy    = strategyRebase
	defaultFailMode    = failModeFailFast
)

// rebuildPayload is the payload of a rebuild job: what was decided when it
// was queued, which the job keeps whatever the workspace becomes. SessionID
// is the agent session of the source, nil for none; the job's events carry
// it, and the job's record does not show it.
type rebuildPayload struct {
	Strategy          string  `json:"strategy"`
	FailMode          string  `json:"fail_mode"`
	IntegrationBranch string  `json:"integration_branch"`
	SubmittedBy       string  `json:"submitted_by"`
	SessionID         *string `json:"session_id"`
}

// EnqueueRebuild queues a rebuild of the carry-patch workspace slug, as
// RebuildEnqueuer: the one enqueue of every source of a rebuild, with the
// dedup of the job queue. A strategy or fail mode opts leaves nil or empty
// is the workspace's setting. A workspace with no patch to apply still gets
// its job: only the route refuses one. When a rebuild of the workspace is
// pending already, nothing is queued and the result names that job, with no
// error. A workspace that does not exist or is not in mode carry_patch is
// an error.
func (c *Component) EnqueueRebuild(ctx context.Context, slug string, opts RebuildOptions) (RebuildResult, error) {
	if c.d.Store == nil {
		return RebuildResult{}, errNotBound
	}
	ws, found, err := workspace.BySlug(ctx, c.d.Store.Main(), slug)
	if err != nil {
		return RebuildResult{}, fmt.Errorf("read the workspace %q: %w", slug, err)
	}
	if !found {
		return RebuildResult{}, fmt.Errorf("queue a rebuild of %q: no such workspace", slug)
	}
	if ws.Mode != workspace.ModeCarryPatch {
		return RebuildResult{}, fmt.Errorf("queue a rebuild of %q: the workspace is in mode %s, not %s", slug, ws.Mode, workspace.ModeCarryPatch)
	}
	job, existing, err := c.enqueueRebuild(ctx, ws, opts, false)
	if err != nil {
		return RebuildResult{}, err
	}
	if existing != "" {
		return RebuildResult{ExistingJobID: existing}, nil
	}
	return RebuildResult{Queued: true, JobID: job.ID}, nil
}

// enqueueRebuild queues a rebuild of ws with opts in one store transaction
// with its parley.rebuild.enqueue event, and returns the job; or, when the
// workspace has a pending rebuild, which the follow-up of a running job
// may not join either, that job's id in existing, with nothing written.
// requireActive refuses a workspace with no patch in active or conflict
// with 400 no_active_patches, before the dedup.
func (c *Component) enqueueRebuild(ctx context.Context, ws workspace.Workspace, opts RebuildOptions, requireActive bool) (job jobs.Job, existing string, err error) {
	if c.d.Store == nil || c.d.Jobs == nil || c.d.Events == nil {
		return jobs.Job{}, "", errNotBound
	}
	payload := rebuildPayload{
		Strategy:          c.rebuildOption(ws, opts.Strategy, rebuildStrategyKey, defaultStrategy),
		FailMode:          c.rebuildOption(ws, opts.FailMode, rebuildFailModeKey, defaultFailMode),
		IntegrationBranch: IntegrationBranch(ws),
		SubmittedBy:       opts.SubmittedBy,
		SessionID:         opts.SessionID,
	}
	doc, err := json.Marshal(payload)
	if err != nil {
		return jobs.Job{}, "", err
	}
	err = c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		job, existing = jobs.Job{}, ""
		n, err := countApplicable(ctx, tx, ws.Slug)
		if err != nil {
			return err
		}
		if requireActive && n == 0 {
			return server.NewError("no_active_patches", http.StatusBadRequest,
				fmt.Sprintf("workspace %q has no patch in active or conflict to rebuild", ws.Slug))
		}
		if opts.FollowUpOf != nil {
			job, err = c.d.Jobs.EnqueueFollowUp(ctx, tx, RebuildJobType, ws.Slug, *opts.FollowUpOf, doc)
		} else {
			job, err = c.d.Jobs.Enqueue(ctx, tx, RebuildJobType, ws.Slug, doc)
		}
		if dup, ok := errors.AsType[*jobs.DuplicateError](err); ok {
			existing = dup.JobID
			return nil
		}
		if err != nil {
			return fmt.Errorf("queue a rebuild of %s: %w", ws.Slug, err)
		}
		_, err = c.d.Events.Emit(ctx, tx, event.Input{
			Type: EventRebuildEnqueue, Workspace: &ws.Slug, ActorType: opts.ActorType, ActorID: opts.ActorID,
			ResourceType: rebuildResource, ResourceID: job.ID, SessionID: opts.SessionID,
			Metadata: map[string]any{
				"submitted_by": payload.SubmittedBy, "strategy": payload.Strategy, "fail_mode": payload.FailMode, "patch_count": n,
			},
		})
		return err
	})
	if err != nil {
		return jobs.Job{}, "", err
	}
	return job, existing, nil
}

// rebuildOption is the value of a rebuild option: given, when it is not nil
// or empty, else the effective setting key of ws, else def, the setting's
// default, when the hub has no settings registry or the setting is not a
// string.
func (c *Component) rebuildOption(ws workspace.Workspace, given *string, key, def string) string {
	if given != nil && *given != "" {
		return *given
	}
	if c.d.Settings == nil {
		return def
	}
	if v, ok := c.d.Settings.EffectiveSettings(ws.Settings, ws.DefaultBranch)[key].(string); ok && v != "" {
		return v
	}
	return def
}
