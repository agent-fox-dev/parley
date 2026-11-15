package change

import (
	"context"
	"database/sql"
)

// VersionInput is a move of a branch that RecordVersion records.
type VersionInput struct {
	// Workspace is the slug of the source workspace, the one whose branch
	// moved; the change lives in its target, the parent of a fork.
	Workspace string
	// Branch is the branch that moved, without refs/heads/, and Head the
	// commit it moved to.
	Branch, Head string
	// Kind is what moved the branch: KindPush, KindCommit, KindRef,
	// KindRebase, KindRestack, KindRevert or KindSync.
	Kind string
	// Caller is who moved the branch, with the credential they used; the
	// zero value is the hub itself (the system actor, no credential).
	Caller Caller
	// SessionID is the session of the move, nil for none.
	SessionID *string
}

// RecordVersion records the move in of a branch as the next version of the
// draft or open change whose source the branch is, in tx, the caller's
// transaction, which it neither begins nor commits. It records nothing and
// returns no version when the branch is the source of no draft or open
// change or already has the head of the change's current version.
//
// A session must pass the session linker (without one, every session is
// refused) or the answer is 400 unknown_session, before anything is
// written. The version's base is the merge base of its head and the
// target branch, none for a head that shares no history with it, or the
// previous version's base while the target branch does not exist; a
// version with no base is measured from the empty tree. It is
// patch-equivalent to the latest earlier version
// whose diff against its own base is the same. RecordVersion stores the
// version with its creating credential, writes its ref
// refs/parley/changes/<number>/<version> in the target's repository,
// bringing a fork's commit there first, links its session, makes it the
// change's current version with mergeable unknown, then runs the stack
// linker, the landing derivation and the version observers in that order
// and writes parley.change.version. The landing of a draft stays
// blocked/draft; a missing target branch, a head with no commit the target
// lacks, a head with no history in common with the target and a merge
// commit among the version's commits block it as target_not_found,
// nothing_to_land, unrelated_history and merge_commits without asking the
// landing evaluator. The change keeps its state.
//
// As with Open, the version ref is written before the collaborators run
// and before RecordVersion returns, and removed again when a later step
// fails; a ref left by a transaction the caller rolls back after
// RecordVersion succeeded names no stored version and is overwritten when
// that number is next recorded. An error a collaborator returns is
// returned unchanged.
func (c *Component) RecordVersion(ctx context.Context, tx *sql.Tx, in VersionInput) (_ *Version, err error) {
	workspace, number, found, err := activeOfSource(ctx, tx, in.Workspace, in.Branch)
	if err != nil || !found {
		return nil, err
	}
	ch, err := readChange(ctx, tx, workspace, number)
	if err != nil || ch.HeadSHA == in.Head {
		return nil, err
	}
	co := c.collaborators()
	var session *string
	if in.SessionID != nil {
		if session, err = c.session(ctx, co, OpenInput{Workspace: workspace, SessionID: in.SessionID, Caller: in.Caller}, Workspace{}); err != nil {
			return nil, err
		}
	}
	o, structural, err := c.measureMove(ctx, tx, ch, in.Head)
	if err != nil {
		return nil, err
	}
	equivalent, err := latestEquivalent(ctx, tx, workspace, number, o.patchID)
	if err != nil {
		return nil, err
	}
	actor, err := c.actorOf(ctx, tx, in.Caller)
	if err != nil {
		return nil, err
	}
	v := Version{
		Number: ch.Version + 1, HeadSHA: in.Head, BaseSHA: baseOf(o.base), Kind: in.Kind, PatchEquivalentTo: equivalent,
		CreatedBy: actor, SessionID: session, Stats: o.stats, CreatedAt: c.now(), Credential: in.Caller.Credential,
		PatchID: o.patchID,
	}
	if err := insertVersion(ctx, tx, workspace, number, v); err != nil {
		return nil, err
	}
	if err := o.writeVersionRef(ctx, number, v.Number); err != nil {
		return nil, err
	}
	defer func() { c.dropVersionRef(ctx, o.targetRepo, workspace, number, v.Number, err) }()
	if session != nil {
		if err := co.sessions.Record(ctx, tx, *session, workspace, number, v.Number); err != nil {
			return nil, err
		}
	}
	if err := storeVersion(ctx, tx, workspace, number, v, o.paths, o.truncated); err != nil {
		return nil, err
	}
	if err := c.afterVersion(ctx, tx, co, workspace, number, v, nil, structural); err != nil {
		return nil, err
	}
	if ch, err = readChange(ctx, tx, workspace, number); err != nil {
		return nil, err
	}
	if err := c.emitVersion(ctx, tx, in.Caller, ch, v); err != nil {
		return nil, err
	}
	return &v, nil
}

