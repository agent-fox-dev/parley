package change

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// restackAttempts is how many times one restack reads the source branch,
// replays it and tries to swap it, when the branch moves in between each
// time: a branch that keeps moving is someone else's work in progress.
const restackAttempts = 5

// restackOutcome is how a restack ended.
type restackOutcome int

const (
	// restacked: the source branch moved to the replayed commits, and the
	// change has a restack version.
	restacked restackOutcome = iota
	// restackUpToDate: the change already sat on the commit, and nothing
	// moved.
	restackUpToDate
	// restackConflict: a commit of the change did not apply, and nothing
	// moved.
	restackConflict
	// restackClosed: the change is no longer draft or open, or is gone.
	restackClosed
	// restackLost: the source branch moved before each swap, and nothing
	// moved.
	restackLost
)

// restackResult is the end of a restack: its outcome, the version it
// recorded when restacked, the head the source branch was read at in the
// last attempt, and the conflict report of a conflict.
type restackResult struct {
	outcome restackOutcome
	version *Version
	head    string
	report  *gitrepo.ConflictReport
}

// restack restacks the change of the job onto the commit it must sit on
// (sitOn): the commits after the change's base up to the head of its source
// branch are replayed on that commit, in the object store alone, and the
// branch is moved to the result with a compare-and-swap on the head that
// was read, in a store transaction that records the move as the change's
// next version, of kind restack, by caller (the zero Caller for the hub)
// in session, nil for the session of the version it replaces. When the
// branch, or the commit to sit on, moved in between, the restack starts
// again, at most restackAttempts times. Each end removes the job's pending
// record, except a lost race and an error, which change nothing.
//
// It runs no transaction of its caller's: it reads and writes in
// transactions of its own, and replays outside any, since a replay may
// take long and a push waits for the store.
func (c *Component) restack(ctx context.Context, j restackJob, by Caller, session *string) (restackResult, error) {
	var res restackResult
	for attempt := 1; attempt <= restackAttempts; attempt++ {
		var again bool
		var err error
		if res, again, err = c.restackOnce(ctx, j, by, session, attempt); err != nil || !again {
			return res, err
		}
	}
	res.outcome = restackLost
	return res, nil
}

// restackOnce is one attempt of restack; again is set when the source
// branch or the commit to sit on moved before the swap.
func (c *Component) restackOnce(ctx context.Context, j restackJob, by Caller, session *string, attempt int) (res restackResult, again bool, err error) {
	var ch Change
	var onto string
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		if ch, err = readChange(ctx, tx, j.workspace, j.number); err != nil || !active(ch) {
			return err
		}
		onto, err = c.sitOn(ctx, tx, ch, j.onto)
		return err
	})
	switch {
	case errors.Is(err, ErrChangeNotFound):
		return restackResult{outcome: restackClosed}, false, nil
	case err != nil:
		return res, false, err
	case !active(ch):
		return c.restackEnded(ctx, j, ch, restackResult{outcome: restackClosed})
	case onto == "":
		return res, false, fmt.Errorf("restack %s: its target branch %s does not exist", ch.Ref, ch.TargetBranch)
	}

	repo, err := c.deps.Repos(ctx, ch.Source.WorkspaceSlug)
	if err != nil {
		return res, false, err
	}
	branch := ch.Source.Branch
	head, found, err := repo.ResolveBranch(ctx, branch)
	switch {
	case err != nil:
		return res, false, err
	case !found:
		return res, false, fmt.Errorf("restack %s: its source branch %s:%s does not exist", ch.Ref, ch.Source.WorkspaceSlug, branch)
	}
	res.head = head
	built, err := sitsOn(ctx, repo, baseColumn(ch.BaseSHA), head, onto)
	if err != nil {
		return res, false, err
	}
	if built {
		res.outcome = restackUpToDate
		return c.restackEnded(ctx, j, ch, res)
	}
	rebase, beforeSwap := c.restacks.seams()
	newHead, report, err := rebase(ctx, repo, baseColumn(ch.BaseSHA), head, onto, c.now())
	switch {
	case err != nil:
		return res, false, err
	case report != nil:
		res.outcome, res.report = restackConflict, report
		return c.restackEnded(ctx, j, ch, res)
	}
	if beforeSwap != nil {
		beforeSwap(ctx, j.workspace, j.number, attempt)
	}

	swapped := false
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		now, err := readChange(ctx, tx, j.workspace, j.number)
		if err != nil || !active(now) {
			res.outcome = restackClosed
			return c.ignoreGone(ctx, tx, j, err)
		}
		if ontoNow, err := c.sitOn(ctx, tx, now, j.onto); err != nil || ontoNow != onto {
			again = err == nil
			return err
		}
		switch err := repo.CompareAndSwapRef(ctx, "refs/heads/"+branch, newHead, head); {
		case errors.Is(err, gitrepo.ErrRefMoved):
			again = true
			return nil
		case err != nil:
			return err
		}
		swapped = true
		in := session
		if in == nil {
			in = now.SessionID
		}
		res.outcome = restacked
		res.version, err = c.recordRestack(ctx, tx, j, now, by, in, newHead, onto)
		return err
	})
	if err != nil && swapped {
		// The version was not recorded, so the branch goes back to the head
		// it had; a push since then keeps its own move.
		if rerr := repo.CompareAndSwapRef(context.WithoutCancel(ctx), "refs/heads/"+branch, head, newHead); rerr != nil {
			c.deps.Logger.Error("the source branch of a failed restack was not set back", "change", ch.Ref, "branch", branch, "error", rerr)
		}
	}
	if err != nil || again {
		return restackResult{head: head}, again, err
	}
	return res, false, nil
}

