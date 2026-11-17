package carrypatch

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// EventRebuildFollowUp is the event of a rebuild job that ended on inputs
// that went stale while it ran, and so queued a follow-up rebuild.
const EventRebuildFollowUp = "parley.rebuild.followup"

// autoRebuildAfterPushKey is the workspace setting that tells whether a
// moved patch branch queues a rebuild; auto_rebuild_after_sync tells it for
// a moved upstream base.
const autoRebuildAfterPushKey = "auto_rebuild_after_push"

// followUpSubmitter is the submitted_by of a follow-up rebuild.
const followUpSubmitter = "system:followup"

// followUp is the follow-up rebuild an attempt queued: its job's id, the
// branch names of the patches whose tips went stale, in position order,
// and whether the upstream base did.
type followUp struct {
	jobID         string
	stalePatches  []string
	upstreamStale bool
}

// followUp compares, once the run has published and pushed or stopped at a
// fail_fast conflict, the inputs it read with the present, without
// fetching anything: a patch the run listed that is not deleted since is
// stale when its branch exists and its tip is not the one the run read,
// and upstream is stale when the upstream base resolved now is not the one
// the run started from. A trigger that came while the run went on was
// skipped; this check is what keeps it from being lost. Stale patches
// count only while auto_rebuild_after_push is on, a stale upstream only
// while auto_rebuild_after_sync is, both read now. When anything counts,
// it queues one rebuild with the job's strategy and fail mode as the
// follow-up of the job, which is still running, so that it starts only
// once this one has ended; the settlement reports it. A rebuild of the
// workspace already pending makes the follow-up needless, and a check or
// an enqueue that fails is logged: neither changes the attempt's outcome.
func (r *rebuildRun) followUp(ctx context.Context) *followUp {
	stale, upstreamStale := r.staleInputs(ctx)
	if len(stale) == 0 && !upstreamStale {
		return nil
	}
	ws, found, err := workspace.BySlug(ctx, r.c.d.Store.Main(), r.job.Workspace)
	switch {
	case err != nil:
		r.c.logger().Warn("rebuild could not read its workspace for the follow-up",
			"workspace", r.job.Workspace, "job_id", r.job.ID, "error", err)
	case !found:
		return nil
	default:
		r.ws = ws
	}
	if !r.c.autoRebuildOn(r.ws, autoRebuildAfterPushKey) {
		stale = nil
	}
	if !r.c.autoRebuildOn(r.ws, autoRebuildAfterSyncKey) {
		upstreamStale = false
	}
	if len(stale) == 0 && !upstreamStale {
		return nil
	}
	opts := RebuildOptions{
		SubmittedBy: followUpSubmitter, ActorType: event.ActorSystem,
		SessionID: r.payload.SessionID, FollowUpOf: &r.job.ID,
	}
	if r.payload.Strategy != "" {
		opts.Strategy = &r.payload.Strategy
	}
	if r.payload.FailMode != "" {
		opts.FailMode = &r.payload.FailMode
	}
	res, err := r.c.Rebuilds().EnqueueRebuild(ctx, r.ws.Slug, opts)
	if err != nil {
		r.c.logger().Warn("rebuild could not queue its follow-up",
			"workspace", r.ws.Slug, "job_id", r.job.ID, "error", err)
		return nil
	}
	if !res.Queued {
		return nil
	}
	return &followUp{jobID: res.JobID, stalePatches: stale, upstreamStale: upstreamStale}
}

// staleInputs are the branch names of the patches whose tips moved since
// the run listed them, in their present order, and whether the upstream
// base moved since the run resolved it. What cannot be read is not stale.
func (r *rebuildRun) staleInputs(ctx context.Context) (stale []string, upstreamStale bool) {
	if r.repo == nil {
		return nil, false
	}
	read := map[string]*string{}
	for _, p := range r.patches {
		read[p.ID] = p.source
	}
	live, err := ListLivePatches(ctx, r.c.d.Store.Main(), r.job.Workspace)
	if err != nil {
		r.c.logger().Warn("rebuild could not list its patches for the follow-up",
			"workspace", r.job.Workspace, "job_id", r.job.ID, "error", err)
		live = nil
	}
	for _, p := range live {
		source, listed := read[p.ID]
		if !listed {
			continue
		}
		var tip string
		var found bool
		err := r.git(ctx, func(ctx context.Context) (err error) {
			tip, found, err = r.repo.ResolveBranch(ctx, p.BranchName)
			return err
		})
		// A branch deleted meanwhile is not stale: a delete queues no
		// rebuild.
		if err != nil || !found {
			continue
		}
		if source == nil || *source != tip {
			stale = append(stale, p.BranchName)
		}
	}
	base, err := r.c.UpstreamBase(ctx, r.ws)
	return stale, err == nil && base != r.base
}

// autoRebuildOn reports whether the effective boolean setting key of ws is
// on; a missing key or registry reads as the default, on.
func (c *Component) autoRebuildOn(ws workspace.Workspace, key string) bool {
	if c.d.Settings == nil {
		return true
	}
	on, isBool := c.d.Settings.EffectiveSettings(ws.Settings, ws.DefaultBranch)[key].(bool)
	return on || !isBool
}
