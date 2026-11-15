package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// scmHook is the name of the start hook that binds the source control
// API's read routes.
const scmHook = "scm"

// composeSCM adds the source control API: it registers the read and write
// routes under /workspaces/:slug/scm, whose scope and access need
// the shared checks decide, and binds the component in a start hook to the
// workspace records in the store; to the fork component's parent-state
// check for a read and for a write, so that a fork whose parent is busy or
// failed, or archived for a write, is refused as every route that works on
// a fork refuses it; to the place of
// each repository on disk, opened with the hub's git; to the ref view of a
// workspace, which for a fork is the fork component's (its own branches
// and its tracking branches and tags at the parent's current heads, the
// names the git server advertises) and for a repository workspace what its
// repository holds; and to the push rules' protected-branch test over the
// workspace's effective protected_branches, a fork's from its parent's, so
// that the reads call protected what a push meets as protected; and to the
// push rule engine and the ref writer of the git server's part, so that a
// write is decided and written exactly as a push of the same ref update.
// Each read is bounded by [scm].read_timeout and a file by
// [scm].max_file_size.
//
// The change, commit-change and session lookups keep their defaults, which
// name none: the hub has no change or session records to ask. The hub
// registers no request recorder; the default discards the observations.
//
// The component exists from the composition on (SCM); tests change its
// dependencies with AdjustSCM.
func composeSCM(c *Composition) {
	comp := &scm.Component{}
	c.scm = comp
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.AddStartHook(scmHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		db := rt.Store.Main()
		d := scm.Deps{
			Workspaces: func(ctx context.Context, slug string) (workspace.Workspace, bool, error) {
				return workspace.BySlug(ctx, db, slug)
			},
			ParentState: func(ctx context.Context, slug string) error {
				return c.forks.CheckParentState(ctx, slug, fork.UseRead)
			},
			WriteParentState: func(ctx context.Context, slug string) error {
				return c.forks.CheckParentState(ctx, slug, fork.UseWrite)
			},
			Open: func(ctx context.Context, slug string) (scm.Repository, error) {
				repo, err := git.Open(ctx, c.workspaces.RepoPath(slug))
				if err != nil {
					return nil, err
				}
				return repo, nil
			},
			RefView: func(_ context.Context, w workspace.Workspace, repo scm.Repository) scm.RefView {
				if w.Kind == access.KindFork {
					return forkRefs{c.forks, w.Slug}
				}
				return scm.RepoRefs(repo)
			},
			Protected:   protectedBranches{pushSettings{db, c.workspaces.Settings()}},
			Rules:       c.pushRules,
			RefWriter:   c.refWriter,
			ReadTimeout: rt.Config.SCM.ReadTimeout,
			MaxFileSize: rt.Config.SCM.MaxFileSize,
		}
		for _, adjust := range c.scmAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// forkRefs is the ref view of the fork slug: the fork component's, its
// branches with their tracking flag and its recorded tags, which the reads
// peel to the commits they reach. It names no refs/parley/ ref, as the git
// server's fork view names no change.
type forkRefs struct {
	forks *fork.Component
	slug  string
}

func (v forkRefs) Refs(ctx context.Context) ([]scm.Ref, error) {
	view, err := v.forks.Refs(ctx, v.slug)
	if err != nil {
		return nil, err
	}
	refs := make([]scm.Ref, 0, len(view.Branches)+len(view.Tags))
	for _, b := range view.Branches {
		refs = append(refs, scm.Ref{Name: "refs/heads/" + b.Name, SHA: b.SHA, Commit: b.SHA, Tracking: b.Tracking})
	}
	for _, t := range view.Tags {
		refs = append(refs, scm.Ref{Name: "refs/tags/" + t.Name, SHA: t.SHA})
	}
	return refs, nil
}

// protectedBranches is the reads' protected-branch lookup: the push rules'
// test over the protected_branches the push rule engine reads.
type protectedBranches struct{ settings pushSettings }

func (p protectedBranches) ProtectedBranches(ctx context.Context, w workspace.Workspace) (func(string) bool, error) {
	ws := pushrule.Workspace{Slug: w.Slug, Kind: w.Kind}
	if w.ParentSlug != nil {
		ws.Parent = *w.ParentSlug
	}
	s, err := p.settings.PushSettings(ctx, ws)
	if err != nil {
		return nil, err
	}
	return func(branch string) bool { return pushrule.Protected(branch, s.ProtectedBranches) }, nil
}

// SCM returns the hub's read routes of the source control API, which serve
// once their start hook has run.
func (c *Composition) SCM() *scm.Component { return c.scm }

// AdjustSCM adds fn to the functions that change the read routes'
// dependencies after the start hook has built them and before it binds
// them, in the order they were added. Tests replace a collaborator with it,
// such as the request recorder or the repository opener.
func (c *Composition) AdjustSCM(fn func(*scm.Deps)) {
	c.scmAdjusters = append(c.scmAdjusters, fn)
}