// recordRestack records in tx the move of the source branch of the change
// ch to head, the commits of ch replayed onto onto, as its next version, of
// kind restack, by caller in session, which was judged when the version it
// replaces was recorded or by the caller of a manual restack. A restack
// version is recorded as any other, so the changes above ch get their own
// pending restack from it. The change no longer needs a restack, unless it
// sits too deep in its stack; parley.change.restack follows the version's
// event, and the changes whose target is the moved branch are checked
// again, since no ref-updated stage saw the move.
func (c *Component) recordRestack(ctx context.Context, tx *sql.Tx, j restackJob, ch Change, by Caller, session *string, head, onto string) (*Version, error) {
	v, err := c.RecordVersion(ctx, tx, VersionInput{Workspace: ch.Source.WorkspaceSlug, Branch: ch.Source.Branch, Head: head,
		Kind: KindRestack, Caller: by, SessionID: session, SessionChecked: true})
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("restack %s: no version was recorded for %s", ch.Ref, head)
	}
	if ch, err = readChange(ctx, tx, j.workspace, j.number); err != nil {
		return nil, err
	}
	if ch.NeedsRestack && reasonOf(ch) != RestackTooDeep {
		if err := c.SetNeedsRestack(ctx, tx, j.workspace, j.number, false, nil); err != nil {
			return nil, err
		}
	}
	if err := clearRestackOnto(ctx, tx, j.workspace, j.number); err != nil {
		return nil, err
	}
	if err := c.emit(ctx, tx, by, ch, EventRestack, map[string]any{
		"version": v.Number, "head_sha": v.HeadSHA, "onto_sha": onto, "trigger": j.trigger,
	}); err != nil {
		return nil, err
	}
	if err := c.ScheduleTargetMergeable(ctx, tx, ch.Source.WorkspaceSlug, ch.Source.Branch); err != nil {
		return nil, err
	}
	_, err = dropRestack(ctx, tx, j)
	return v, err
}

// restackEnded stores, in a transaction of its own, the end of a restack
// of the change ch that moved nothing, and removes the job's pending
// record:
//
//   - up to date: a needs_restack set for a conflict or with automatic
//     restacks off is cleared, since the change now sits on what it must,
//     and so is the landed commit it had to sit on;
//   - a conflict: the mergeability check reports it;
//   - closed: nothing else.
//
// The mergeability check of a change with a pending record stores nothing
// (mergePair), so it is scheduled again once the record is gone.
func (c *Component) restackEnded(ctx context.Context, j restackJob, ch Change, res restackResult) (restackResult, bool, error) {
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		dropped, err := dropRestack(ctx, tx, j)
		if err != nil || res.outcome == restackClosed {
			return err
		}
		now, err := readChange(ctx, tx, j.workspace, j.number)
		if err != nil || !active(now) {
			return c.ignoreGone(ctx, tx, j, err)
		}
		if res.outcome == restackConflict {
			return c.restackConflicted(ctx, tx, now, res.report)
		}
		cleared := false
		if reason := reasonOf(now); now.NeedsRestack && (reason == RestackConflict || reason == RestackAutoOff) {
			if err := c.SetNeedsRestack(ctx, tx, j.workspace, j.number, false, nil); err != nil {
				return err
			}
			cleared = true
		}
		if err := clearRestackOnto(ctx, tx, j.workspace, j.number); err != nil {
			return err
		}
		if !dropped && !cleared {
			return nil
		}
		return c.ScheduleMergeable(ctx, tx, j.workspace, j.number)
	})
	return res, false, err
}

