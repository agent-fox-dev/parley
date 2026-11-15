package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// changeHook is the name of the start hook that binds the change component.
const changeHook = "changes"

// composeChanges adds the changes: the schema of the change record, the
// change routes under /workspaces/:slug/changes, whose scope and access
// need the shared checks decide, and the component, bound in a start hook to
// the store, to the place of each repository on disk, opened with the hub's
// git, to the workspace records with their effective settings, read in the
// caller's transaction, to the access rule, which Authorize asks, and to
// the hub's event log.
//
// No collaborator is registered on the component yet: without a session
// linker an explicit session_id is refused and a defaulted one dropped;
// without a stack linker no change depends on another; without a landing
// evaluator every open change is ready to land; and auto_land is stored as
// given.
//
// The component exists from the composition on (Changes); tests change its
// dependencies with AdjustChange.
func composeChanges(c *Composition) {
	comp := &change.Component{}
	c.changes = comp
	c.AddSchemaStep(change.SchemaStep)
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.AddStartHook(changeHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := change.Deps{
			Store: rt.Store,
			Repos: func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			Workspaces: changeWorkspaces{c.workspaces.Settings()},
			Access:     c.access,
			Events:     c.events,
			Now:        rt.Now,
			Logger:     rt.Logger,
		}
		for _, adjust := range c.changeAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// changeWorkspaces reads the workspace records for the change component,
// with the effective settings the registry derives.
type changeWorkspaces struct{ settings *workspace.SettingsRegistry }

func (r changeWorkspaces) Workspace(ctx context.Context, q change.Querier, slug string) (change.Workspace, bool, error) {
	w, found, err := workspace.BySlug(ctx, q, slug)
	if err != nil || !found {
		return change.Workspace{}, found, err
	}
	out := change.Workspace{
		Slug: w.Slug, Fork: w.Kind == access.KindFork, CarryPatch: w.Mode == workspace.ModeCarryPatch,
		Purpose: w.Purpose, SessionID: w.SessionID, Unusable: workspace.RequireReady(w),
	}
	for dst, src := range map[*string]*string{&out.Parent: w.ParentSlug, &out.IntegrationBranch: w.IntegrationBranch, &out.DefaultBranch: w.DefaultBranch} {
		if src != nil {
			*dst = *src
		}
	}
	if on, ok := r.settings.EffectiveSettings(w.Settings, w.DefaultBranch)["changes.require_intent"].(bool); ok {
		out.RequireIntent = on
	}
	return out, true, nil
}

// Changes returns the hub's change component, which serves once its start
// hook has run.
func (c *Composition) Changes() *change.Component { return c.changes }

// AdjustChange adds fn to the functions that change the change component's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added.
func (c *Composition) AdjustChange(fn func(*change.Deps)) {
	c.changeAdjusters = append(c.changeAdjusters, fn)
}
