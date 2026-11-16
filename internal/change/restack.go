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
	// restackSkipped: the worker leaves the change as it is, since it sits
	// too deep in its stack to be restacked by the hub, or no longer sits
	// on what the restack was for; nothing moved.
	restackSkipped
)

// restackResult is the end of a restack: its outcome, the head the source
// branch was read at in the last attempt, and the conflict report of a
// conflict.
type restackResult struct {
	outcome restackOutcome
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
// branch, the change's version or the commit to sit on moved in between,
// the restack starts again, at most restackAttempts times. Each end
// removes the job's pending record, except a lost race and an error, which
// change nothing.
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

// restackRead is what an attempt of a restack starts from: the change as
// recorded, the commit it must sit on, its source repository and the head
// its source branch was read at. ends is set, with outcome, when the
// restack ends there without a replay.
type restackRead struct {
	ch      Change
	onto    string
	repo    *gitrepo.Repo
	head    string
	ends    bool
	outcome restackOutcome
}

// restackOnce is one attempt of restack; again is set when the source
// branch, the change's version or the commit to sit on moved before the
// swap.
func (c *Component) restackOnce(ctx context.Context, j restackJob, by Caller, session *string, attempt int) (restackResult, bool, error) {
	rd, err := c.readRestack(ctx, j)
	switch {
	case err != nil:
		return restackResult{head: rd.head}, false, err
	case rd.ends:
		return c.restackEnded(ctx, j, restackResult{outcome: rd.outcome, head: rd.head})
	}
	rebase, beforeSwap := c.restacks.seams()
	newHead, report, err := rebase(ctx, rd.repo, baseColumn(rd.ch.BaseSHA), rd.head, rd.onto, c.now(), j.resolutions)
	switch {
	case err != nil:
		return restackResult{head: rd.head}, false, err
	case report != nil:
		return c.restackConflicted(ctx, j, rd.ch, restackResult{outcome: restackConflict, head: rd.head, report: report}, rd.onto)
	}
	// The hub's own restacks are not judged: their actor is the hub, and
	// the version guard judges a caller's version, which a replay that
	// leaves the head as it is does not make.
	if g := c.collaborators().versions; g != nil && !j.background && newHead != rd.head {
		if err := g.CheckVersion(ctx, by, rd.ch, rd.onto, newHead); err != nil {
			return restackResult{head: rd.head}, false, err
		}
	}
	if beforeSwap != nil {
		beforeSwap(ctx, j.workspace, j.number, attempt)
	}
	return c.swapRestack(ctx, j, rd, newHead, by, session)
}

// readRestack reads what an attempt of the restack of the job starts from:
// the change and the commit it must sit on, in a transaction of its own,
// which waits for the one that recorded the job; then, outside any, the
// head of its source branch. The restack ends there when the change is
// gone or no longer draft or open (closed), when the worker leaves it as
// it is (skipsRestack), or when it already sits on the commit (up to
// date).
func (c *Component) readRestack(ctx context.Context, j restackJob) (rd restackRead, err error) {
	var skip bool
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		if rd.ch, err = readChange(ctx, tx, j.workspace, j.number); err != nil || !active(rd.ch) {
			return err
		}
		if j.background {
			if skip, err = c.skipsRestack(ctx, tx, j, rd.ch); err != nil || skip {
				return err
			}
		}
		rd.onto, err = c.sitOn(ctx, tx, rd.ch, j.onto)
		return err
	})
	switch {
	case errors.Is(err, ErrChangeNotFound), err == nil && !active(rd.ch):
		rd.ends, rd.outcome = true, restackClosed
		return rd, nil
	case err != nil:
		return rd, err
	case skip:
		rd.ends, rd.outcome = true, restackSkipped
		return rd, nil
	case rd.onto == "":
		return rd, fmt.Errorf("restack %s: its target branch %s does not exist", rd.ch.Ref, rd.ch.TargetBranch)
	}
	if rd.repo, err = c.deps.Repos(ctx, rd.ch.Source.WorkspaceSlug); err != nil {
		return rd, err
	}
	branch := rd.ch.Source.Branch
	head, found, err := rd.repo.ResolveBranch(ctx, branch)
	switch {
	case err != nil:
		return rd, err
	case !found:
		return rd, fmt.Errorf("restack %s: its source branch %s:%s does not exist", rd.ch.Ref, rd.ch.Source.WorkspaceSlug, branch)
	}
	rd.head = head
	built, err := sitsOn(ctx, rd.repo, baseColumn(rd.ch.BaseSHA), head, rd.onto)
	rd.ends, rd.outcome = built, restackUpToDate
	return rd, err
}

