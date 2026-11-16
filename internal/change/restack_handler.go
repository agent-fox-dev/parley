package change

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// VersionGuard judges the version a caller's restack would record, such as
// against the paths a path-scoped token is limited to. Without one every
// version passes.
type VersionGuard interface {
	// CheckVersion is asked, once per attempt of a manual restack of the
	// change ch, after the replay and before the source branch moves,
	// whether caller (with their credential and paths) may record the
	// version from base, the commit the change is restacked onto, to head,
	// the replayed head. Its error refuses the restack unchanged, and
	// nothing moves.
	CheckVersion(ctx context.Context, caller Caller, ch Change, base, head string) error
}

// RegisterVersionGuard makes g the version guard; nil removes it.
func (c *Component) RegisterVersionGuard(g VersionGuard) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.versionGuard = g
}

// Statuses of the answer of the manual restack.
const (
	restackStatusRestacked = "restacked"
	restackStatusUpToDate  = "up_to_date"
)

// restackRoute restacks the change :number of the workspace :slug on its
// caller's demand, as the hub restacks it in the background, and answers
// with the outcome. In one store transaction and in the order of modify,
// changeOf resolves the change (404 change_not_found), AuthorizeModify
// applies the caller rule (403 forbidden), the body, none or a JSON
// object, is read: resolutions must be a list of valid resolutions
// (readResolutions, 400 invalid_request), and session_id must pass the
// session-linking rule (400 unknown_session); then the target
// workspace's status (409), the change's state (409 change_closed) and
// the commit it must sit on (400 target_not_found when it is the head of
// a target branch that does not exist). The restack runs after that
// transaction, with the trigger manual, by the caller in the body's
// session or the replaced version's, and ends the change's pending
// restack when it ends. A change too deep in its stack is restacked too,
// and keeps its flag.
//
// The replay takes, at each commit that does not apply, the resolution of
// every path that conflicts there, and stops at the first commit with a
// conflicted path that has none (gitrepo.Repo.RebaseFromResolving); a
// change that already sits on the commit ignores them.
//
// It answers 200 {status, change}: restacked with the restack version, or
// up_to_date when the change already sits on the commit; 409
// rebase_conflict with the conflict report as details, stored on the
// change as a background restack stores it; 409 ref_moved with the
// source branch's head as details.current_sha when the branch moved
// before every swap; 400 merge_commits_unsupported for a merge commit
// among the change's own commits; and a version guard's refusal as it is.
func (c *Component) restackRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p, _ := identity.PrincipalOf(ctx)
	caller := CallerOf(p)
	var j restackJob
	var session *string
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		ch, err := c.changeOf(r, tx)
		if err != nil {
			return err
		}
		if err := AuthorizeModify(r, ch); err != nil {
			return err
		}
		var body map[string]json.RawMessage
		if err := decodeOptional(r, &body); err != nil {
			return err
		}
		named, err := member[string](body, "session_id", "a string")
		if err != nil {
			return err
		}
		resolutions, err := readResolutions(body)
		if err != nil {
			return err
		}
		if named != nil {
			if session, err = c.session(ctx, c.collaborators(), OpenInput{Workspace: ch.WorkspaceSlug, SessionID: named, Caller: caller}, Workspace{}); err != nil {
				return err
			}
		}
		if err := c.workspaceUsable(ctx, tx, ch.WorkspaceSlug); err != nil {
			return err
		}
		if !active(ch) {
			return changeClosed(ch, "cannot be restacked")
		}
		// The job takes over the change's pending restack, so that it is
		// removed when this one ends.
		j = restackJob{workspace: ch.WorkspaceSlug, number: ch.Number, trigger: TriggerManual, resolutions: resolutions}
		if j.seq, j.onto, err = pendingRestack(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
			return err
		}
		onto, err := c.sitOn(ctx, tx, ch, j.onto)
		if err != nil {
			return err
		}
		if onto == "" {
			return server.NewError("target_not_found", http.StatusBadRequest,
				fmt.Sprintf("%s depends on no change and its target branch %s does not exist", ch.Ref, ch.TargetBranch))
		}
		return nil
	})
	if err != nil {
		return err
	}

	res, err := c.restack(ctx, j, caller, session)
	switch {
	case errors.Is(err, gitrepo.ErrMergeCommit):
		return server.NewError("merge_commits_unsupported", http.StatusBadRequest,
			fmt.Sprintf("%s has a merge commit among its own commits; a restack replays no merge commit", Ref(j.workspace, j.number)))
	case err != nil:
		return err
	}
	ch, err := readChange(ctx, c.deps.Store.Main(), j.workspace, j.number)
	if err != nil {
		return err
	}
	status := restackStatusRestacked
	switch res.outcome {
	case restackUpToDate:
		status = restackStatusUpToDate
	case restackConflict:
		return server.NewError("rebase_conflict", http.StatusConflict,
			fmt.Sprintf("commit %s of %s does not apply on top of %s", res.report.CommitSHA, ch.Ref, res.report.TargetSHA)).
			WithDetails(res.report)
	case restackLost:
		return c.restackRefMoved(ctx, ch)
	case restackClosed:
		return changeClosed(ch, "cannot be restacked")
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"status": status, "change": ch})
	return nil
}

// restackRefMoved is the answer to a manual restack of the change ch whose
// source branch moved before each swap: 409 ref_moved with the head the
// branch is at now, null when it is gone.
func (c *Component) restackRefMoved(ctx context.Context, ch Change) error {
	repo, err := c.deps.Repos(ctx, ch.Source.WorkspaceSlug)
	if err != nil {
		return err
	}
	head, found, err := repo.ResolveBranch(ctx, ch.Source.Branch)
	if err != nil {
		return err
	}
	var current *string
	if found {
		current = &head
	}
	return server.NewError(pushrule.ErrorTypeRefMoved, http.StatusConflict,
		fmt.Sprintf("source branch %s of %s moved during each of %d attempts; try again", ch.Source.Branch, ch.Ref, restackAttempts)).
		WithDetails(map[string]any{"current_sha": current})
}

// pendingRestack is the seq and the onto of the pending restack of the
// change number of the workspace, read through q; 0 and "" for none.
func pendingRestack(ctx context.Context, q Querier, workspace string, number int64) (seq int64, onto string, err error) {
	err = q.QueryRowContext(ctx, `SELECT seq, onto FROM change_restacks WHERE workspace_slug = ? AND number = ?`, workspace, number).Scan(&seq, &onto)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, "", nil
	case err != nil:
		return 0, "", fmt.Errorf("read the restack of %s: %w", Ref(workspace, number), err)
	}
	return seq, onto, nil
}
