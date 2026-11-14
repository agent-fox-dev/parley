package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// workspacesHook is the name of the start hook that binds the workspace
// service.
const workspacesHook = "workspaces"

// composeWorkspaces adds the workspace fields to the workspace records of
// the access part and registers the workspace routes. Its start hook binds
// the workspace service to the stores, the secret and event components of
// the parts composed before it, the hub's scrubber, logger and clock,
// [server].external_url and [workspace].path, after every adjuster
// AdjustWorkspace added has changed those dependencies. The service exists
// from the composition on, so a later part keeps it through
// Composition.Workspaces and registers its setting keys on
// Workspaces().Settings() when it is composed.
func composeWorkspaces(c *Composition) {
	svc := &workspace.Service{}
	c.workspaces = svc
	c.AddSchemaStep(workspace.SchemaStep)
	for _, r := range svc.Routes() {
		c.AddRoute(r)
	}
	c.AddStartHook(workspacesHook, func(_ context.Context, rt *Runtime) error {
		d := workspace.Deps{
			Store:       rt.Store,
			Secrets:     c.Secrets(),
			Events:      c.Events(),
			Scrubber:    rt.Scrubber,
			Logger:      rt.Logger,
			Now:         rt.Now,
			ExternalURL: rt.Config.Server.ExternalURL,
			Root:        rt.Config.Workspace.Path,
		}
		for _, adjust := range c.workspaceAdjusters {
			adjust(&d)
		}
		svc.Bind(d)
		return nil
	})
}

// Workspaces returns the hub's workspace service. A part keeps it when it is
// composed; the service serves once its start hook has run.
func (c *Composition) Workspaces() *workspace.Service { return c.workspaces }

// AdjustWorkspace adds fn to the functions that change the workspace
// service's dependencies after the start hook has built them and before it
// binds them, in the order they were added. Tests replace a collaborator
// with it, such as the origin lister or the clone notification.
func (c *Composition) AdjustWorkspace(fn func(*workspace.Deps)) {
	c.workspaceAdjusters = append(c.workspaceAdjusters, fn)
}
