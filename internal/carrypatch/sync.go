package carrypatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The component is the carry-patch part of a sync, which the workspace
// service runs at two points of every sync of a carry-patch workspace.
var _ workspace.SyncExtension = (*Component)(nil)

// AfterFetch is the first stage of the sync of a carry-patch workspace,
// which the workspace service runs once origin is fetched and before any
// tag or branch of the copy is created or moved: it repeats the set-up,
// which fetches upstream with the upstream credential read at this moment,
// and resolves the upstream base, so that a sync that cannot reach upstream
// fails before it has moved anything. Only then does it create a missing
// integration branch, at the base, as the set-up would: a sync that fails
// creates nothing. Its error, the failed fetch's own or the base's, names
// upstream; the workspace service answers it as a failed fetch. A failure
// of another step of the set-up is logged and does not fail the sync: the
// sync does not depend on it, and the next sync or rebuild repeats it. It
// waits for a set-up of the workspace that a rebuild or another sync runs,
// so that two upstream fetches never run into one copy at once.
func (c *Component) AfterFetch(ctx context.Context, run workspace.SyncRun) error {
	if c.d.RepoPath == nil {
		return errNotBound
	}
	w := run.Workspace
	unlock, err := c.lockSetup(ctx, w.Slug)
	if err != nil {
		return err
	}
	defer unlock()
	if err := c.runSetup(ctx, w, true, 0); err != nil {
		if fetchErr := upstreamFetchFailure(err); fetchErr != nil {
			return fetchErr
		}
		c.logger().Warn("carry-patch sync could not repeat a set-up step", "slug", w.Slug, "error", err.Error())
	}
	base, err := c.syncBase(ctx, w)
	if err != nil {
		return err
	}
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(w.Slug))
	if err == nil {
		err = createIntegrationBranch(ctx, repo, w, base)
	}
	if err != nil {
		c.logger().Warn("carry-patch sync could not create the integration branch", "slug", w.Slug, "error", err.Error())
	}
	return nil
}

// upstreamFetchFailure is the failure of the upstream fetch among the
// failed steps of the set-up error err, nil when the fetch succeeded. The
// sync reports the fetch's failure alone, which names upstream, and not
// those of steps it does not depend on.
func upstreamFetchFailure(err error) error {
	if !errors.Is(err, ErrUpstreamFetch) {
		return nil
	}
	if se, ok := errors.AsType[*setupError](err); ok {
		for _, step := range se.failed {
			if errors.Is(step, ErrUpstreamFetch) {
				return step
			}
		}
	}
	return err
}

// syncBase is the upstream base of the sync of w, from what the copy holds
// after the sync's upstream fetch. No base is an error that says the base
// cannot be resolved, which fails the sync as a failed upstream fetch does:
// without one the sync can tell neither the upstream's movement nor which
// patches upstream merged.
func (c *Component) syncBase(ctx context.Context, w workspace.Workspace) (string, error) {
	base, err := c.UpstreamBase(ctx, w)
	if errors.Is(err, ErrNoUpstreamBase) {
		return "", fmt.Errorf("upstream base cannot be resolved for workspace %q: upstream has neither a default branch nor a branch named like the default branch", w.Slug)
	}
	return base, err
}

// BeforeCompletion is the second stage of the sync of a carry-patch
// workspace, which the workspace service runs after its per-branch pass and
// before it records the completion: it refreshes the patch branches from
// origin when origin is their authority, then marks the patches upstream
// merged, queues the rebuild of the integration branch when the sync
// changed what it is built from, reports what the sync found and did
// on the carry-patch side as the answer's carry-patch members, and gives the
// upstream base as the upstream_head_sha to record when upstream advanced.
// Its error stops the sync before the completion is recorded, so that the
// next sync sees the same upstream movement again and repeats what this one
// did not finish; a refresh that stops part way runs no merge detection.
// The branch moves and merge marks made before the error stay, and the
// next sync does not see them again, so a failure after them still queues
// the rebuild they call for.
func (c *Component) BeforeCompletion(ctx context.Context, run workspace.SyncRun, _ []workspace.BranchResult) (workspace.SyncAddition, error) {
	if c.d.RepoPath == nil {
		return workspace.SyncAddition{}, errNotBound
	}
	s, err := c.startSyncPass(ctx, run)
	if err == nil {
		err = s.runSteps(ctx)
	}
	if err != nil {
		return workspace.SyncAddition{}, fmt.Errorf("the sync of workspace %q stopped: %w", run.Workspace.Slug, err)
	}
	return s.addition()
}