// skipsRestack reports whether the worker leaves the change ch of the job
// as it is, read in tx. A change that sank too deep in its stack after its
// restack was recorded is not restacked by the hub; its depth is counted,
// not read from its restack_reason, which a conflict may have replaced. A
// restack for a new version below, or for a caller's link, of a change
// that no longer depends on a draft or open change and has no landed
// commit to sit on, ends too: the change below it is no longer there, and
// nothing asked for a restack onto its target branch.
func (c *Component) skipsRestack(ctx context.Context, tx *sql.Tx, j restackJob, ch Change) (bool, error) {
	if deep, err := c.tooDeep(ctx, tx, j.workspace, j.number); err != nil || deep {
		return deep, err
	}
	if (j.trigger != TriggerVersion && j.trigger != TriggerManual) || j.onto != "" {
		return false, nil
	}
	if ch.DependsOn != nil {
		lower, err := readChange(ctx, tx, ch.WorkspaceSlug, *ch.DependsOn)
		switch {
		case err == nil && active(lower):
			return false, nil
		case err != nil && !errors.Is(err, ErrChangeNotFound):
			return false, err
		}
	}
	landed, err := landedBelow(ctx, tx, ch)
	return landed == "", err
}

// swapRestack moves, in a store transaction, the source branch that the
// attempt rd read to newHead, the change's commits replayed onto rd.onto,
// and records the move (recordRestack). When the change got a version or
// another base, or the commit to sit on moved, since rd was read, or the
// branch moved, nothing is written and again is set: the replay was of
// commits the change no longer has. A replay that leaves the head as it
// is, as it does once the change was pushed onto that commit, moves
// nothing and ends the restack as up to date; the push records its own
// version. A failure after the branch moved sets it back.
func (c *Component) swapRestack(ctx context.Context, j restackJob, rd restackRead, newHead string, by Caller, session *string) (restackResult, bool, error) {
	res := restackResult{outcome: restacked, head: rd.head}
	ref := "refs/heads/" + rd.ch.Source.Branch
	again, swapped := false, false
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		now, err := readChange(ctx, tx, j.workspace, j.number)
		if err != nil || !active(now) {
			res.outcome = restackClosed
			return c.ignoreGone(ctx, tx, j, err)
		}
		if now.HeadSHA != rd.ch.HeadSHA || baseColumn(now.BaseSHA) != baseColumn(rd.ch.BaseSHA) {
			again = true
			return nil
		}
		if onto, err := c.sitOn(ctx, tx, now, j.onto); err != nil || onto != rd.onto {
			again = err == nil
			return err
		}
		if newHead == rd.head {
			res.outcome = restackUpToDate
			return c.storeRestackEnd(ctx, tx, j, restackUpToDate)
		}
		switch err := rd.repo.CompareAndSwapRef(ctx, ref, newHead, rd.head); {
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
		return c.recordRestack(ctx, tx, j, now, by, in, newHead, rd.onto)
	})
	if err != nil && swapped {
		// The version was not recorded, so the branch goes back to the head
		// it had; a push since then keeps its own move.
		if rerr := rd.repo.CompareAndSwapRef(context.WithoutCancel(ctx), ref, rd.head, newHead); rerr != nil {
			c.deps.Logger.Error("the source branch of a failed restack was not set back", "change", rd.ch.Ref, "branch", rd.ch.Source.Branch, "error", rerr)
		}
	}
	if err != nil || again {
		return restackResult{head: rd.head}, again, err
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
func (c *Component) recordRestack(ctx context.Context, tx *sql.Tx, j restackJob, ch Change, by Caller, session *string, head, onto string) error {
	v, err := c.RecordVersion(ctx, tx, VersionInput{Workspace: ch.Source.WorkspaceSlug, Branch: ch.Source.Branch, Head: head,
		Kind: KindRestack, Caller: by, SessionID: session, SessionChecked: true})
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("restack %s: no version was recorded for %s", ch.Ref, head)
	}
	if ch, err = readChange(ctx, tx, j.workspace, j.number); err != nil {
		return err
	}
	if reason := reasonOf(ch); ch.NeedsRestack && reason != RestackTooDeep {
		if err := c.clearNeedsRestack(ctx, tx, j.workspace, j.number, reason); err != nil {
			return err
		}
	}
	if err := clearRestackOnto(ctx, tx, j.workspace, j.number); err != nil {
		return err
	}
	if err := c.emit(ctx, tx, by, ch, EventRestack, map[string]any{
		"version": v.Number, "head_sha": v.HeadSHA, "onto_sha": onto, "trigger": j.trigger,
	}); err != nil {
		return err
	}
	if err := c.ScheduleTargetMergeable(ctx, tx, ch.Source.WorkspaceSlug, ch.Source.Branch); err != nil {
		return err
	}
	_, err = dropRestack(ctx, tx, j)
	return err
}

