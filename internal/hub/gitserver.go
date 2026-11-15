package hub

import (
	"context"
	"database/sql"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// gitServerHook is the name of the start hook that binds the git server.
const gitServerHook = "gitserver"

// composeGitServer adds the git server: it mounts the three git routes
// under /git on the server's router, outside the REST API, so that they
// pass none of its checks, and binds the component in a start hook, before
// the hub serves its first request, to the identity service, which checks
// the Basic password as it checks a bearer credential, to the server's
// own rate limiter, so that a credential has one window across git and the
// REST API, to the access service, whose binding guard and access rule
// decide a git request as they decide a REST request, to the store the
// repository's organization and workspace record are read from, to the
// fork component, whose parent check judges a fork's parent as it does for
// every route that works on a fork, and to the workspace service's place
// of each repository on disk, which is opened with the hub's git before any
// exchange, to the hub's git, which runs upload-pack for a fetch and
// receive-pack's advertisement for a push, and copies a fork's tracking
// refs from its parent's repository into the fork's, to
// the fork view, which names those refs from the fork's record, to the
// event log, which receives the event of an audited fetch, to the
// workspace service's settings registry, from which a workspace's
// audit_fetches is read, and to [git].timeout, which it registers and
// which bounds each run. The server,
// and with it the limiter, is built after the start hooks, so the limiter
// reaches the component through a server hook: the component holds a
// serverLimiter, which the hook points at the limiter of the server that
// serves.
//
// The same start hook binds the ref writer (RefWriter) to the store, in
// whose transactions its ref-updated stages run, to the head_sha setter,
// which records the new head of a repository's default branch on its
// record, and to the hub's logger. No push processor is installed yet, so
// nothing writes through it over git: a push that passes the checks is
// answered 500 internal error, its body unread and git not run.
//
// The component exists from the composition on (GitServer); tests change
// its dependencies with AdjustGitServer.
func composeGitServer(c *Composition) {
	comp := &gitserver.Component{}
	c.gitServer = comp
	comp.Mount(c.Handle)
	if err := gitserver.Register(c.registry); err != nil {
		c.errs = append(c.errs, err)
	}
	limiter := &serverLimiter{}
	c.addServerHook(func(srv *server.Server) { limiter.l = srv.RateLimiter() })
	c.AddStartHook(gitServerHook, func(_ context.Context, rt *Runtime) error {
		d := gitserver.Deps{
			Credentials: c.identity, Limiter: limiter, Guard: c.access, Access: c.access,
			Records: gitserver.StoreRecords(rt.Store.Main()), Parents: c.forks,
			Repositories: gitserver.DiskRepositories(gitFor(c.env), c.workspaces.RepoPath),
			Runner:       gitFor(c.env), Timeout: gitserver.TimeoutFrom(rt.Config), Logger: rt.Logger,
			ForkView: forkView{rt.Store.Main()}, Copier: gitFor(c.env),
			Events: c.Events(), Settings: c.workspaces.Settings(),
		}
		for _, adjust := range c.gitServerAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		c.refWriter.Bind(pushrule.WriterDeps{Transact: rt.Store.Tx, Heads: headSetter{rt.Store}, Logger: rt.Logger})
		return nil
	})
}

// headSetter is the ref writer's head_sha setter: it writes the head on the
// workspace's record in a transaction of its own, which the writer opens
// after its own has committed.
type headSetter struct{ st *store.Store }

func (h headSetter) SetHead(ctx context.Context, slug, id string) error {
	return h.st.Tx(ctx, func(tx *sql.Tx) error { return workspace.RecordHead(ctx, tx, slug, id) })
}

// forkView is the git server's fork view: a fork's tracking refs are the
// branches and tags its record holds, the names the fork feature recorded
// when it created the fork. It names no change: which changes have a fork
// as their source is recorded by the change feature, which the hub does not
// have yet, so a fork advertises no version refs, and the copy deletes any
// refs/parley/changes/ ref from the fork's repository.
type forkView struct{ db *sql.DB }

func (v forkView) ForkRefs(ctx context.Context, slug string) (gitserver.ForkRefs, error) {
	f, found, err := fork.BySlug(ctx, v.db, slug)
	if err != nil {
		return gitserver.ForkRefs{}, err
	}
	if !found {
		return gitserver.ForkRefs{}, fork.ErrNotFork
	}
	var refs gitserver.ForkRefs
	for _, b := range f.TrackingBranches {
		refs.Tracking = append(refs.Tracking, "refs/heads/"+b)
	}
	for _, t := range f.TrackingTags {
		refs.Tracking = append(refs.Tracking, "refs/tags/"+t)
	}
	return refs, nil
}

// serverLimiter is the rate limiter of the HTTP server the hub serves with,
// which exists only once the server is built. install sets l before the
// server serves its first request.
type serverLimiter struct{ l *server.Limiter }

func (s *serverLimiter) Allow(key string) (bool, int) { return s.l.Allow(key) }

// RefWriter returns the hub's ref writer, which writes the refs of every
// surface that moves a workspace's refs once the git server's start hook
// has bound it. A part keeps it when it is composed and registers its
// collaborators then: its ref-updated stages and the fork activity
// recorder.
func (c *Composition) RefWriter() *pushrule.Writer { return c.refWriter }

// GitServer returns the hub's git server, which serves the three git routes
// once its start hook has run.
func (c *Composition) GitServer() *gitserver.Component { return c.gitServer }

// AdjustGitServer adds fn to the functions that change the git server's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added. Tests replace a collaborator with it,
// such as the service that runs after the checks.
func (c *Composition) AdjustGitServer(fn func(*gitserver.Deps)) {
	c.gitServerAdjusters = append(c.gitServerAdjusters, fn)
}