// RecordRacedVersion records in tx the move in of the source branch of the
// draft or open change number of the workspace by a write whose
// ref-updated stage found no change, the change being opened after it
// moved the branch, and returns the number of the version the move is:
//
//   - the current version, when the change already has in.Head: the open
//     that followed the branch (OpenInput.AfterMove) recorded it, of kind
//     ref by the hub with no session, and it becomes the move's, of
//     in.Kind by in.Caller in in.SessionID, its session linked; the event
//     written for it then is left as it was;
//   - the current version, with nothing recorded, when the branch is no
//     longer at in.Head: a later write moved it, and its version stands;
//   - otherwise the next version, as RecordVersion records it.
//
// A session the session linker refuses is 400 unknown_session, before
// anything is written. in.Workspace and in.Branch name the change's source.
func (c *Component) RecordRacedVersion(ctx context.Context, tx *sql.Tx, workspace string, number int64, in VersionInput) (int64, error) {
	ch, err := readChange(ctx, tx, workspace, number)
	if err != nil {
		return 0, err
	}
	if ch.HeadSHA != in.Head {
		repo, err := c.deps.Repos(ctx, ch.Source.WorkspaceSlug)
		if err != nil {
			return 0, err
		}
		head, found, err := repo.ResolveBranch(ctx, ch.Source.Branch)
		if err != nil || !found || head != in.Head {
			return ch.Version, err
		}
		v, err := c.RecordVersion(ctx, tx, in)
		if err != nil || v == nil {
			return ch.Version, err
		}
		return v.Number, nil
	}
	current, err := readVersionsAfter(ctx, tx, workspace, number, ch.Version-1, 1)
	if err != nil || len(current) == 0 {
		return ch.Version, err
	}
	v := current[0]
	if v.Kind != KindRef || v.CreatedBy.ActorType != ActorSystem || v.SessionID != nil {
		return v.Number, nil
	}
	co := c.collaborators()
	if in.SessionID != nil {
		if v.SessionID, err = c.session(ctx, co, OpenInput{Workspace: workspace, SessionID: in.SessionID, Caller: in.Caller}, Workspace{}); err != nil {
			return 0, err
		}
	}
	if v.CreatedBy, err = c.actorOf(ctx, tx, in.Caller); err != nil {
		return 0, err
	}
	v.Kind, v.Credential = in.Kind, in.Caller.Credential
	if err := attributeVersion(ctx, tx, workspace, number, v); err != nil {
		return 0, err
	}
	if v.SessionID != nil {
		if err := co.sessions.Record(ctx, tx, *v.SessionID, workspace, number, v.Number); err != nil {
			return 0, err
		}
		if err := update(ctx, tx, workspace, number, c.now(), `session_id = ?`, v.SessionID); err != nil {
			return 0, err
		}
	}
	return v.Number, nil
}

// measureMove measures head, the new head of the change ch's source branch,
// as Open measures version 1: its base, stats, touched paths and patch id,
// read in the source's repository, which for a fork reads its parent's
// objects. structural is the landing a cause in the version blocks, nil
// for none: the target branch missing, no commit the target lacks, no
// history in common with the target, or a merge commit among the commits.
func (c *Component) measureMove(ctx context.Context, q Querier, ch Change, head string) (o *opening, structural *Landing, err error) {
	o = &opening{head: head}
	if o.targetRepo, err = c.deps.Repos(ctx, ch.WorkspaceSlug); err != nil {
		return nil, nil, err
	}
	o.sourceRepo = o.targetRepo
	if ch.Source.WorkspaceSlug != ch.WorkspaceSlug {
		if o.sourceRepo, err = c.deps.Repos(ctx, ch.Source.WorkspaceSlug); err != nil {
			return nil, nil, err
		}
	}
	targetHead, found, err := o.targetRepo.ResolveBranch(ctx, ch.TargetBranch)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		if o.base, err = versionBase(ctx, q, ch.WorkspaceSlug, ch.Number, ch.Version); err != nil {
			return nil, nil, err
		}
		ahead, err := o.sourceRepo.CommitsBetween(ctx, o.base, head, 0)
		if err != nil {
			return nil, nil, err
		}
		l := blockedLanding("target_not_found")
		return o, &l, o.measureFromBase(ctx, len(ahead))
	}
	ahead, err := o.sourceRepo.CommitsBetween(ctx, targetHead, head, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := o.measure(ctx, targetHead, len(ahead)); err != nil {
		return nil, nil, err
	}
	if len(ahead) == 0 {
		l := blockedLanding("nothing_to_land")
		return o, &l, nil
	}
	if o.base == "" {
		l := blockedLanding("unrelated_history")
		return o, &l, nil
	}
	commits, err := o.sourceRepo.ReadCommits(ctx, ahead)
	if err != nil {
		return nil, nil, err
	}
	for _, cm := range commits {
		if len(cm.Parents) > 1 {
			l := blockedLanding("merge_commits")
			return o, &l, nil
		}
	}
	return o, nil, nil
}