// restackEnded stores, in a transaction of its own, the end of a restack
// that moved nothing and found no conflict (storeRestackEnd), and returns
// res as the attempt's result.
func (c *Component) restackEnded(ctx context.Context, j restackJob, res restackResult) (restackResult, bool, error) {
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.storeRestackEnd(ctx, tx, j, res.outcome) })
	return res, false, err
}

// storeRestackEnd stores in tx the end of the restack of the job that
// moved nothing and found no conflict (restackConflicted stores that), and
// removes the job's pending record:
//
//   - up to date: a needs_restack set for a conflict or with automatic
//     restacks off is cleared, since the change now sits on what it must,
//     and so is the landed commit it had to sit on;
//   - skipped: its mergeability is checked again;
//   - closed: nothing else.
//
// The mergeability check of a change with a pending record stores nothing
// (mergePair), so it is scheduled again once the record is gone.
func (c *Component) storeRestackEnd(ctx context.Context, tx *sql.Tx, j restackJob, outcome restackOutcome) error {
	dropped, err := dropRestack(ctx, tx, j)
	switch {
	case err != nil || outcome == restackClosed:
		return err
	case outcome == restackSkipped:
		if !dropped {
			return nil
		}
		return c.ScheduleMergeable(ctx, tx, j.workspace, j.number)
	}
	now, err := readChange(ctx, tx, j.workspace, j.number)
	if err != nil || !active(now) {
		return c.ignoreGone(ctx, tx, j, err)
	}
	cleared := false
	if reason := reasonOf(now); now.NeedsRestack && (reason == RestackConflict || reason == RestackAutoOff) {
		if err := c.clearNeedsRestack(ctx, tx, j.workspace, j.number, reason); err != nil {
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
	landed, err := landedBelow(ctx, q, ch)
	if err != nil || landed != "" {
		return landed, err
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

// landedBelow is the commit the change below the change ch landed as,
// which ch must sit on until it does, read through q; "" for none.
func landedBelow(ctx context.Context, q Querier, ch Change) (string, error) {
	var landed sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT restack_onto FROM changes WHERE workspace_slug = ? AND number = ?`,
		ch.WorkspaceSlug, ch.Number).Scan(&landed); err != nil {
		return "", fmt.Errorf("read what %s sits on: %w", ch.Ref, err)
	}
	return landed.String, nil
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
