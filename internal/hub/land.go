package hub

import (
	"context"
	"errors"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// landHook is the name of the start hook that binds the land component;
// landJournalHook the name of the one after it that completes or drops
// the lands a stop left in the land journal; landRecoverHook the name of
// the last, which hands the attempts a stop left running to the pool to
// run them again.
const (
	landHook        = "land"
	landJournalHook = "land-journal"
	landRecoverHook = "land-recover"
)

// landPoolWork is the name of the background work that runs the land
// attempts; landPoolGrace is how long the stop waits for it, which ends
// the git processes of the running attempts at once and leaves them
// running for the next start.
const (
	landPoolWork  = "land_pool"
	landPoolGrace = 5 * time.Second
)

// composeLand adds the landing: the schema of the land attempts, the land
// route and the read routes of the attempts per change, per workspace and
// by id, whose scope and access need the shared checks decide, and the
// component, bound in a
// start hook to the store, to the place of each repository on disk, opened
// with the hub's git, to the change component, to the workspace records
// with their effective land settings, read in the caller's transaction, to
// the access rule, to the hub's event log, to the ref writer and to the
// [land] options: the tries of one land, the workers of the pool, the
// deadline of an attempt and the landing identity. The pool runs as
// background work; the stop leaves its running attempts running, and the
// next start, once the land journal is reconciled and before the listener
// binds, hands them to the pool again, which runs them while requests are
// served. A test starts the hub with the pool paused by setting
// land.Deps.Paused through AdjustLand: the attempts it is handed stay
// running until a start without it. No
// session linker is registered: until the session feature registers one,
// every session_id a land names is refused.
//
// Two after-landing hooks are registered here for the features built
// before the landing: the change feature's entry point for a landed change
// (OnLanded), which unlinks each landed change's dependants and restacks
// them onto its landed commit, and the fork feature's activity recorder
// (RecordActivity), told of each landed change whose source is a task
// fork. Both open their own store transaction; the hooks are called
// outside any. No policy gate, override rule, request guard, target guard
// or final observer is registered: every change is ready to land, an
// override passes nothing and every request and move is allowed.
//
// The source control API's session lookup (commitSessions) is set here
// through AdjustSCM, since it asks the change records and the land
// attempts, which hold the sessions of the writes that introduced commits.
// The landing's attribution of a conflict to the landed changes it
// collided with (Attribute) is handed, as a function value, to the source
// control API through AdjustSCM and to the change component through
// AdjustChange, which cannot import the landing: the comparison, the
// server-side rebase, the mergeability check and the restack thus name
// the landed changes in their conflict reports as a land does.
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
	c.AdjustSCM(func(d *scm.Deps) { d.Sessions, d.Attribute = commitSessions{c.changes, comp}, comp.Attribute })
	c.AdjustChange(func(d *change.Deps) { d.Attribute = comp.Attribute })
	comp.AddAfterLanding(func(ctx context.Context, in land.AfterLanding) error {
		var errs []error
		for _, ch := range in.Changes {
			errs = append(errs, c.changes.OnLanded(ctx, in.Workspace, ch.Number, ch.LandedSHA))
		}
		return errors.Join(errs...)
	})
	comp.AddAfterLanding(func(ctx context.Context, in land.AfterLanding) error {
		var errs []error
		for _, ch := range in.Changes {
			if ch.SourceWorkspace != in.Workspace {
				errs = append(errs, c.forks.RecordActivity(ctx, ch.SourceWorkspace))
			}
		}
		return errors.Join(errs...)
	})
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

			MaxAttempts: rt.Config.Land.MaxAttempts,
			Workers:     rt.Config.Land.Workers,
			Timeout:     rt.Config.Land.Timeout,
			Committer:   land.Committer{Name: rt.Config.Land.CommitterName, Email: rt.Config.Land.CommitterEmail},
		}
		for _, adjust := range c.landAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
	c.AddStartHook(landJournalHook, func(ctx context.Context, _ *Runtime) error { return comp.Reconcile(ctx) })
	c.AddStartHook(landRecoverHook, func(ctx context.Context, _ *Runtime) error { return comp.Recover(ctx) })
	c.AddBackgroundWork(landPoolWork, landPoolGrace, func(ctx context.Context, _ *Runtime) { comp.RunPool(ctx) })
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

// commitSessions is the source control API's session lookup: the session
// of the write that introduced a commit. A commit that is the head of a
// change's version was introduced by the first write that recorded it, a
// push, an API commit or a rebase, in its session; a commit a land wrote
// in place of a version's commit, by the land, in the session it was asked
// in. Any other commit, such as one below a version's head, has no session
// recorded for it.
type commitSessions struct {
	changes *change.Component
	lands   *land.Component
}

func (s commitSessions) SessionOf(ctx context.Context, slug, sha string) (*string, error) {
	session, found, err := s.changes.SessionOfHead(ctx, nil, slug, sha)
	if err != nil || found {
		return session, err
	}
	cc, found, err := s.changes.ChangeOfCommit(ctx, nil, slug, sha)
	if err != nil || !found || !cc.Landed || cc.LandedFrom == sha {
		return nil, err
	}
	session, _, err = s.lands.SessionOfLanding(ctx, cc.WorkspaceSlug, cc.Number)
	return session, err
}
