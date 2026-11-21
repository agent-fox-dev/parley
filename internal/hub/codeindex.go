package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/codeindex"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// codeIndexHook is the start hook that binds the code index component and,
// on a hub with [code_index], refuses a platform the index cannot run on.
const codeIndexHook = "code-index"

// composeCodeIndex composes the code index: the trees and indexes of the
// workspaces' default branches (package codeindex), built by jobs of the
// type codeindex.JobType in the hub's queue, at most [code_index].workers
// at once. A hub whose configuration has no [code_index] gets the
// component and nothing it does: no handler is registered, so nothing
// builds.
//
// The component exists from the composition on (CodeIndex); tests change
// its dependencies with AdjustCodeIndex.
func composeCodeIndex(c *Composition) {
	comp := &codeindex.Component{}
	c.codeIndex = comp
	c.AddSchemaStep(codeindex.SchemaStep)
	c.AddStartHook(codeIndexHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		settings := c.workspaces.Settings()
		d := codeindex.Deps{
			Settings: rt.Config.CodeIndex,
			DataDir:  rt.Config.DataDir(),
			Store:    rt.Store,
			Jobs:     c.Jobs(),
			Workspace: func(ctx context.Context, slug string) (codeindex.Workspace, bool, error) {
				w, found, err := workspace.BySlug(ctx, rt.Store.Main(), slug)
				if err != nil || !found {
					return codeindex.Workspace{}, false, err
				}
				ws := codeindex.Workspace{Slug: w.Slug, Repository: w.Kind == access.KindRepository}
				if w.DefaultBranch != nil {
					ws.DefaultBranch = *w.DefaultBranch
				}
				ws.Indexed, _ = settings.EffectiveSettings(w.Settings, w.DefaultBranch)[codeindex.SettingName].(bool)
				return ws, true, nil
			},
			Repos: func(ctx context.Context, slug string) (codeindex.Repository, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			Metrics: c.metrics,
			Scrub:   func(text string) string { return rt.Scrubber.Scrub(text) },
			Now:     rt.Now,
		}
		for _, adjust := range c.codeIndexAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		if !comp.Enabled() {
			return nil
		}
		if err := comp.CheckPlatform(); err != nil {
			return err
		}
		// The builds are bounded apart from whatever else the queue runs.
		c.Jobs().Limit(codeindex.JobType, rt.Config.CodeIndex.Workers)
		c.Jobs().Register(codeindex.JobType, comp.Handler())
		return nil
	})
}

// CodeIndex returns the hub's code index component, which works once its
// start hook has bound it.
func (c *Composition) CodeIndex() *codeindex.Component { return c.codeIndex }

// AdjustCodeIndex adds fn to the functions that change the code index
// component's dependencies after the start hook has built them and before
// it binds them, in the order they were added.
func (c *Composition) AdjustCodeIndex(fn func(*codeindex.Deps)) {
	c.codeIndexAdjusters = append(c.codeIndexAdjusters, fn)
}