// runSteps runs the steps of the pass in order: the origin refresh, merge
// detection, the clearing of the origin fields and the rebuild trigger,
// stopping at the first that fails. A failure of one of the first three
// queues the rebuild that what was done before it calls for.
func (s *syncPass) runSteps(ctx context.Context) error {
	for _, step := range []func(context.Context) error{s.refreshFromOrigin, s.detectMerges, s.clearOriginFields} {
		if err := step(ctx); err != nil {
			s.rebuildAfterFailure(ctx)
			return err
		}
	}
	return s.triggerRebuild(ctx)
}

// syncPass is the second stage of one sync of a carry-patch workspace: the
// copy, the upstream base the first stage fetched and what the stage
// reports.
type syncPass struct {
	c    *Component
	run  workspace.SyncRun
	repo *gitrepo.Repo
	// by is the sync's caller, the actor of the events the pass emits.
	by actor
	// base is the upstream base as of this sync's upstream fetch.
	base   string
	report syncReport
	// moved are the patches whose branch the origin refresh created or
	// moved, as they were listed before the refresh, with the status they
	// had then.
	moved []Patch
}

// syncReport is what the sync of a carry-patch workspace adds to the
// sync's answer. Every list is present, empty when it names nothing, and
// rebuild_job_id is null when no rebuild was queued.
type syncReport struct {
	UpstreamAdvanced  bool        `json:"upstream_advanced"`
	ForcePushDetected bool        `json:"force_push_detected"`
	OriginFetched     bool        `json:"origin_fetched"`
	PatchesMerged     []string    `json:"patches_merged"`
	PatchesSynced     []syncEntry `json:"patches_synced"`
	PatchesDiverged   []string    `json:"patches_diverged"`
	RebuildTriggered  bool        `json:"rebuild_triggered"`
	RebuildJobID      *string     `json:"rebuild_job_id"`
}

// syncEntry is what the origin refresh of a sync did with one patch
// branch.
type syncEntry struct {
	BranchName  string  `json:"branch_name"`
	Action      string  `json:"action"`
	State       string  `json:"state"`
	LocalSHA    *string `json:"local_sha"`
	OriginSHA   *string `json:"origin_sha"`
	ReplacedSHA *string `json:"replaced_sha"`
}

// startSyncPass begins the second stage of the sync run: it resolves the
// upstream base again, from the refs the first stage fetched, since the
// sync carries nothing from one stage to the other, and compares it with
// the stored upstream head. The workspace service has fetched origin
// before either stage runs, so origin_fetched is true.
func (c *Component) startSyncPass(ctx context.Context, run workspace.SyncRun) (*syncPass, error) {
	w := run.Workspace
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(w.Slug))
	if err != nil {
		return nil, err
	}
	base, err := c.syncBase(ctx, w)
	if err != nil {
		return nil, err
	}
	s := &syncPass{c: c, run: run, repo: repo, by: actorOfPrincipal(run.Actor), base: base, report: syncReport{
		OriginFetched:   true,
		PatchesMerged:   []string{},
		PatchesSynced:   []syncEntry{},
		PatchesDiverged: []string{},
	}}
	s.report.UpstreamAdvanced, s.report.ForcePushDetected, err = s.upstreamMovement(ctx, w.UpstreamHeadSHA)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// upstreamMovement compares the upstream base with stored, the upstream
// head the last completed sync recorded: upstream advanced when there is no
// stored head or the base differs from it, and it was force-pushed when it
// advanced from a stored head that the base does not descend from, or
// whose commit the copy no longer holds, so that nothing can be said about
// what upstream kept.
func (s *syncPass) upstreamMovement(ctx context.Context, stored *string) (advanced, forced bool, err error) {
	if stored == nil || *stored == "" {
		return true, false, nil
	}
	if *stored == s.base {
		return false, false, nil
	}
	err = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		missing, err := s.repo.Missing(ctx, []string{*stored})
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			forced = true
			return nil
		}
		descends, err := s.repo.IsAncestor(ctx, *stored, s.base)
		forced = !descends
		return err
	})
	if err != nil {
		return false, false, fmt.Errorf("compare the upstream base with the stored upstream head: %w", err)
	}
	return true, forced, nil
}

