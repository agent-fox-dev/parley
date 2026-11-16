package land

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// EventRevert is the event of a landed change that a change was opened to
// revert, written on the reverted change.
const EventRevert = "parley.change.revert"

// Rules decide whether the ref updates of a write are allowed: the push
// rule engine, *pushrule.Engine, which a git push and the source control
// API ask too.
type Rules interface {
	Evaluate(ctx context.Context, req pushrule.Request) pushrule.Verdict
}

// revertRequest is what a revert request's body carries, nil or false
// where a member is absent.
type revertRequest struct {
	title, intent, sessionID *string
	autoLand                 *bool
	draft                    bool
}

// parseRevert reads the body of the revert request r: none, or a JSON
// object whose title is a string an open accepts as a title (1 to 200
// characters on one line), intent a string of at most 16,000 characters,
// draft and auto_land booleans and session_id a string. Members it does
// not know are ignored; anything else is 400 invalid_request.
func parseRevert(r *http.Request) (revertRequest, error) {
	var req revertRequest
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return req, server.InvalidRequest("request body could not be read")
	}
	body := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if err := server.DecodeJSON(r, &body); err != nil {
			return req, err
		}
	}
	texts := map[string]**string{"title": &req.title, "intent": &req.intent, "session_id": &req.sessionID}
	for _, name := range []string{"title", "intent", "session_id", "draft", "auto_land"} {
		raw, ok := body[name]
		if !ok {
			continue
		}
		// A null is no value of any member's type.
		null := bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
		if dst, ok := texts[name]; ok {
			var s string
			if null || json.Unmarshal(raw, &s) != nil {
				return req, server.InvalidRequest(name + ": must be a string")
			}
			*dst = &s
			continue
		}
		var b bool
		if null || json.Unmarshal(raw, &b) != nil {
			return req, server.InvalidRequest(name + ": must be a boolean")
		}
		if name == "draft" {
			req.draft = b
		} else {
			req.autoLand = &b
		}
	}
	if req.title != nil {
		if err := change.CheckTitle(*req.title); err != nil {
			return req, err
		}
	}
	if req.intent != nil {
		if err := change.CheckIntent(*req.intent); err != nil {
			return req, err
		}
	}
	return req, nil
}

// revertRoute reverts the landed change :number of the workspace :slug: it
// undoes the change's landed commits, newest first, one commit each, on the
// head of the change's target branch, creates the branch revert/<number>
// (or the first free revert/<number>-<n>) at the last of them, opens a
// change from it to the target branch that reverts the change, writes
// parley.change.revert on the reverted change and answers 201 with the new
// change.
//
// In this order, after the shared checks: the change (404
// change_not_found), the body (400 invalid_request), who may revert (403
// forbidden, see mayRevert), the session (400 unknown_session), the workspace's status (409, such as
// workspace_archived), a change that is not landed (409
// change_not_landed), one a draft, open or landed change already reverts
// (409 already_reverted, with details.reverted_by), a target branch that is
// gone (400 target_not_found), a file of the landed version outside a
// path-scoped caller's paths (403 path_not_allowed, with the path), nothing
// to undo (400 nothing_to_land: no
// landed commit, or every undo empty) and an undo that does not apply (409
// revert_conflict with the conflict report as details.conflict). A refusal
// creates nothing: no commit is referenced, and no branch, change or event
// is written.
func (c *Component) revertRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	slug := server.Param(r, "slug")
	number, ok := positive(server.Param(r, "number"))
	if !ok {
		return change.ErrChangeNotFound
	}
	orig, err := c.deps.Changes.ReadChange(ctx, nil, slug, number)
	if err != nil {
		return err
	}
	req, err := parseRevert(r)
	if err != nil {
		return err
	}
	p, _ := identity.PrincipalOf(ctx)
	caller := change.CallerOf(p)
	ws, err := c.workspace(ctx, slug)
	if err != nil {
		return err
	}
	if err := c.mayRevert(ctx, caller, orig, ws); err != nil {
		return err
	}
	if req.sessionID != nil {
		id, err := c.deps.Changes.CheckSession(ctx, caller, *req.sessionID, slug)
		if err != nil {
			return err
		}
		req.sessionID = &id
	}
	if ws.Ready != nil {
		return ws.Ready
	}
	switch {
	case orig.State != change.StateLanded || orig.Landed == nil:
		return server.NewError("change_not_landed", http.StatusConflict,
			fmt.Sprintf("%s is %s; only a landed change is reverted", orig.Ref, orig.State))
	case orig.RevertedBy != nil:
		return server.NewError("already_reverted", http.StatusConflict,
			fmt.Sprintf("%s is reverted by %s already", orig.Ref, change.Ref(slug, *orig.RevertedBy))).
			WithDetails(map[string]any{"reverted_by": *orig.RevertedBy})
	}
	repo, err := c.deps.Repos(ctx, slug)
	if err != nil {
		return err
	}
	target, found, err := repo.ResolveBranch(ctx, orig.TargetBranch)
	if err != nil {
		return err
	}
	if !found {
		return server.NewError("target_not_found", http.StatusBadRequest,
			fmt.Sprintf("%s landed on %s, which %s no longer has", orig.Ref, orig.TargetBranch, slug))
	}
	// A path-scoped token reverts only what it could have landed: the
	// files of the landed version, as recorded, before any undo is made.
	base := ""
	if orig.BaseSHA != nil {
		base = *orig.BaseSHA
	}
	refused, err := CheckPaths(ctx, repo, caller.Paths, base, orig.HeadSHA)
	if err != nil {
		return err
	}
	if refused != "" {
		return pathNotAllowed(refused)
	}
	head, err := c.undo(ctx, repo, orig, target)
	if err != nil {
		return err
	}
	ch, err := c.openRevert(ctx, r, repo, ws, caller, orig, req, head)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, ch)
	return nil
}