// restackConflicted stores in tx that the restack of the change ch found
// the conflict the report describes: its mergeability check, which finds
// it again, is scheduled.
func (c *Component) restackConflicted(ctx context.Context, tx *sql.Tx, ch Change, _ *gitrepo.ConflictReport) error {
	return c.ScheduleMergeable(ctx, tx, ch.WorkspaceSlug, ch.Number)
}

// ignoreGone is nil for err, the error of reading the change of the job in
// tx, when the change is gone or not draft or open, and removes its
// pending record then; any other error is returned.
func (c *Component) ignoreGone(ctx context.Context, tx *sql.Tx, j restackJob, err error) error {
	if err != nil && !errors.Is(err, ErrChangeNotFound) {
		return err
	}
	_, err = dropRestack(ctx, tx, j)
	return err
}

// sitOn is the commit the change ch must sit on, read through q: the head
// of the change it depends on while that one is draft or open; else onto,
// the commit its pending record names, or the commit the change below
// landed as, which the change keeps; else the head of its target branch,
// "" when that branch does not exist.
func (c *Component) sitOn(ctx context.Context, q Querier, ch Change, onto string) (string, error) {
	if ch.DependsOn != nil {
		lower, err := readChange(ctx, q, ch.WorkspaceSlug, *ch.DependsOn)
		switch {
		case err == nil && active(lower):
			return lower.HeadSHA, nil
		case err != nil && !errors.Is(err, ErrChangeNotFound):
			return "", err
		}
	}
	if onto != "" {
		return onto, nil
	}
	var landed sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT restack_onto FROM changes WHERE workspace_slug = ? AND number = ?`,
		ch.WorkspaceSlug, ch.Number).Scan(&landed); err != nil {
		return "", fmt.Errorf("read what %s sits on: %w", ch.Ref, err)
	}
	if landed.String != "" {
		return landed.String, nil
	}
	repo, err := c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return "", err
	}
	head, _, err := repo.ResolveBranch(ctx, ch.TargetBranch)
	return head, err
}

// sitsOn reports whether the change whose commits are base..head in repo
// already sits on onto: head has onto, and onto has base, so that every
// commit of head that onto lacks is the change's own. A base onto does not
// have, such as the head of an abandoned change below, means that head
// still carries commits that are not the change's own, which a restack
// leaves out. A change with no base counts every commit of its head as
// its own.
func sitsOn(ctx context.Context, repo *gitrepo.Repo, base, head, onto string) (bool, error) {
	built, err := repo.IsAncestor(ctx, onto, head)
	if err != nil || !built || base == "" {
		return built, err
	}
	return repo.IsAncestor(ctx, base, onto)
}

// clearRestackOnto forgets in tx the landed commit the change number of
// the workspace had to sit on, which it now does.
func clearRestackOnto(ctx context.Context, tx *sql.Tx, workspace string, number int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE changes SET restack_onto = NULL WHERE workspace_slug = ? AND number = ? AND restack_onto IS NOT NULL`,
		workspace, number); err != nil {
		return fmt.Errorf("store what %s sits on: %w", Ref(workspace, number), err)
	}
	return nil
}

// reasonOf is the restack_reason of the change, "" for none.
func reasonOf(ch Change) string {
	if ch.RestackReason == nil {
		return ""
	}
	return *ch.RestackReason
}

// active reports whether the change is draft or open.
func active(ch Change) bool { return ch.State == StateDraft || ch.State == StateOpen }
