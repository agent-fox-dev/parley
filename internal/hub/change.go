package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/server"
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
//     changes from a fork (forkView);
//   - the push processor's mapping stage, a change mapper over the
//     component (pushChanges), turns each branch of an accepted push into
//     the version or the change it makes and tells the git client, after
//     the refs moved and before the push is answered; it is bound to the
//     hub's logger in the start hook.
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
	mapper := gitserver.NewChangeMapper(pushChanges{comp, c.runtime}, nil)
	c.receiver.RegisterMappingStage(mapper)
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
		mapper.Bind(rt.Logger)
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
// any transaction. A change a commit opens has version 1 of kind commit;
// the open reads the branch again in its transaction, so that a write
// that moved it in between is its next version (change.OpenInput.AfterMove).
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
// its session as the trigger's, by its caller. The open comes after the
// branch moved, outside the write's transaction, so it follows a later
// move of the branch (AfterMove); a check before the move has none to
// follow.
func openingInput(o scm.ChangeOpening) change.OpenInput {
	in := change.OpenInput{Workspace: o.Workspace, SourceWorkspace: o.SourceWorkspace, SourceBranch: o.SourceBranch,
		Head: o.Head, TriggerSession: o.SessionID, Draft: o.Draft, Caller: change.CallerOf(o.Caller), Kind: change.KindCommit,
		AfterMove: true}
	for dst, src := range map[**string]string{&in.TargetBranch: o.TargetBranch, &in.Title: o.Title, &in.Intent: o.Intent} {
		if src != "" {
			*dst = &src
		}
	}
	return in
}

// pushChanges is the change service of the push processor's mapping stage:
// the change component's lookup of a branch's change, its open of the
// change a push asks for, and the version of a change another push opened
// first, each write in a store transaction of its own, since the mapping
// runs once the push's refs moved, outside the ref writer's transaction. A
// change a push opens has version 1 of kind push, and the open reads the
// branch again in its transaction, so that a write that moved it in between
// is its next version (change.OpenInput.AfterMove), or a branch deleted in
// between abandons it.
type pushChanges struct {
	changes *change.Component
	rt      *Runtime
}

func (p pushChanges) OpenChangeOfBranch(ctx context.Context, source, branch string) (*gitserver.BranchChange, error) {
	b, found, err := p.changes.OpenChangeOfBranch(ctx, nil, source, branch)
	if err != nil || !found {
		return nil, err
	}
	return &gitserver.BranchChange{Workspace: b.WorkspaceSlug, Number: int(b.Number)}, nil
}

// RecordPushVersion answers, in one transaction, the push whose branch
// the change the request names was opened for after the push moved it: a
// change that left draft and open since is refused with the state it is
// in, and an open one is told of the move (change.RecordRacedVersion).
func (p pushChanges) RecordPushVersion(ctx context.Context, req gitserver.VersionRequest) (*pushrule.Version, error) {
	var out *pushrule.Version
	err := p.rt.Store.Tx(ctx, func(tx *sql.Tx) error {
		ch, err := p.changes.ReadChange(ctx, tx, req.Workspace, int64(req.Number))
		if err != nil {
			return err
		}
		if ch.State != change.StateDraft && ch.State != change.StateOpen {
			return &gitserver.ChangeNotOpenError{Workspace: req.Workspace, Number: req.Number, State: ch.State}
		}
		v, err := p.changes.RecordRacedVersion(ctx, tx, req.Workspace, int64(req.Number), change.VersionInput{Workspace: req.Source,
			Branch: req.Branch, Head: req.Head, Kind: change.KindPush, Caller: pushCaller(req.Caller), SessionID: req.SessionID})
		if err != nil {
			return err
		}
		out = &pushrule.Version{WorkspaceSlug: req.Workspace, Number: req.Number, Version: int(v)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// OpenChange opens the change with what the push named, nil being the
// defaults, and its session as the trigger's. The component's change_exists
// names the change that has the source only in its details, so the
// refusal is answered with the change the lookup finds; a lookup that
// fails or finds none leaves the push unrecorded, an error of the hub's,
// not a refusal of the push.
func (p pushChanges) OpenChange(ctx context.Context, req gitserver.OpenRequest) (gitserver.OpenedChange, error) {
	in := change.OpenInput{Workspace: req.Workspace, SourceWorkspace: req.Source, SourceBranch: req.Branch, Head: req.Head,
		TargetBranch: req.Target, Title: req.Title, Intent: req.Intent, TriggerSession: req.SessionID, Draft: req.Draft,
		Caller: pushCaller(req.Caller), Kind: change.KindPush, AfterMove: true}
	var ch change.Change
	err := p.rt.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		ch, err = p.changes.Open(ctx, tx, in)
		return err
	})
	var refusal *server.Error
	if errors.As(err, &refusal) && refusal.Type() == "change_exists" {
		// The refusal is kept as text only: wrapped, the mapper would word
		// it as the push's refusal, when the push failed to be recorded.
		b, found, lerr := p.changes.OpenChangeOfBranch(ctx, nil, req.Source, req.Branch)
		switch {
		case lerr != nil:
			return gitserver.OpenedChange{}, fmt.Errorf("open refused (%v), and the lookup of the source's change failed: %w", err, lerr)
		case !found:
			return gitserver.OpenedChange{}, fmt.Errorf("open refused (%v), and the lookup found no change of the source", err)
		}
		return gitserver.OpenedChange{}, &gitserver.ChangeExistsError{Workspace: b.WorkspaceSlug, Number: int(b.Number)}
	}
	if err != nil {
		return gitserver.OpenedChange{}, err
	}
	return gitserver.OpenedChange{Workspace: ch.WorkspaceSlug, Number: int(ch.Number), Draft: ch.State == change.StateDraft,
		Abandoned: ch.State == change.StateAbandoned}, nil
}

// pushCaller is the change component's caller a push's caller is: a PAT
// as the credential check read it, with its paths and its binding, and
// any other credential by its user, type and id.
func pushCaller(c gitserver.PushCaller) change.Caller {
	if c.PAT != nil {
		return change.CallerOf(*c.PAT)
	}
	return change.Caller{UserID: c.UserID, Credential: change.Credential{Type: c.CredentialType, ID: c.CredentialID}}
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
