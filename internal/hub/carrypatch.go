package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// carryPatchHook is the name of the start hook that binds the carry-patch
// component; carryPatchRecoveryHook is the name of the start hook that
// discards what a rebuild interrupted by the last stop left in the copies;
// patchPurgeTask is the name of the maintenance task that removes the
// patches deleted long ago.
const (
	carryPatchHook         = "carry_patch"
	carryPatchRecoveryHook = "carry_patch_recovery"
	patchPurgeTask         = "patch_purge"
)

// composeCarryPatch adds the carry-patch work: the schema of the patch
// lists and of the remembered-resolution entries, and the component,
// registered with the workspace service as the post-clone step of the
// workspaces in mode carry_patch, so that every fresh copy of one is set up
// (its remembered-resolution entries forgotten, upstream fetched,
// remembered resolutions on, the integration branch written) before the
// workspace is ready, and as the patch-branch lookup of the push route,
// which leaves the patch branches to the patch list, without the workspace
// package knowing the mode's work, and as the extension of the syncs of
// those workspaces, which fetches upstream before a sync moves anything and
// reports the upstream's movement in the sync's answer and its record, and
// as the provider of the branches such a sync leaves out, the branches of
// the patches that are not deleted, which the per-branch pass must not move.
// The workspace service is also what the component tells of each patch
// branch a sync's origin refresh moved, so that its branch-move observers
// see those moves as they see the per-branch pass's.
// The component's controller (PushControl) is registered with the push
// rules as their carry-patch controller, which decides every update of a
// carry_patch workspace that the rules before it let through: it refuses
// the integration branch, holds the patch branches to the workspace's
// patch settings, and forwards a patch branch to origin, within the
// component's forward limit of 120 s, before the hub accepts it when
// origin is the branches' authority. Without it every such update would be
// refused as internal. The controller reads the record, the settings and
// the patch list when it decides, through the component, so it serves once
// the start hook has bound the component.
// The component's trigger is registered with the ref writer as a post-push
// listener: after every accepted git push or source control API write it
// records origin's head of each branch the write forwarded to origin, now
// that the hub's copy holds the commit, and, when the write created or
// moved the branch of a patch that is not deleted and
// auto_rebuild_after_push is on, queues one rebuild submitted by
// system:push with the write's session; then, when the hub is the
// authority of the patch branches and push_patches_to_origin is on, it
// mirrors each such branch to origin within the component's forward limit,
// reporting a failed mirror with parley.patch.mirror_failed. It runs on the
// listener's own goroutine, after the client has its answer, one at a time
// per workspace, and ignores a land, which reaches it through the
// after-landing hook. Its context ends when the hub's stop begins
// (ShuttingDown), since the stop does not wait for it and the stores close
// after the stop; a mirror cut off so reports nothing.
// The component's landing guard (LandGuard) is registered with the landing
// as a target guard, which holds a land onto a patch branch to the rules a
// push of it obeys before the land moves the branch: blocked when origin is
// the branches' authority without push_patches_to_origin, and with it
// allowed only once the forward of the new head to origin succeeded, failed
// origin_rejected otherwise. The trigger is registered as an after-landing
// hook too, with the source land and the landing's session: for a land
// that moved its target it runs on a goroutine of its own, so that the
// land's answer never waits for it, with a context that ends when the
// hub's stop begins, as the listener's does; a land that moved nothing
// triggers nothing.
// A workspace's delete removes its patches
// and its remembered-resolution entries (ADR 07). The component is the
// hub's rebuild enqueuer, through which every source of a rebuild queues
// one as a job of the hub's queue, and the handler of those jobs, which
// runs every attempt of a rebuild and writes the event of its outcome in
// the settlement's transaction. The component's routes, those of the
// patch list and of the rebuild jobs, are registered with the hub's router.
// Its start hook binds the component to the store, the secret component,
// which holds the upstream and origin credentials, the hub's git, the place
// of each copy on disk, the workspace settings registry, through which a
// patch branch's resolution reads patch_branch_source, the landing
// identity of [land], the committer of every commit a rebuild makes, the
// conflict-file describer of internal/gitrepo, which gives a rebuild's
// conflict report its files, the hub's scrubber, which cleans the text of a
// rebuild's failure, the event component, which writes an add's event in
// its transaction (ADR 05), the job queue, which the jobs part's hook binds
// before this one, and the hub's clock and logger, after every adjuster
// AdjustCarryPatch added has changed those dependencies. The hook runs
// after the workspace part's, and like it before the clone workers launch,
// so no clone runs the step before the component is bound.
// A second start hook, registered after it so that it runs on the bound
// component, discards in the copy of every carry-patch workspace the
// scratch working trees and their records a rebuild interrupted by the
// last stop or a crash left there. It is a hook, not background work,
// because the start hooks all end before the job workers start, so no
// rebuild, among them the job the jobs part's hook has just returned to
// queued, works in a copy while it runs; a copy it cannot clean is logged
// and does not stop the start.
// The maintenance task patch_purge removes, at every maintenance pass, the
// patches deleted more than seven days before the hub's clock says it is,
// with their backup refs, in every workspace; a patch it cannot remove
// fails the task for that pass and is tried again at the next.
//
// The component exists from the composition on (CarryPatch); tests change
// its dependencies with AdjustCarryPatch.
func composeCarryPatch(c *Composition) {
	comp := &carrypatch.Component{}
	c.carryPatch = comp
	c.AddSchemaStep(carrypatch.SchemaStep)
	c.AddSchemaStep(carrypatch.RerereSchemaStep)
	c.workspaces.RegisterPostCloneStep(workspace.ModeCarryPatch, comp.PostClone)
	c.workspaces.RegisterPatchBranchLookup(comp)
	c.pushControl = comp.PushControl()
	c.PushRules().RegisterController(c.pushControl)
	listen := comp.PostPushListener()
	c.RefWriter().RegisterPostPushListener(func(ctx context.Context, p pushrule.PostPush) error {
		ctx, cancel := untilShutdown(ctx, c.ShuttingDown())
		defer cancel()
		return listen(ctx, p)
	})
	c.Land().AddTargetGuard(comp.LandGuard())
	trigger := comp.Trigger()
	c.Land().AddAfterLanding(func(ctx context.Context, in land.AfterLanding) error {
		if in.Update.Before == in.Update.After {
			return nil
		}
		ctx, cancel := untilShutdown(context.WithoutCancel(ctx), c.ShuttingDown())
		updates := []pushrule.Update{{Ref: in.Update.Ref, Before: in.Update.Before, After: in.Update.After}}
		go func() {
			defer cancel()
			trigger.AfterUpdates(ctx, in.Workspace, updates, carrypatch.SourceLand, in.SessionID)
		}()
		return nil
	})
	c.workspaces.RegisterSyncExtension(comp)
	c.workspaces.RegisterExcludedBranchProvider(comp)
	c.workspaces.Teardown().RegisterRemover(carrypatch.RemoveWorkspace)
	c.workspaces.Teardown().RegisterRemover(carrypatch.RemoveRerereEntries)
	comp.RegisterRebuildEnqueuer(comp)
	c.Jobs().Register(carrypatch.RebuildJobType, comp.RebuildHandler())
	for _, rt := range comp.Routes() {
		c.AddRoute(rt)
	}
	c.AddStartHook(carryPatchHook, func(_ context.Context, rt *Runtime) error {
		d := carrypatch.Deps{
			Store:          rt.Store,
			Secrets:        c.Secrets(),
			Git:            gitFor(c.env),
			RepoPath:       c.workspaces.RepoPath,
			Settings:       c.workspaces.Settings(),
			CommitterName:  rt.Config.Land.CommitterName,
			CommitterEmail: rt.Config.Land.CommitterEmail,
			ConflictFiles:  carrypatch.GitConflictFiles{},
			Scrubber:       rt.Scrubber,
			Events:         c.Events(),
			BranchMoves:    c.workspaces,
			Jobs:           c.Jobs(),
			Now:            rt.Now,
			Logger:         rt.Logger,
		}
		for _, adjust := range c.carryPatchAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
	c.AddStartHook(carryPatchRecoveryHook, func(ctx context.Context, _ *Runtime) error {
		return comp.DiscardRebuildLeftovers(ctx)
	})
	c.AddMaintenanceTask(patchPurgeTask, func(ctx context.Context, rt *Runtime) error {
		return comp.PurgeDeletedPatches(ctx, rt.Now())
	})
}

// CarryPatch returns the hub's carry-patch component, which serves once its
// start hook has run.
func (c *Composition) CarryPatch() *carrypatch.Component { return c.carryPatch }

// PushControl returns the carry-patch controller the hub registered with
// its push rules, which reads the carry-patch component's store and
// settings and forwards with it once its start hook has run. A test that
// counts the controller's decisions registers a wrapper of it with
// PushRules().RegisterController, which replaces it.
func (c *Composition) PushControl() *carrypatch.PushControl { return c.pushControl }

// untilShutdown is ctx, ended as well when shutdown is closed, the hub's
// stop: work that runs after a request's answer, which the stop does not
// wait for, so ends before the stores close.
func untilShutdown(ctx context.Context, shutdown <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// AdjustCarryPatch adds fn to the functions that change the carry-patch
// component's dependencies after the start hook has built them and before
// it binds them, in the order they were added.
func (c *Composition) AdjustCarryPatch(fn func(*carrypatch.Deps)) {
	c.carryPatchAdjusters = append(c.carryPatchAdjusters, fn)
}
