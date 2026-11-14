package hub

import (
	"context"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// workspacesHook is the name of the start hook that binds the workspace
// service and queues the clone work left from before the start.
const workspacesHook = "workspaces"

// cloneWork is the name of the background work that runs the clone workers
// while the hub serves and stops them when the hub stops; cloneStopGrace is
// how long the stop waits for them. A running clone ends as soon as its git
// process is killed, so the grace only covers the cleanup after it.
const (
	cloneWork      = "workspace_clones"
	cloneStopGrace = 10 * time.Second
)

// gitPassthrough are the variables of the hub's environment that every git
// process gets: an operator whose origins sit behind a private certificate
// authority or a proxy sets them for parleyd as for git itself. Nothing else
// of the hub's environment reaches git but PATH, which finds it.
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
// the access part and registers the workspace routes. Its start hook builds
// the clone workers, [workspace].workers of them, and binds the workspace
// service to the stores, the secret and event components of the parts
// composed before it, the hub's git, which lists origins and makes the
// workspaces' copies, the hub's scrubber, logger and clock,
// [server].external_url, [workspace].path, where the copies live, and the
// workers, which are told about every workspace created, after every
// adjuster AdjustWorkspace added has changed those dependencies. It then
// has the workers queue the clone work left from before the start, so that
// it is queued before the hub listens; a refused number of workers stops the
// start. The workers run in the background work, which begins only once the
// hub listens, so that a start that fails after the hook leaves no clone
// running over stores that are closed; they are stopped when the hub stops.
// The service
// exists from the composition on, so a later part keeps it through
// Composition.Workspaces and registers its setting keys on
// Workspaces().Settings() when it is composed.
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
	// Built by the start hook, which runs before the background work.
	var workers *workspace.Workers
	c.AddStartHook(workspacesHook, func(ctx context.Context, rt *Runtime) error {
		var err error
		if workers, err = workspace.NewWorkers(rt.Config.Workspace.Workers, svc, nil); err != nil {
			return err
		}
		d := workspace.Deps{
			Store:       rt.Store,
			Secrets:     c.Secrets(),
			Events:      c.Events(),
			Lister:      originLister{git},
			Git:         git,
			Notify:      workers.Enqueue,
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
		return workers.Start(ctx)
	})
	c.AddBackgroundWork(cloneWork, cloneStopGrace, func(ctx context.Context, rt *Runtime) {
		if err := workers.Launch(ctx); err != nil {
			rt.Logger.Error("workspace clone workers did not start", "error", err)
			return
		}
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), cloneStopGrace)
		defer cancel()
		if err := workers.Stop(stop); err != nil {
			rt.Logger.Warn("workspace clone workers did not stop", "error", err)
		}
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
