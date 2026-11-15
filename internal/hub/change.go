package hub

import (
	"context"
	"database/sql"
	"errors"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// changeHook is the name of the start hook that binds the change component.
const changeHook = "changes"

// composeChanges adds the changes: the schema of the change record, the
// change routes under /workspaces/:slug/changes, whose scope and access
// need the shared checks decide, and the component, bound in a start hook to
// the store, to the place of each repository on disk, opened with the hub's
// git, to the workspace records with their effective settings, read in the
// caller's transaction, to the access rule, which Authorize asks, and to
// the hub's event log.
//
// It connects the component to the features that move a change's source
// or ask about changes, each registered on the instance that calls it
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md):
//
//   - the ref writer runs the component's ref-updated stage (RefUpdated) in
//     the store transaction of every write of refs through git or the source
//     control API, which records the version a moved source branch makes
//     and abandons the change of a deleted one before the refs move;
//   - the source control API's writes name, check and open changes through
//     the change linker, which opens in a store transaction of its own, and
//     its reads name the change of a branch or a commit through its change
//     lookups;
//   - a branch a sync moved records a version of kind sync by the hub, in a
//     store transaction of its own, since the sync tells its observers
//     outside one;
//   - the teardown of a workspace, and of each of its forks, abandons the
//     changes whose source is a deleted fork and deletes the workspace's
//     own changes, in the delete's transaction;
//   - the fork component reads the changes from a fork for its early
//     expiry, and the component records a fork's activity in the
//     transaction that opens or abandons a change from it
//     (fork.RecordActivityIn), since a second transaction would wait for
//     that one forever;
//   - the event log asks a change's source workspace, so that a token bound
//     to a fork reads the change events of its parent that are the fork's;
//   - the git server's fork view advertises the version refs of the
//     changes from a fork (forkView).
//
// No session linker, stack linker, landing evaluator or auto_land guard is
// registered: without a session linker an explicit session_id is refused
// and a defaulted one dropped; without a stack linker no change depends on
// another; without a landing evaluator every open change is ready to land;
// and auto_land is stored as given.
//
// The component exists from the composition on (Changes); tests change its
// dependencies with AdjustChange.
func composeChanges(c *Composition) {
	comp := &change.Component{}
	c.changes = comp
	c.AddSchemaStep(change.SchemaStep)
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.refWriter.RegisterRefUpdatedStage(comp.RefUpdated)
	c.scm.RegisterChangeLinker(changeLinker{comp, c.runtime})
	c.AdjustSCM(func(d *scm.Deps) { d.Changes = changeLookup{comp} })
	c.workspaces.RegisterBranchMoveObserver(syncVersions{comp, c.runtime})
	c.workspaces.Teardown().RegisterRemover(func(ctx context.Context, tx *sql.Tx, slug string) error {
		if err := comp.ForkDeleted(ctx, tx, slug, change.Caller{}); err != nil {
			return err
		}
		return comp.WorkspaceDeleted(ctx, tx, slug)
	})
	c.forks.RegisterChangeReader(forkChanges{comp})
	comp.RegisterForkActivity(forkActivity{c.forks})
	c.events.SetChangeSources(event.ChangeSourcesFunc(func(ctx context.Context, workspace string, number int64) (string, error) {
		ch, err := comp.ReadChange(ctx, nil, workspace, number)
		if errors.Is(err, change.ErrChangeNotFound) {
			return "", nil
		}
		return ch.Source.WorkspaceSlug, err
	}))
	c.AddStartHook(changeHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := change.Deps{
			Store: rt.Store,
			Repos: func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			Workspaces: changeWorkspaces{c.workspaces.Settings()},
			Access:     c.access,
			Events:     c.events,
			Now:        rt.Now,
			Logger:     rt.Logger,
		}
		for _, adjust := range c.changeAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// changeWorkspaces reads the workspace records for the change component,
// with the effective settings the registry derives.
type changeWorkspaces struct{ settings *workspace.SettingsRegistry }

func (r changeWorkspaces) Workspace(ctx context.Context, q change.Querier, slug string) (change.Workspace, bool, error) {
	w, found, err := workspace.BySlug(ctx, q, slug)
	if err != nil || !found {
		return change.Workspace{}, found, err
	}
	out := change.Workspace{
		Slug: w.Slug, Fork: w.Kind == access.KindFork, CarryPatch: w.Mode == workspace.ModeCarryPatch,
		Purpose: w.Purpose, SessionID: w.SessionID, Unusable: workspace.RequireReady(w),
	}
	for dst, src := range map[*string]*string{&out.Parent: w.ParentSlug, &out.IntegrationBranch: w.IntegrationBranch, &out.DefaultBranch: w.DefaultBranch} {
		if src != nil {
			*dst = *src
		}
	}
	if on, ok := r.settings.EffectiveSettings(w.Settings, w.DefaultBranch)["changes.require_intent"].(bool); ok {
		out.RequireIntent = on
	}
	return out, true, nil
}

// changeLinker is the source control API's change linker: the change
// component's lookup of a branch's change, its check of an open, which
// writes nothing, and its open, in a store transaction of its own, since
// the writes check before the branch moves and open after it moved, outside
// any transaction. A change a commit opens has version 1 of kind commit.
type changeLinker struct {
	changes *change.Component
	rt      *Runtime
}

func (l changeLinker) FindOpen(ctx context.Context, slug, branch string) (*scm.LinkedChange, error) {
	b, found, err := l.changes.OpenChangeOfBranch(ctx, nil, slug, branch)
	if err != nil || !found {
		return nil, err
	}
	return &scm.LinkedChange{WorkspaceSlug: b.WorkspaceSlug, Number: int(b.Number), Version: int(b.Version), State: b.State}, nil
}

func (l changeLinker) CheckOpening(ctx context.Context, o scm.ChangeOpening) error {
	return l.changes.CheckOpen(ctx, nil, openingInput(o))
}

func (l changeLinker) Open(ctx context.Context, o scm.ChangeOpening) (*scm.LinkedChange, error) {
	var ch change.Change
	err := l.rt.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		ch, err = l.changes.Open(ctx, tx, openingInput(o))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &scm.LinkedChange{WorkspaceSlug: ch.WorkspaceSlug, Number: int(ch.Number), Version: int(ch.Version), State: ch.State}, nil
}

// openingInput is the open of the change a write asks for: what it named,
// "" being the defaults, at the head it moves the source branch to, with
// its session as the trigger's, by its caller.
func openingInput(o scm.ChangeOpening) change.OpenInput {
	in := change.OpenInput{Workspace: o.Workspace, SourceWorkspace: o.SourceWorkspace, SourceBranch: o.SourceBranch,
		Head: o.Head, TriggerSession: o.SessionID, Draft: o.Draft, Caller: change.CallerOf(o.Caller), Kind: change.KindCommit}
	for dst, src := range map[**string]string{&in.TargetBranch: o.TargetBranch, &in.Title: o.Title, &in.Intent: o.Intent} {
		if src != "" {
			*dst = &src
		}
	}
	return in
}

// changeLookup is the source control reads' lookup of the change of a
// branch or of a commit, which a fork's slug asks of its parent's changes
// whose source is the fork.
type changeLookup struct{ changes *change.Component }

func (l changeLookup) OpenChangeOfBranch(ctx context.Context, slug, branch string) (*scm.Change, error) {
	b, found, err := l.changes.OpenChangeOfBranch(ctx, nil, slug, branch)
	if err != nil || !found {
		return nil, err
	}
	return &scm.Change{WorkspaceSlug: b.WorkspaceSlug, Number: int(b.Number)}, nil
}

func (l changeLookup) ChangeOfCommit(ctx context.Context, slug, sha string) (*scm.CommitChange, error) {
	cc, found, err := l.changes.ChangeOfCommit(ctx, nil, slug, sha)
	if err != nil || !found {
		return nil, err
	}
	out := &scm.CommitChange{WorkspaceSlug: cc.WorkspaceSlug, Number: int(cc.Number)}
	if cc.Version != nil {
		out.Version = new(int(*cc.Version))
	}
	return out, nil
}

// syncVersions is the branch-move observer of sync: a branch a sync moved
// records the next version of its draft or open change, of kind sync, by
// the hub, in a store transaction of its own.
type syncVersions struct {
	changes *change.Component
	rt      *Runtime
}

func (o syncVersions) BranchMoved(ctx context.Context, m workspace.BranchMove) error {
	return o.rt.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := o.changes.RecordVersion(ctx, tx, change.VersionInput{Workspace: m.Workspace, Branch: m.Branch, Head: m.After,
			Kind: change.KindSync})
		return err
	})
}

// forkChanges is the fork component's change reader: every change, in any
// state, whose source is the fork.
type forkChanges struct{ changes *change.Component }

func (r forkChanges) ChangesFrom(ctx context.Context, forkSlug string) ([]fork.SourceChange, error) {
	from, err := r.changes.ChangeNumbersOfSource(ctx, nil, forkSlug)
	if err != nil {
		return nil, err
	}
	out := make([]fork.SourceChange, 0, len(from))
	for _, s := range from {
		out = append(out, fork.SourceChange{Branch: s.Branch, State: s.State})
	}
	return out, nil
}

// forkActivity records, in the transaction of the change it is told of, an
// activity of the fork the change is from.
type forkActivity struct{ forks *fork.Component }

func (a forkActivity) RecordActivity(ctx context.Context, tx *sql.Tx, slug string) error {
	return a.forks.RecordActivityIn(ctx, tx, slug)
}

// Changes returns the hub's change component, which serves once its start
// hook has run.
func (c *Composition) Changes() *change.Component { return c.changes }

// AdjustChange adds fn to the functions that change the change component's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added.
func (c *Composition) AdjustChange(fn func(*change.Deps)) {
	c.changeAdjusters = append(c.changeAdjusters, fn)
}
