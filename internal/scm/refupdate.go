package scm

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Rules decide whether the ref updates of a write are allowed: the push
// rule engine, *pushrule.Engine, which a git push asks too.
type Rules interface {
	Evaluate(ctx context.Context, req pushrule.Request) pushrule.Verdict
}

// RefWriter writes the refs of an allowed write and then its events: the
// ref writer, *pushrule.Writer, through which a git push moves its refs
// too.
type RefWriter interface {
	Apply(ctx context.Context, req pushrule.ApplyRequest) (pushrule.Result, error)
	Finish(ctx context.Context, res pushrule.Result)
}

// msgRulesInternal is the message of a write a push rule could not be
// evaluated for; what failed is the hub's to know, not the caller's.
const msgRulesInternal = "the push rules could not be evaluated; try again"

// errNotGit is the failure of a write to a workspace whose repository the
// opener did not hand over as a git repository, which the ref writer needs.
var errNotGit = errors.New("scm: the repository of a write is not a git repository")

// target is the repository a write changes: the git repository the ref
// writer writes to, and the read of the same workspace, whose resolver
// resolves the write's revisions and whose ref objects its answer holds.
type target struct {
	*read
	git *gitrepo.Repo
}

// target opens the repository of the write wr.
func (c *Component) target(ctx context.Context, wr *write) (*target, error) {
	g, err := c.gitRepo(ctx, wr.ws.Slug)
	if err != nil {
		return nil, err
	}
	view := c.deps.RefView(ctx, wr.ws, g)
	var defaultBranch string
	if wr.ws.DefaultBranch != nil {
		defaultBranch = *wr.ws.DefaultBranch
	}
	rd := &read{ws: wr.ws, repo: g, view: view, resolver: NewResolver(g, view, defaultBranch)}
	return &target{read: rd, git: g}, nil
}

// gitRepo opens the git repository of the workspace slug.
func (c *Component) gitRepo(ctx context.Context, slug string) (*gitrepo.Repo, error) {
	repo, err := c.deps.Open(ctx, slug)
	if err != nil {
		return nil, err
	}
	g, ok := repo.(*gitrepo.Repo)
	if !ok {
		return nil, errNotGit
	}
	return g, nil
}

// lookup is the ref of the full name name as the write's workspace shows
// it: the ref of its repository, else, in a fork, the tracking branch of
// that name, which shows the parent's branch and is in no ref of the
// fork's repository. A write to a tracking branch so meets the push rules
// with the value a push of it would name, and is refused by them as the
// push is. found is false when the workspace shows no such ref.
func (t *target) lookup(ctx context.Context, name string) (ref gitrepo.Ref, found bool, err error) {
	ref, found, err = t.git.LookupRef(ctx, name)
	if err != nil || found || t.ws.Kind != access.KindFork {
		return ref, found, err
	}
	refs, err := t.view.Refs(ctx)
	if err != nil {
		return gitrepo.Ref{}, false, err
	}
	for _, r := range refs {
		if r.Name != name || !r.Tracking {
			continue
		}
		commit := r.Commit
		if commit == "" {
			// A branch names a commit.
			commit = r.SHA
		}
		return gitrepo.Ref{Name: r.Name, ID: r.SHA, Commit: commit}, true, nil
	}
	return gitrepo.Ref{}, false, nil
}

// refUpdate is the one ref update a write asks for: ref, a full ref name,
// from before (pushrule.ZeroID for a create) to after (pushrule.ZeroID for
// a delete), the kind of version it makes of a change whose source it moves
// (one of the pushrule.VersionKind constants), and the session the write
// belongs to, nil for none. beforeWrite, when set, runs once the push rules
// have allowed the update, with their decision, and before the ref writer
// is asked to write it; its error is the write's answer and nothing moves.
// events are what the write owes beside the push event of its ref, which
// the ref writer journals and writes with it.
type refUpdate struct {
	ref, before, after string
	versionKind        string
	sessionID          *string
	beforeWrite        func(ctx context.Context, d pushrule.Decision) error
	events             []event.Input
}

