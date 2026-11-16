package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// landHook is the name of the start hook that binds the land component;
// landJournalHook the name of the one after it that completes or drops
// the lands a stop left in the land journal.
const (
	landHook        = "land"
	landJournalHook = "land-journal"
)

// composeLand adds the landing: the schema of the land attempts, the read
// routes of the attempts per change, per workspace and by id, whose scope
// and access need the shared checks decide, and the component, bound in a
// start hook to the store, to the place of each repository on disk, opened
// with the hub's git, to the change component, to the workspace records
// with their effective land settings, read in the caller's transaction, to
// the access rule, to the hub's event log and to the ref writer.
//
// The teardown of a workspace deletes its land attempts in the delete's
// transaction (docs/adr/07-register-the-teardown-collaborators-on-the-workspace-service.md).
//
// The component exists from the composition on (Land); tests change its
// dependencies with AdjustLand.
func composeLand(c *Composition) {
	comp := &land.Component{}
	c.land = comp
	c.AddSchemaStep(land.SchemaStep)
	c.AddSchemaStep(land.JournalStep)
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.workspaces.Teardown().RegisterRemover(comp.WorkspaceDeleted)
	c.AddStartHook(landHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := land.Deps{
			Store: rt.Store,
			Repos: func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			Changes:    c.changes,
			Workspaces: landWorkspaces{c.workspaces.Settings()},
			Access:     c.access,
			Events:     c.events,
			RefWriter:  c.refWriter,
			Now:        rt.Now,
			Logger:     rt.Logger,
		}
		for _, adjust := range c.landAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
	c.AddStartHook(landJournalHook, func(ctx context.Context, _ *Runtime) error { return comp.Reconcile(ctx) })
}

// landWorkspaces reads the workspace records for the land component, with
// the effective land settings the registry derives.
type landWorkspaces struct{ settings *workspace.SettingsRegistry }

func (r landWorkspaces) Workspace(ctx context.Context, q land.Querier, slug string) (land.Workspace, bool, error) {
	w, found, err := workspace.BySlug(ctx, q, slug)
	if err != nil || !found {
		return land.Workspace{}, found, err
	}
	out := land.Workspace{Slug: w.Slug, Kind: w.Kind, Mode: w.Mode, Ready: workspace.RequireReady(w)}
	for dst, src := range map[*string]*string{&out.Parent: w.ParentSlug, &out.DefaultBranch: w.DefaultBranch} {
		if src != nil {
			*dst = *src
		}
	}
	settings := r.settings.EffectiveSettings(w.Settings, w.DefaultBranch)
	if s, ok := settings["land.strategy"].(string); ok {
		out.Strategy = s
	}
	if on, ok := settings["land.by_author"].(bool); ok {
		out.ByAuthor = on
	}
	if on, ok := settings["land.delete_source"].(bool); ok {
		out.DeleteSource = on
	}
	return out, true, nil
}

// Land returns the hub's land component, which serves once its start hook
// has run.
func (c *Composition) Land() *land.Component { return c.land }

// AdjustLand adds fn to the functions that change the land component's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added.
func (c *Composition) AdjustLand(fn func(*land.Deps)) {
	c.landAdjusters = append(c.landAdjusters, fn)
}