// triggerRebuild queues the rebuild of the integration branch when the
// sync changed what it is built from: upstream advanced, the origin refresh
// created or moved the branch of a patch that was active or conflict, or a
// patch was found merged, and the workspace's auto_rebuild_after_sync is
// on. The branch of a disabled patch is not part of a rebuild, so its move
// alone queues none.
func (s *syncPass) triggerRebuild(ctx context.Context) error {
	if !s.report.UpstreamAdvanced && len(s.report.PatchesMerged) == 0 && !s.movedApplicable() {
		return nil
	}
	if !s.c.autoRebuildAfterSync(ctx, s.run.Workspace) {
		return nil
	}
	return s.queueRebuild(ctx)
}

// rebuildAfterFailure queues the rebuild of the integration branch, with
// the same duplicate skip, after a step of the pass failed, when the pass
// had already created or moved the branch of an active or conflict patch
// or marked a patch merged, and auto_rebuild_after_sync is on. Those
// changes stay and the next sync does not see them again, so a rebuild
// must take them in even though the sync fails; upstream's movement does
// not count, since the failed sync records no upstream head and the next
// sync sees it again. A failed enqueue is only logged: the sync answers the
// step's failure.
func (s *syncPass) rebuildAfterFailure(ctx context.Context) {
	if !s.movedApplicable() && len(s.report.PatchesMerged) == 0 || !s.c.autoRebuildAfterSync(ctx, s.run.Workspace) {
		return
	}
	if err := s.queueRebuild(ctx); err != nil {
		s.c.logger().Warn("carry-patch sync could not queue the rebuild after a failed step",
			"slug", s.run.Workspace.Slug, "error", err.Error())
	}
}

// movedApplicable reports whether the origin refresh created or moved the
// branch of a patch that was active or conflict when the refresh listed
// it.
func (s *syncPass) movedApplicable() bool {
	return slices.ContainsFunc(s.moved, func(p Patch) bool {
		return p.Status == StatusActive || p.Status == StatusConflict
	})
}

// queueRebuild queues a rebuild of the workspace through the registered
// RebuildEnqueuer, in the strategy and fail mode of the workspace's
// settings, submitted by the caller (see actor.submitter). A queued job is
// reported as triggered with its id. A rebuild already queued, running or
// failed is no failure: the sync queues none and reports none, since that
// rebuild will build from the inputs as this sync leaves them. Any other
// failure of the enqueue is the error, so that the sync records no
// completion: the next sync sees the same upstream movement and queues the
// rebuild again, but not the branch moves and merge marks this sync made,
// which no later sync calls a rebuild for.
func (s *syncPass) queueRebuild(ctx context.Context) error {
	res, err := s.c.Rebuilds().EnqueueRebuild(ctx, s.run.Workspace.Slug, RebuildOptions{
		SubmittedBy: s.by.submitter(), ActorType: s.by.typ, ActorID: s.by.id,
	})
	if err != nil {
		return fmt.Errorf("queue the rebuild of the integration branch: %w", err)
	}
	if res.Queued {
		s.report.RebuildTriggered, s.report.RebuildJobID = true, new(res.JobID)
	}
	return nil
}

// addition is what the pass adds to the completed sync: its report as the
// answer's members, with patches_diverged naming the branches the refresh
// left diverged, in the position order of patches_synced, so that a caller
// can fail on them without reading every entry; and the upstream base as
// the upstream_head_sha to record when upstream advanced, so that a sync
// that saw no movement leaves the stored head as it was.
func (s *syncPass) addition() (workspace.SyncAddition, error) {
	for _, e := range s.report.PatchesSynced {
		if e.State == originDiverged {
			s.report.PatchesDiverged = append(s.report.PatchesDiverged, e.BranchName)
		}
	}
	body, err := json.Marshal(s.report)
	if err != nil {
		return workspace.SyncAddition{}, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		return workspace.SyncAddition{}, err
	}
	add := workspace.SyncAddition{Members: members}
	if s.report.UpstreamAdvanced {
		add.UpstreamHeadSHA = new(s.base)
	}
	return add, nil
}