// refUpdated is a ref update the ref writer made: the engine's decision,
// which says whether it created, deleted or forced its ref and whether it
// was a direct push, and the version a ref-updated stage recorded for it,
// nil for none. A direct push opens no change: a route asks the change
// linker nothing after one, and names only the version a stage recorded
// when the branch was already a change's source.
type refUpdated struct {
	decision pushrule.Decision
	version  *pushrule.Version
}

// change is the change the update made a version of, as a write's answer
// names it: nil when no stage recorded a version.
func (u refUpdated) change() *LinkedChange {
	if u.version == nil {
		return nil
	}
	v := u.version
	return &LinkedChange{WorkspaceSlug: v.WorkspaceSlug, Number: v.Number, Version: v.Version}
}

// updateRef is the one way a write moves a ref, so that it meets the push
// rules exactly where a git push of the same update would and has the same
// consequences: the engine decides the update with the facts of the write's
// caller and workspace, and the ref writer writes it by compare-and-swap
// against u.before with via api, running its ref-updated stages, setting
// head_sha, recording a fork's activity, then, in its event step, emitting
// parley.git.push and telling the post-push listeners. This package writes
// no ref and no push event itself.
//
// A refused update is 403 with the refusing rule's message and error type;
// one no rule could be evaluated for, 500 internal. A ref not at u.before
// when the refs are written is 409 ref_moved with the commit it is at then
// as details.current_sha; a ref to be created whose name another ref is in
// the way of is 409 ref_exists naming that ref, since no head to retry
// from would make the create succeed. A failure of a ref-updated stage is answered as
// it is when it is an API error, else 500 internal.
func (c *Component) updateRef(ctx context.Context, wr *write, repo *gitrepo.Repo, u refUpdate) (refUpdated, error) {
	pw, pc := ruleFacts(wr)
	req := pushrule.Request{Workspace: pw, Caller: pc, Repo: repo,
		Updates: []pushrule.Update{{Ref: u.ref, Before: u.before, After: u.after}}}
	if pw.Parent != "" {
		parent, err := c.gitRepo(ctx, pw.Parent)
		if err != nil {
			return refUpdated{}, err
		}
		req.ParentRepo = parent.Dir()
	}
	verdict := c.deps.Rules.Evaluate(ctx, req)
	if verdict.Refused() {
		return refUpdated{}, refusal(wr.ws, verdict.Decisions)
	}
	if u.beforeWrite != nil && len(verdict.Decisions) > 0 {
		if err := u.beforeWrite(ctx, verdict.Decisions[0]); err != nil {
			return refUpdated{}, err
		}
	}
	res, err := c.deps.RefWriter.Apply(ctx, pushrule.ApplyRequest{
		Verdict: verdict, Repo: repo, Workspace: pw, Caller: pc, SessionID: u.sessionID,
		Via: pushrule.ViaAPI, VersionKind: u.versionKind, Events: u.events,
	})
	switch res.Outcome {
	case pushrule.OutcomeAccepted:
	case pushrule.OutcomeRefMoved:
		return refUpdated{}, refMovedNow(context.WithoutCancel(ctx), repo, u.ref, u.before == pushrule.ZeroID)
	case pushrule.OutcomeRefused:
		return refUpdated{}, refusal(wr.ws, decisionsOf(res))
	default:
		var apiErr *server.Error
		if errors.As(err, &apiErr) {
			return refUpdated{}, apiErr
		}
		if err == nil {
			err = errors.New("the ref writer wrote nothing")
		}
		return refUpdated{}, fmt.Errorf("write %s: %w", u.ref, err)
	}
	// The ref has moved: its events are written whatever becomes of the
	// request's context.
	c.deps.RefWriter.Finish(context.WithoutCancel(ctx), res)
	up := refUpdated{}
	if len(res.Updates) > 0 {
		up = refUpdated{decision: res.Updates[0].Decision, version: res.Updates[0].Version}
	}
	return up, nil
}

