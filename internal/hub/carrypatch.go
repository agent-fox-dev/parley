package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// carryPatchHook is the name of the start hook that binds the carry-patch
// component.
const carryPatchHook = "carry_patch"

// composeCarryPatch adds the carry-patch work: the component, registered
// with the workspace service as the post-clone step of the workspaces in
// mode carry_patch, so that every fresh copy of one is set up (upstream
// fetched, remembered resolutions on, the integration branch written)
// before the workspace is ready, without the workspace package knowing the
// mode's work. Its start hook binds the component to the store, the secret
// component, which holds the upstream and origin credentials, the hub's git
// and the place of each copy on disk, after every adjuster
// AdjustCarryPatch added has changed those dependencies. The hook runs
// after the workspace part's, and like it before the clone workers launch,
// so no clone runs the step before the component is bound.
//
// The component exists from the composition on (CarryPatch); tests change
// its dependencies with AdjustCarryPatch.
func composeCarryPatch(c *Composition) {
	comp := &carrypatch.Component{}
	c.carryPatch = comp
	c.workspaces.RegisterPostCloneStep(workspace.ModeCarryPatch, comp.PostClone)
	c.AddStartHook(carryPatchHook, func(_ context.Context, rt *Runtime) error {
		d := carrypatch.Deps{
			Store:    rt.Store,
			Secrets:  c.Secrets(),
			Git:      gitFor(c.env),
			RepoPath: c.workspaces.RepoPath,
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