// mayRevert admits caller to revert the change orig of the workspace ws by
// the rule of who may land it: the admin token, a writer of the workspace,
// which is its owner, and, while land.by_author is on, the change's author
// with any credential of theirs. Every other caller, who reads the
// workspace since the route's shared checks passed, is answered 403
// forbidden. A token bound to a fork never gets here: the revert is not a
// route the binding guard lets such a token reach on the fork's parent,
// so that it cannot undo work there.
func (c *Component) mayRevert(ctx context.Context, caller change.Caller, orig change.Change, ws Workspace) error {
	may, err := mayLand(ctx, c.deps.Access, caller, []change.Change{orig}, ws)
	if err != nil || may {
		return err
	}
	return server.Forbidden(fmt.Sprintf("%s may be reverted only by a writer of %s, the admin token or, while land.by_author is on, its author",
		orig.Ref, ws.Slug))
}

// undo makes, in repo, the commits that undo the landed commits of the
// change orig, newest first, on top of target, the head of its target
// branch, by the landing identity, and returns the last of them. A change
// that landed no commit, or whose every undo would be empty, is 400
// nothing_to_land; an undo that does not apply is 409 revert_conflict with
// the report as details.conflict.
func (c *Component) undo(ctx context.Context, repo *gitrepo.Repo, orig change.Change, target string) (string, error) {
	commits := make([]string, 0, len(orig.Landed.Commits))
	for _, lc := range slices.Backward(orig.Landed.Commits) {
		commits = append(commits, lc.LandedSHA)
	}
	nothing := server.NewError("nothing_to_land", http.StatusBadRequest,
		fmt.Sprintf("%s: %s already lacks what it landed", orig.Ref, orig.TargetBranch))
	if len(commits) == 0 {
		return "", nothing
	}
	as := gitrepo.Signature{Name: c.deps.Committer.Name, Email: c.deps.Committer.Email, Time: c.now()}
	head, report, err := repo.Revert(ctx, target, commits, as)
	switch {
	case err != nil:
		return "", err
	case report != nil:
		return "", server.NewError("revert_conflict", http.StatusConflict, fmt.Sprintf(
			"%s: commit %s cannot be undone on top of %s", orig.Ref, report.CommitSHA, report.TargetSHA)).
			WithDetails(map[string]any{"conflict": report})
	case head == target:
		return "", nothing
	}
	return head, nil
}