// ruleFacts are the write's workspace and caller as the push rules take
// them, as the git server hands them over for a push.
func ruleFacts(wr *write) (pushrule.Workspace, pushrule.Caller) {
	ws, p := wr.ws, wr.principal
	pw := pushrule.Workspace{Slug: ws.Slug, Kind: ws.Kind, Mode: ws.Mode}
	if ws.Kind == access.KindFork && ws.ParentSlug != nil {
		pw.Parent = *ws.ParentSlug
	}
	if ws.DefaultBranch != nil {
		pw.DefaultBranch = *ws.DefaultBranch
	}
	pc := pushrule.Caller{Level: wr.decision.Level, CredentialType: p.Type, CredentialID: p.CredentialID,
		Bound: p.Workspace != nil, Paths: p.Paths}
	if p.Type == identity.TypeAdmin {
		pc.CredentialType = pushrule.CredentialAdminToken
	}
	if p.User != nil {
		pc.UserID = p.User.ID
	}
	return pw, pc
}

// decisionsOf are the decisions of the updates of res.
func decisionsOf(res pushrule.Result) []pushrule.Decision {
	ds := make([]pushrule.Decision, len(res.Updates))
	for i, u := range res.Updates {
		ds[i] = u.Decision
	}
	return ds
}

// ruleRefusal is the refusal of a write by a push rule, which the request
// recorder reports as ResultRefused; the API error it wraps is the answer.
type ruleRefusal struct{ err *server.Error }

func (r *ruleRefusal) Error() string { return r.err.Error() }

func (r *ruleRefusal) Unwrap() error { return r.err }

// refusal is the answer to a write to the workspace ws whose update the
// decisions ds refuse: the first refusing rule's message, unchanged, and
// error type with 403, or 500 internal with a message of its own when no
// rule could be evaluated, since what failed is the hub's to know. A path
// refusal names the path the token may not change as the member path of
// the error; a tracking-branch refusal names the branch the fork was taken
// from as details.base_ref, the branch to write to instead.
func refusal(ws workspace.Workspace, ds []pushrule.Decision) error {
	for _, d := range ds {
		switch {
		case d.ErrorType == pushrule.ErrorTypeInternal:
			return server.Internal(msgRulesInternal)
		case d.ErrorType == "":
			continue
		}
		e := server.NewError(d.ErrorType, http.StatusForbidden, d.Message)
		switch {
		case d.ErrorType == pushrule.ErrorTypePathNotAllowed && d.Path != "":
			e = e.With("path", d.Path)
		case d.ErrorType == pushrule.ErrorTypeTrackingBranch && ws.ForkPoint != nil:
			e = e.WithDetails(map[string]any{"base_ref": ws.ForkPoint.Ref})
		}
		return &ruleRefusal{e}
	}
	return server.Internal(msgRulesInternal)
}

// refMoved is the answer to a write that found the ref name at another
// commit than the caller expected: 409 ref_moved, with the commit it is at,
// current, as details.current_sha, null when the ref does not exist.
func refMoved(name string, current *string) error {
	return server.NewError(pushrule.ErrorTypeRefMoved, http.StatusConflict,
		fmt.Sprintf("%s is not at the expected commit; read it again and retry", name)).
		WithDetails(map[string]any{"current_sha": current})
}

// refMovedNow is refMoved with the commit the ref name of repo is at now,
// read after the ref writer found it moved. For a create, create true, a
// ref name that does not exist then was refused because another ref's name
// is in the way of it, and the answer is refInTheWay when one is.
func refMovedNow(ctx context.Context, repo *gitrepo.Repo, name string, create bool) error {
	ref, found, err := repo.LookupRef(ctx, name)
	if err != nil {
		return err
	}
	if found {
		current := commitOf(ref)
		return refMoved(name, &current)
	}
	if create {
		blocker, blocked, err := repo.RefInTheWay(ctx, name)
		if err != nil {
			return err
		}
		if blocked {
			return refInTheWay(name, blocker)
		}
	}
	return refMoved(name, nil)
}

// refInTheWay is the answer to a create of the ref name that git cannot
// hold beside the ref blocker, one name being a directory of the other:
// 409 ref_exists naming blocker, which must go before name can be created.
func refInTheWay(name, blocker string) error {
	return server.NewError("ref_exists", http.StatusConflict,
		fmt.Sprintf("ref %s is in the way of %s; delete it to create %s", blocker, name, name))
}
