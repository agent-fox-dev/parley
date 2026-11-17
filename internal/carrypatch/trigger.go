package carrypatch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// TriggerSource is how the updates a trigger runs for reached the hub: a
// git push or a write through the source control API (push), or the
// landing of a change (land). A rebuild the trigger queues is submitted by
// "system:<source>".
type TriggerSource string

// The sources of a trigger.
const (
	SourcePush TriggerSource = "push"
	SourceLand TriggerSource = "land"
)

// refLimit is the deadline of a git that reads or writes a few refs of the
// hub's copy, such as the one that records origin's head of a branch.
const refLimit = 30 * time.Second

// Trigger runs what the hub owes a carry-patch workspace once it accepted
// updates of its patch branches: one rebuild of the integration branch for
// the request, when auto_rebuild_after_push is on, and then, when the hub
// is the authority of the patch branches and push_patches_to_origin is on,
// the mirror of each patch branch the request created or moved to origin,
// whatever became of the rebuild. It reads the workspace
// record, the settings and the patch list through the same readers as
// PushControl, the component's own unless a test replaces them, and queues
// with the component's registered rebuild enqueuer, read at every run.
//
// A trigger runs after the client has its answer and changes nothing of
// it: it returns nothing, a failure is logged, and a panic is recovered.
type Trigger struct {
	Records  RecordReader
	Settings SettingsReader
	Patches  PatchReader
	// Mirror pushes the branches of patches, the patches whose branches
	// the request created or moved, to the origin of the workspace w: the
	// component's mirror, which a test may replace or wrap.
	Mirror func(ctx context.Context, w workspace.Workspace, patches []Patch)

	c *Component
}

// Trigger is the component's trigger, which reads the component's store
// and settings registry.
func (c *Component) Trigger() *Trigger {
	return &Trigger{Records: c, Settings: c, Patches: c, Mirror: c.mirror, c: c}
}

// PostPushListener is the trigger as a post-push listener of the ref
// writer, which the hub registers.
func (c *Component) PostPushListener() pushrule.PostPushListener {
	return c.Trigger().PostPush
}

// AfterUpdates runs the trigger for the updates of the workspace slug that
// one accepted request wrote, in command order, from source, with the
// session the request belongs to, nil for none. The triggers of one
// workspace run one at a time, so that a second request's trigger starts
// when the first has finished; one that is still waiting when ctx ends
// does not run.
func (t *Trigger) AfterUpdates(ctx context.Context, slug string, updates []pushrule.Update, source TriggerSource, sessionID *string) {
	t.serialized(ctx, slug, func(log *slog.Logger) {
		t.run(ctx, log, slug, updates, source, sessionID)
	})
}

// PostPush is the trigger for an accepted write the ref writer reports: a
// git push or a source control API write is a push. A land is ignored,
// since the landing reaches the trigger through the after-landing hook
// and would otherwise trigger twice. Before the trigger it records origin's
// head of each branch the write forwarded to origin. It always returns
// nil: the write is accepted, and nothing the trigger meets changes that.
func (t *Trigger) PostPush(ctx context.Context, p pushrule.PostPush) error {
	if p.Via == pushrule.ViaLand {
		return nil
	}
	updates := make([]pushrule.Update, len(p.Updates))
	for i, d := range p.Updates {
		updates[i] = d.Update
	}
	t.serialized(ctx, p.Workspace, func(log *slog.Logger) {
		t.recordForwardedHeads(ctx, log, p.Workspace, p.Updates)
		t.run(ctx, log, p.Workspace, updates, SourcePush, p.SessionID)
	})
	return nil
}

// serialized runs fn under the trigger lock of slug, with the component's
// logger naming the workspace, and logs a panic of fn as an error.
func (t *Trigger) serialized(ctx context.Context, slug string, fn func(log *slog.Logger)) {
	log := t.c.logger().With("workspace", slug)
	defer func() {
		if r := recover(); r != nil {
			log.Error("carry-patch trigger panicked", "panic", fmt.Sprint(r))
		}
	}()
	unlock, err := t.c.triggers.lock(ctx, slug)
	if err != nil {
		log.Warn("carry-patch trigger did not run: it ended while the one before it ran", "error", err)
		return
	}
	defer unlock()
	fn(log)
}

