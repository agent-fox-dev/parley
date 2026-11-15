package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// gitServerHook is the name of the start hook that binds the git server,
// and pushJournalHook that of the one that settles the push journal.
const (
	gitServerHook   = "gitserver"
	pushJournalHook = "push-journal"
)

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
// the fork view, which names those refs from the fork's record and the
// changes from the fork from the change component, to the
// event log, which receives the event of an audited fetch, to the
// workspace service's settings registry, from which a workspace's
// audit_fetches is read, and to [git].timeout, which it registers and
// which bounds each run, and each push up to its ref transaction. The server,
// and with it the limiter, is built after the start hooks, so the limiter
// reaches the component through a server hook: the component holds a
// serverLimiter, which the hook points at the limiter of the server that
// serves.
//
// The same start hook binds the push path. The ref writer (RefWriter) is
// bound to the store, in whose transactions its ref-updated stages run and
// its push journal is kept, to the event log, which receives the
// parley.git.push events, to the head_sha setter, which records the new
// head of a repository's default branch on its record, to the place of
// each repository on disk, opened with the hub's git, and to the hub's
// logger. The push journal's schema step is registered here, and a second
// start hook, after the first and before the hub serves, has the ref writer
// settle it: the events of a request whose refs moved before the hub
// stopped are written then. The push rule
// engine (PushRules) is bound to the settings reader, which reads a
// workspace's effective protected_branches and protect.owner_push from its
// record, a fork's from its parent's, and to the tracking-branch lookup,
// which asks the fork component. The push processor (Receiver) is bound to
// the engine, the ref writer, the hub's git, the workspace service's
// place of each repository on disk and [git].max_push_size, which the part
// registers beside [git].timeout, and is the git server's Push, so that
// a push that passed the checks is decided by the rules and written by the
// ref writer. Its options stage (PushOptions) is registered when the part is
// composed and bound to the hub's logger in the same start hook: it refuses
// a push with invalid options before its pack is read and hands on the
// session of parley.session.
//
// The component exists from the composition on (GitServer); tests change
// its dependencies with AdjustGitServer.
func composeGitServer(c *Composition) {
	comp := &gitserver.Component{}
	c.gitServer = comp
	c.pushOptions = &gitserver.PushOptionsStage{}
	c.receiver.RegisterOptionsStage(c.pushOptions)
	comp.Mount(c.Handle)
	if err := gitserver.Register(c.registry); err != nil {
		c.errs = append(c.errs, err)
	}
	limiter := &serverLimiter{}
	c.addServerHook(func(srv *server.Server) { limiter.l = srv.RateLimiter() })
	c.AddStartHook(gitServerHook, func(_ context.Context, rt *Runtime) error {
		c.refWriter.Bind(pushrule.WriterDeps{
			Transact: rt.Store.Tx, Events: c.Events(), Heads: headSetter{rt.Store}, Logger: rt.Logger,
			Repos: func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
				return gitFor(c.env).Open(ctx, c.workspaces.RepoPath(slug))
			},
		})
		c.pushRules.Bind(pushrule.EngineDeps{
			Settings: pushSettings{rt.Store.Main(), c.workspaces.Settings()}, Tracking: trackingBranches{c.forks},
		})
		c.pushOptions.Bind(rt.Logger)
		c.receiver.Bind(gitserver.ReceiverDeps{
			Engine: c.pushRules, Writer: c.refWriter, Git: gitFor(c.env), RepoPath: c.workspaces.RepoPath,
			MaxPushSize: gitserver.MaxPushSizeFrom(rt.Config), Logger: rt.Logger,
		})
		d := gitserver.Deps{
			Credentials: c.identity, Limiter: limiter, Guard: c.access, Access: c.access,
			Records: gitserver.StoreRecords(rt.Store.Main()), Parents: c.forks,
			Repositories: gitserver.DiskRepositories(gitFor(c.env), c.workspaces.RepoPath),
			Runner:       gitFor(c.env), Timeout: gitserver.TimeoutFrom(rt.Config), Logger: rt.Logger,
			ForkView: forkView{rt.Store.Main(), c.changes}, Copier: gitFor(c.env),
			Events: c.Events(), Settings: c.workspaces.Settings(), Push: c.receiver,
		}
		for _, adjust := range c.gitServerAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
	c.AddSchemaStep(pushrule.JournalStep)
	c.AddSchemaStep(pushrule.JournalEventsStep)
	c.AddStartHook(pushJournalHook, func(ctx context.Context, _ *Runtime) error { return c.refWriter.Reconcile(ctx) })
}

// headSetter is the ref writer's head_sha setter: it writes the head on the
// workspace's record in a transaction of its own, which the writer opens
// after its own has committed.
type headSetter struct{ st *store.Store }

func (h headSetter) SetHead(ctx context.Context, slug, id string) error {
	return h.st.Tx(ctx, func(tx *sql.Tx) error { return workspace.RecordHead(ctx, tx, slug, id) })
}

// pushSettings is the push rule engine's settings reader: the effective
// protected_branches and protect.owner_push of a workspace, read from its
// record at the moment of the push, a task fork's from its parent's, since
// a fork has no settings of its own.
type pushSettings struct {
	db       *sql.DB
	registry *workspace.SettingsRegistry
}

func (s pushSettings) PushSettings(ctx context.Context, ws pushrule.Workspace) (pushrule.Settings, error) {
	slug := ws.Slug
	if ws.Kind == access.KindFork {
		slug = ws.Parent
	}
	rec, found, err := workspace.BySlug(ctx, s.db, slug)
	if err != nil {
		return pushrule.Settings{}, err
	}
	if !found {
		return pushrule.Settings{}, fmt.Errorf("push settings: workspace %q not found", slug)
	}
	eff := s.registry.EffectiveSettings(rec.Settings, rec.DefaultBranch)
	globs, globsOK := eff["protected_branches"].([]string)
	ownerPush, ownerPushOK := eff["protect.owner_push"].(bool)
	if !globsOK || !ownerPushOK {
		return pushrule.Settings{}, fmt.Errorf("push settings of %q cannot be read", slug)
	}
	return pushrule.Settings{ProtectedBranches: globs, OwnerPush: ownerPush}, nil
}

// trackingBranches is the push rule engine's tracking-branch lookup: the
// fork component's answer whether a branch of a fork may be written, whose
// tracking_branch refusal names a tracking branch; any other error is a
// failed lookup.
type trackingBranches struct{ forks *fork.Component }

func (t trackingBranches) IsTracking(ctx context.Context, forkSlug, branch string) (bool, error) {
	err := t.forks.CheckBranchWrite(ctx, forkSlug, branch)
	var refusal *server.Error
	switch {
	case err == nil:
		return false, nil
	case errors.As(err, &refusal) && refusal.Type() == "tracking_branch":
		return true, nil
	}
	return false, err
}

// forkView is the git server's fork view: a fork's tracking refs are the
// branches and tags its record holds, the names the fork feature recorded
// when it created the fork, and its changes are those, in any state, whose
// source is the fork, as the change component records them, whose version
// refs the fork shows from its parent's repository.
type forkView struct {
	db      *sql.DB
	changes *change.Component
}

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
	from, err := v.changes.ChangeNumbersOfSource(ctx, v.db, slug)
	if err != nil {
		return gitserver.ForkRefs{}, err
	}
	for _, s := range from {
		refs.Changes = append(refs.Changes, s.Number)
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
// collaborators then: its ref-updated stages, the fork activity recorder
// and its post-push listeners.
func (c *Composition) RefWriter() *pushrule.Writer { return c.refWriter }

// PushRules returns the hub's push rule engine, which decides the ref
// updates of every surface that writes refs once the git server's start
// hook has bound it. A part keeps it when it is composed and registers its
// collaborators then, such as the carry-patch controller.
func (c *Composition) PushRules() *pushrule.Engine { return c.pushRules }

// Receiver returns the hub's push processor behind git-receive-pack, the
// git server's Push once its start hook has run. A part keeps it when it is
// composed and registers its options stage and its mapping stage then.
func (c *Composition) Receiver() *gitserver.Receiver { return c.receiver }

// PushOptions returns the hub's options stage, the push processor's from
// the composition on, which judges a push's options before its pack is
// read. The part that owns the session-linking rule registers its session
// linker on it when it is composed; until one does, every parley.session
// is refused.
func (c *Composition) PushOptions() *gitserver.PushOptionsStage { return c.pushOptions }

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
