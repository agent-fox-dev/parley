package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// forgeHook is the name of the start hook that binds the forge component.
const forgeHook = "forge"

// composeForge adds the forge component, the hub's client of the forge
// that hosts a workspace's remote repository. Its start hook binds it to
// the workspace records in the store, the workspace settings, the secret
// component, through which a forge credential is read and registered with
// the scrubber, and the access rule, which decides a task fork's caller's
// access to the fork's parent; and to the options of its client: the hub's
// environment, from which the proxy variables are read (the process
// environment never is, docs/adr/06-load-the-secret-keys-before-the-stores-open.md);
// the client takes the hub's clock. It hands it the hub's scrubber, logger
// and external URL too.
//
// The component exists from the composition on (Forge), so that the forge
// routes are built with it; a later part and tests change its dependencies
// with AdjustForge.
func composeForge(c *Composition) {
	comp := &forge.Component{}
	c.forge = comp
	c.AddStartHook(forgeHook, func(_ context.Context, rt *Runtime) error {
		db := rt.Store.Main()
		d := forge.Deps{
			Workspaces: func(ctx context.Context, slug string) (workspace.Workspace, bool, error) {
				return workspace.BySlug(ctx, db, slug)
			},
			Settings:    c.workspaces.Settings(),
			Secrets:     c.secrets,
			Access:      c.access,
			Client:      forge.ClientOptions{Env: c.env},
			Scrubber:    rt.Scrubber,
			Logger:      rt.Logger,
			Now:         rt.Now,
			ExternalURL: rt.Config.Server.ExternalURL,
		}
		for _, adjust := range c.forgeAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// Forge returns the hub's forge component, which serves once its start
// hook has run.
func (c *Composition) Forge() *forge.Component { return c.forge }

// AdjustForge adds fn to the functions that change the forge component's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added. Tests replace a collaborator with it,
// such as the client's transport or the secret reader.
func (c *Composition) AdjustForge(fn func(*forge.Deps)) {
	c.forgeAdjusters = append(c.forgeAdjusters, fn)
}
