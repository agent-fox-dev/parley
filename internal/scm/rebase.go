package scm

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/server"
)

// opRebase is the op a rebase request reports.
const opRebase = "rebase"

// The statuses of a rebase answer: the branch moved, or it already held
// onto and nothing changed.
const (
	rebaseRebased  = "rebased"
	rebaseUpToDate = "up_to_date"
)

// rebaseRequest is the body of POST …/scm/rebase.
type rebaseRequest struct {
	Branch          *string `json:"branch"`
	Onto            *string `json:"onto"`
	ExpectedHeadSHA *string `json:"expected_head_sha"`
}

// rebaseAnswer is the answer to a rebase request: the branch at its new
// head, the heads before and after, the commit it was rebased onto, and
// the change whose source the branch is, with the version the rebase made
// of it, null when there is none.
type rebaseAnswer struct {
	Status     string        `json:"status"`
	Branch     refObject     `json:"branch"`
	OldHeadSHA string        `json:"old_head_sha"`
	NewHeadSHA string        `json:"new_head_sha"`
	OntoSHA    string        `json:"onto_sha"`
	Change     *LinkedChange `json:"change"`
}

// rebase answers POST …/scm/rebase: the commits of the branch that onto
// does not hold replayed, oldest first, on top of the commit onto resolves
// to, keeping each one's author and message with the hub as committer, and
// the branch moved to the result through the shared ref update step, with
// the version kind rebase.
//
// The checks run in the order in which each has what it needs: the body
// (400 invalid_request: branch or onto missing, a branch outside
// refs/heads/, an expected_head_sha that is not a full id), before anything
// is read; the branch (404 ref_not_found) and expected_head_sha (409
// ref_moved); onto (404 ref_not_found). A branch that already holds onto
// answers up_to_date and moves nothing. A merge commit among the commits to
// replay is 400 merge_commits_unsupported and the first commit that does
// not apply 409 rebase_conflict with the conflict report as details, both
// before anything moves. The head read at the start guards the
// compare-and-swap, so a branch another writer moves meanwhile answers 409
// ref_moved and keeps that writer's head.
func (c *Component) rebase(w http.ResponseWriter, r *http.Request, wr *write) error {
	var body rebaseRequest
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	branch, err := body.check()
	if err != nil {
		return err
	}
	// The session is checked here, before the branch is resolved.

	ctx := r.Context()
	t, err := c.target(ctx, wr)
	if err != nil {
		return err
	}
	ref, err := t.current(r, branch)
	if err != nil {
		return err
	}
	head := commitOf(ref)
	if body.ExpectedHeadSHA != nil && strings.ToLower(*body.ExpectedHeadSHA) != head {
		return refMoved(branch, &head)
	}
	onto, err := t.resolver.Resolve(ctx, *body.Onto)
	if err != nil {
		return err
	}
	answer := rebaseAnswer{Status: rebaseRebased, OldHeadSHA: head, OntoSHA: onto.SHA}

	upToDate, err := t.git.IsAncestor(ctx, onto.SHA, head)
	if err != nil {
		return err
	}
	if upToDate {
		answer.Status, answer.NewHeadSHA = rebaseUpToDate, head
		if answer.Change, err = c.changeLinker().FindOpen(ctx, wr.ws.Slug, strings.TrimPrefix(branch, "refs/heads/")); err != nil {
			return err
		}
		return c.writeRebase(w, r, t, branch, answer)
	}

	newHead, conflict, err := t.git.Rebase(ctx, onto.SHA, head, c.deps.Now())
	switch {
	case errors.Is(err, gitrepo.ErrMergeCommit):
		return server.NewError("merge_commits_unsupported", http.StatusBadRequest,
			fmt.Sprintf("%s has a merge commit that %s does not hold; a rebase replays no merge commit", branch, *body.Onto))
	case err != nil:
		return err
	case conflict != nil:
		return server.NewError("rebase_conflict", http.StatusConflict,
			fmt.Sprintf("commit %s of %s does not apply on top of %s", conflict.CommitSHA, branch, onto.SHA)).
			WithDetails(conflict)
	}
	if hook := c.rebaseHook(); hook != nil {
		hook()
	}
	up, err := c.updateRef(ctx, wr, t.git, refUpdate{ref: branch, before: ref.ID, after: newHead,
		versionKind: pushrule.VersionKindRebase})
	if err != nil {
		return err
	}
	answer.NewHeadSHA = newHead
	if v := up.version; v != nil {
		answer.Change = &LinkedChange{WorkspaceSlug: v.WorkspaceSlug, Number: v.Number, Version: v.Version}
	}
	return c.writeRebase(w, r, t, branch, answer)
}

// check is the full name of the branch the request names, or 400
// invalid_request for a body of the wrong shape.
func (b *rebaseRequest) check() (string, error) {
	if b.Branch == nil || *b.Branch == "" {
		return "", server.InvalidRequest("branch is required")
	}
	branch, err := branchName(*b.Branch)
	if err != nil {
		return "", err
	}
	if b.Onto == nil || *b.Onto == "" {
		return "", server.InvalidRequest("onto is required")
	}
	if b.ExpectedHeadSHA != nil && !isFullID(*b.ExpectedHeadSHA) {
		return "", server.InvalidRequest("expected_head_sha must be a full 40-character commit id")
	}
	return branch, nil
}

// rebaseHook is the function a test runs between the replay and the move
// of the branch, nil for none.
func (c *Component) rebaseHook() func() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.replayed
}

// writeRebase answers 200 with answer, its branch the ref object of branch
// at the new head.
func (c *Component) writeRebase(w http.ResponseWriter, r *http.Request, t *target, branch string, answer rebaseAnswer) error {
	ref, _, err := c.refObject(r, t.read, Ref{Name: branch, SHA: answer.NewHeadSHA, Commit: answer.NewHeadSHA})
	if err != nil {
		return err
	}
	answer.Branch = ref
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}
