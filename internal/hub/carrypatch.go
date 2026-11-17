package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// carryPatchHook is the name of the start hook that binds the carry-patch
// component.
const carryPatchHook = "carry_patch"

// composeCarryPatch adds the carry-patch work: the schema of the patch
// lists and of the remembered-resolution entries, and the component,
// registered with the workspace service as the post-clone step of the
// workspaces in mode carry_patch, so that every fresh copy of one is set up
// (its remembered-resolution entries forgotten, upstream fetched,
// remembered resolutions on, the integration branch written) before the
// workspace is ready, and as the patch-branch lookup of the push route,
// which leaves the patch branches to the patch list, without the workspace
// package knowing the mode's work. A workspace's delete removes its patches
// and its remembered-resolution entries (ADR 07). The component is the
// hub's rebuild enqueuer, through which every source of a rebuild queues
// one as a job of the hub's queue. The component's routes, those of the
// patch list and of the rebuild jobs, are registered with the hub's router.
// Its start hook binds the component to the store, the secret component,
// which holds the upstream and origin credentials, the hub's git, the place
// of each copy on disk, the workspace settings registry, through which a
// patch branch's resolution reads patch_branch_source, the event component,
// which writes an add's event in its transaction (ADR 05), the job queue,
// which the jobs part's hook binds before this one, and the hub's clock and
// logger, after every adjuster
// AdjustCarryPatch added has changed those dependencies. The hook runs
// after the workspace part's, and like it before the clone workers launch,
// so no clone runs the step before the component is bound.
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
	c.workspaces.Teardown().RegisterRemover(carrypatch.RemoveWorkspace)
	c.workspaces.Teardown().RegisterRemover(carrypatch.RemoveRerereEntries)
	comp.RegisterRebuildEnqueuer(comp)
	for _, rt := range comp.Routes() {
		c.AddRoute(rt)
	}
	c.AddStartHook(carryPatchHook, func(_ context.Context, rt *Runtime) error {
		d := carrypatch.Deps{
			Store:    rt.Store,
			Secrets:  c.Secrets(),
			Git:      gitFor(c.env),
			RepoPath: c.workspaces.RepoPath,
			Settings: c.workspaces.Settings(),
			Events:   c.Events(),
			Jobs:     c.Jobs(),
			Now:      rt.Now,
			Logger:   rt.Logger,
		}
		for _, adjust := range c.carryPatchAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// CarryPatch returns the hub's carry-patch component, which serves once its
// start hook has run.
func (c *Composition) CarryPatch() *carrypatch.Component { return c.carryPatch }

// AdjustCarryPatch adds fn to the functions that change the carry-patch
// component's dependencies after the start hook has built them and before
// it binds them, in the order they were added.
func (c *Composition) AdjustCarryPatch(fn func(*carrypatch.Deps)) {
	c.carryPatchAdjusters = append(c.carryPatchAdjusters, fn)
}