// openRevert creates the branch of the revert of the change orig at head,
// in the repository repo of the workspace ws, and opens the reverting
// change from it, as caller with what req names, writing
// parley.change.revert on orig in the open's transaction; it returns the
// new change.
//
// The open is checked before the branch is created, so that a refused
// open creates no branch, and is made after it, reading the branch again
// in its transaction, as a write that opens a change once it moved the
// source does. Should it fail all the same, the branch just created is
// deleted again through the ref writer, compare-and-swap from head, so
// that its deletion is journalled and written as a push event like its
// creation, and the open's error is the answer.
func (c *Component) openRevert(ctx context.Context, r *http.Request, repo *gitrepo.Repo, ws Workspace, caller change.Caller,
	orig change.Change, req revertRequest, head string) (change.Change, error) {
	in := change.OpenInput{
		Workspace: ws.Slug, Head: head, TargetBranch: &orig.TargetBranch, Title: req.title, Intent: req.intent,
		AutoLand: req.autoLand, SessionID: req.sessionID, Draft: req.draft, Caller: caller, Kind: change.KindRevert,
		Reverts: &orig.Number,
		// Who may revert, and so open the revert from its branch in ws, is
		// decided: an author land.by_author lets revert, who only reads ws,
		// opens it all the same.
		SourceGranted: true,
	}
	if in.Title == nil {
		title := []rune(`Revert "` + orig.Title + `"`)
		in.Title = new(string(title[:min(len(title), change.MaxTitle)]))
	}
	if in.Intent == nil {
		in.Intent = new(fmt.Sprintf("Reverts change %s: %s.", orig.Ref, orig.Title))
	}
	pw, pc := ruleFacts(r, ws, caller)
	ref, err := c.createRevertBranch(ctx, repo, pw, pc, orig.Number, head, req.sessionID, func(branch string) error {
		check := in
		check.SourceBranch = branch
		return c.deps.Changes.CheckOpen(ctx, nil, check)
	})
	if err != nil {
		return change.Change{}, err
	}
	in.SourceBranch, in.AfterMove = ref[len("refs/heads/"):], true
	var ch change.Change
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		if ch, err = c.deps.Changes.Open(ctx, tx, in); err != nil {
			return err
		}
		a := actorOf(caller)
		_, err = c.deps.Events.Emit(ctx, tx, event.Input{
			Type: EventRevert, Workspace: &ws.Slug, ActorType: a.ActorType, ActorID: a.ActorID, ResourceType: "change",
			ResourceID: strconv.FormatInt(orig.Number, 10), SessionID: req.sessionID,
			Metadata: map[string]any{"reverted_by": ch.Number, "number": orig.Number, "ref": orig.Ref},
		})
		return err
	})
	if err != nil {
		c.deleteBranch(context.WithoutCancel(ctx), repo, pw, pc, ref, head, req.sessionID)
		return change.Change{}, err
	}
	return ch, nil
}

// createRevertBranch creates, in repo, the branch revert/<number>, or the
// first of revert/<number>-2, -3 and so on that does not exist, at head,
// and returns its full name. Each create is an API ref creation as the
// source control API makes one: the push rules decide it with the caller's
// facts, check is asked once they allowed it, and the ref writer writes it
// by compare-and-swap against no ref, with via api, so that it is
// journalled and written as a parley.git.push event. A name taken in
// between is answered by the next. A refusal by a push rule is 403 with the
// rule's message and error type; check's error is returned as it is; a name
// another ref's name is in the way of is 409 ref_exists.
func (c *Component) createRevertBranch(ctx context.Context, repo *gitrepo.Repo, pw pushrule.Workspace, pc pushrule.Caller,
	number int64, head string, session *string, check func(branch string) error) (string, error) {
	if c.deps.Rules == nil {
		return "", errors.New("land: no push rules to create a revert branch with")
	}
	for n := 1; ; n++ {
		branch := "revert/" + strconv.FormatInt(number, 10)
		if n > 1 {
			branch += "-" + strconv.Itoa(n)
		}
		ref := "refs/heads/" + branch
		if _, found, err := repo.LookupRef(ctx, ref); err != nil || found {
			if err != nil {
				return "", err
			}
			continue
		}
		verdict := c.deps.Rules.Evaluate(ctx, pushrule.Request{Workspace: pw, Caller: pc, Repo: repo,
			Updates: []pushrule.Update{{Ref: ref, Before: pushrule.ZeroID, After: head}}})
		if verdict.Refused() {
			return "", ruleRefusal(verdict.Decisions)
		}
		if err := check(branch); err != nil {
			return "", err
		}
		res, err := c.deps.RefWriter.Apply(ctx, pushrule.ApplyRequest{Verdict: verdict, Repo: repo, Workspace: pw, Caller: pc,
			SessionID: session, Via: pushrule.ViaAPI, VersionKind: pushrule.VersionKindNone})
		switch res.Outcome {
		case pushrule.OutcomeAccepted:
			// The branch exists: its event is written whatever becomes of
			// the request's context.
			c.deps.RefWriter.Finish(context.WithoutCancel(ctx), res)
			return ref, nil
		case pushrule.OutcomeRefMoved:
			if _, found, err := repo.LookupRef(ctx, ref); err != nil || found {
				if err != nil {
					return "", err
				}
				continue
			}
			blocker, _, err := repo.RefInTheWay(ctx, ref)
			if err != nil {
				return "", err
			}
			return "", server.NewError("ref_exists", http.StatusConflict,
				fmt.Sprintf("ref %s is in the way of %s; delete it to revert", blocker, ref))
		case pushrule.OutcomeRefused:
			ds := make([]pushrule.Decision, len(res.Updates))
			for i, u := range res.Updates {
				ds[i] = u.Decision
			}
			return "", ruleRefusal(ds)
		}
		if err == nil {
			err = errors.New("the ref writer wrote nothing")
		}
		return "", fmt.Errorf("create %s: %w", ref, err)
	}
}