// run is the trigger's work, under the workspace's trigger lock, so that
// the mirrors of two requests never push one workspace's branches at the
// same time, nor in an order other than the requests'. The record is read
// first, so that a workspace not in mode carry_patch, a task fork among
// them, costs no read of the patch list. The patch list is
// read as it is now: a branch whose patch was deleted since the request
// triggers nothing.
func (t *Trigger) run(ctx context.Context, log *slog.Logger, slug string, updates []pushrule.Update, source TriggerSource, sessionID *string) {
	rec, found, err := t.Records.WorkspaceRecord(ctx, slug)
	switch {
	case err != nil:
		log.Error("carry-patch trigger failed: the workspace record cannot be read", "error", err)
		return
	case !found:
		log.Debug("carry-patch trigger skipped: the workspace is gone")
		return
	case rec.Mode != workspace.ModeCarryPatch:
		return
	}
	patches, err := t.movedPatches(ctx, slug, updates)
	if err != nil {
		log.Error("carry-patch trigger failed: the patch list cannot be read", "error", err)
		return
	}
	if len(patches) == 0 {
		return
	}
	settings, err := t.Settings.EffectiveSettings(ctx, rec)
	if err != nil {
		log.Error("carry-patch trigger failed: the settings cannot be read", "error", err)
		return
	}
	t.queueRebuild(ctx, log, slug, settings, source, sessionID)
	if mirrorOn(settings) && t.Mirror != nil {
		t.Mirror(ctx, rec, patches)
	}
}

// movedPatches are the live patches whose branches updates created or
// moved, in the order of the updates and each once; a delete moves none.
func (t *Trigger) movedPatches(ctx context.Context, slug string, updates []pushrule.Update) ([]Patch, error) {
	var out []Patch
	seen := map[string]bool{}
	for _, u := range updates {
		branch, isBranch := strings.CutPrefix(u.Ref, headsPrefix)
		if !isBranch || branch == "" || u.After == pushrule.ZeroID || seen[branch] {
			continue
		}
		seen[branch] = true
		p, live, err := t.Patches.LivePatchOfBranch(ctx, slug, branch)
		if err != nil {
			return nil, fmt.Errorf("look up the patch of %s: %w", branch, err)
		}
		if live {
			out = append(out, p)
		}
	}
	return out, nil
}

// queueRebuild queues one rebuild of slug for the request unless
// auto_rebuild_after_push is off; a missing or mistyped value is its
// default, on. A rebuild already queued, running or waiting to retry is
// no failure: the request is skipped without a word above debug. A
// failure or a panic of the enqueue is logged and ends only this step.
func (t *Trigger) queueRebuild(ctx context.Context, log *slog.Logger, slug string, settings map[string]any, source TriggerSource, sessionID *string) {
	if on, isBool := settings[autoRebuildAfterPushKey].(bool); isBool && !on {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Error("carry-patch trigger could not queue a rebuild: the enqueue panicked", "panic", fmt.Sprint(r))
		}
	}()
	res, err := t.c.Rebuilds().EnqueueRebuild(ctx, slug, RebuildOptions{
		SubmittedBy: "system:" + string(source), ActorType: event.ActorSystem, SessionID: sessionID,
	})
	switch {
	case err != nil:
		log.Error("carry-patch trigger could not queue a rebuild", "error", err)
	case !res.Queued:
		log.Debug("carry-patch trigger queued no rebuild: one is already queued", "existing_job_id", res.ExistingJobID)
	}
}

// recordForwardedHeads records, for each update of an accepted write that
// the ref writer forwarded to origin before accepting it, its commit as
// origin's head of the branch in the hub's copy: origin took the commit,
// and now that the write is accepted the copy holds it too (erratum 37).
// A branch the hub has moved on from since is left to the write that moved
// it, whose trigger runs after this one under the same lock, so that an
// older commit never overwrites a newer record. A failure is logged and
// changes nothing else.
func (t *Trigger) recordForwardedHeads(ctx context.Context, log *slog.Logger, slug string, updates []pushrule.Decision) {
	var repo *gitrepo.Repo
	for _, d := range updates {
		branch, isBranch := strings.CutPrefix(d.Ref, headsPrefix)
		if d.Forward == nil || !isBranch || branch == "" || d.After == pushrule.ZeroID {
			continue
		}
		if repo == nil {
			var err error
			if t.c.d.RepoPath == nil {
				err = errNotBound
			} else {
				repo, err = t.c.d.Git.Open(ctx, t.c.d.RepoPath(slug))
			}
			if err != nil {
				log.Warn("forwarded to origin, and origin's head could not be recorded: the hub's copy cannot be opened", "error", err)
				return
			}
		}
		err := RunGit(ctx, refLimit, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
			head, found, err := repo.ResolveBranch(ctx, branch)
			if err != nil || !found || head != d.After {
				return err
			}
			return repo.SetOriginHead(ctx, branch, d.After)
		})
		if err != nil {
			log.Warn("forwarded to origin, and origin's head could not be recorded", "branch", branch, "error", err)
		}
	}
}
