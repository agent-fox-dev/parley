package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// workspacesHook is the name of the start hook that binds the workspace
// service.
const workspacesHook = "workspaces"

// gitPassthrough are the variables of the hub's environment that every git
// process gets: an operator whose origins sit behind a private certificate
// authority or a proxy sets them for parleyd as for git itself. Nothing else
// of the hub's environment reaches git.
var gitPassthrough = []string{"GIT_SSL_CAINFO", "SSL_CERT_FILE", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"}

// gitFor is the git the hub runs with the environment env: the variables of
// gitPassthrough that env sets, added to gitrepo's minimal environment.
func gitFor(env config.Env) gitrepo.Git {
	var vars []string
	for _, name := range gitPassthrough {
		if v, ok := env(name); ok {
			vars = append(vars, name+"="+v)
		}
	}
	return gitrepo.Git{Env: vars}
}

// originLister checks a credential against an origin by listing its refs
// with git.
type originLister struct{ git gitrepo.Git }

func (l originLister) ListOrigin(ctx context.Context, url string, cred workspace.Credential) error {
	_, err := l.git.ListRemote(ctx, url, cred)
	return err
}

// composeWorkspaces adds the workspace fields to the workspace records of
// the access part and registers the workspace routes. Its start hook binds
// the workspace service to the stores, the secret and event components of
// the parts composed before it, the hub's git, which lists origins and
// makes the workspaces' copies, the hub's scrubber, logger and clock,
// [server].external_url and [workspace].path, where the copies live, after every adjuster AdjustWorkspace added has changed
// those dependencies. The service exists from the composition on, so a
// later part keeps it through Composition.Workspaces and registers its
// setting keys on Workspaces().Settings() when it is composed.
func composeWorkspaces(c *Composition) {
	svc := &workspace.Service{}
	c.workspaces = svc
	// One git for the part, so that every call it makes to an origin runs
	// with the same environment.
	git := gitFor(c.env)
	c.AddSchemaStep(workspace.SchemaStep)
	for _, r := range svc.Routes() {
		c.AddRoute(r)
	}
	c.AddStartHook(workspacesHook, func(_ context.Context, rt *Runtime) error {
		d := workspace.Deps{
			Store:       rt.Store,
			Secrets:     c.Secrets(),
			Events:      c.Events(),
			Lister:      originLister{git},
			Git:         git,
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