// deleteBranch deletes the branch ref of repo, at head, through the ref
// writer, with a verdict that allows it without asking the push rules,
// since it undoes a create they allowed. What fails is logged: the open's
// error is the request's answer.
func (c *Component) deleteBranch(ctx context.Context, repo *gitrepo.Repo, pw pushrule.Workspace, pc pushrule.Caller,
	ref, head string, session *string) {
	verdict, err := pushrule.AllowedVerdict(ctx, repo, []pushrule.Update{{Ref: ref, Before: head, After: pushrule.ZeroID}})
	if err == nil {
		var res pushrule.Result
		res, err = c.deps.RefWriter.Apply(ctx, pushrule.ApplyRequest{Verdict: verdict, Repo: repo, Workspace: pw, Caller: pc,
			SessionID: session, Via: pushrule.ViaAPI, VersionKind: pushrule.VersionKindNone})
		if res.Outcome == pushrule.OutcomeAccepted {
			c.deps.RefWriter.Finish(ctx, res)
			return
		}
		if err == nil {
			err = fmt.Errorf("the ref writer answered %s", res.Outcome)
		}
	}
	c.deps.Logger.Error("the branch of a revert whose change was not opened is left", "workspace", pw.Slug, "ref", ref, "error", err)
}

// ruleFacts are the workspace ws and the caller of the request r as the
// push rules take them, as the source control API hands them over for a
// write: the caller's level on the workspace is the access check's.
func ruleFacts(r *http.Request, ws Workspace, caller change.Caller) (pushrule.Workspace, pushrule.Caller) {
	pw := pushrule.Workspace{Slug: ws.Slug, Kind: ws.Kind, Parent: ws.Parent, DefaultBranch: ws.DefaultBranch, Mode: ws.Mode}
	pc := pushrule.Caller{CredentialType: caller.Credential.Type, CredentialID: caller.Credential.ID, UserID: caller.UserID,
		Bound: caller.Bound != "", Paths: caller.Paths}
	if d, ok := access.DecisionOf(r); ok {
		pc.Level = d.Level
	}
	return pw, pc
}

// ruleRefusal is the answer to a create the decisions ds refuse: the first
// refusing rule's message and error type with 403, naming the path a
// path-scoped token may not change, or 500 internal when no rule could be
// evaluated, since what failed is the hub's to know.
func ruleRefusal(ds []pushrule.Decision) error {
	for _, d := range ds {
		switch d.ErrorType {
		case "":
			continue
		case pushrule.ErrorTypeInternal:
			return server.Internal("the push rules could not be evaluated; try again")
		}
		e := server.NewError(d.ErrorType, http.StatusForbidden, d.Message)
		if d.ErrorType == pushrule.ErrorTypePathNotAllowed && d.Path != "" {
			e = e.With("path", d.Path)
		}
		return e
	}
	return server.Internal("the push rules could not be evaluated; try again")
}
