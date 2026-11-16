package change

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/store"
)

// MergeableStep gives the change record what its mergeable was computed
// for: the head and the commit it was rebased onto, and the value the last
// check computed (NULL before the first), which an unknown in between does
// not replace. It applies after SchemaStep.
var MergeableStep = store.Step{
	Name:   "change_mergeable",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, column := range []string{"mergeable_head TEXT", "mergeable_onto TEXT", "mergeable_last TEXT"} {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE changes ADD COLUMN `+column); err != nil {
				return err
			}
		}
		return nil
	},
}

// EventConflict is the event of a change whose mergeability check found a
// conflict where the last one found none.
const EventConflict = "parley.change.conflict"

// mergeableDeadline is how long a trial rebase may run, the read deadline
// of the hub's git reads; mergeablePool is how many checks run at once.
const (
	mergeableDeadline = 30 * time.Second
	mergeablePool     = 4
)

// ScheduleMergeable sets the mergeable of the change number of the
// workspace to unknown in tx, the caller's transaction, and schedules a
// check of it, for a trigger such as a retarget, a restore or a restack
// that recorded no version. A change that is neither draft nor open is
// left as it is; one that does not exist is the change-not-found error.
//
// The check reads the change in a transaction of its own, which waits for
// tx, so it sees what tx committed, and a check of a change whose tx was
// rolled back computes the change as it is.
func (c *Component) ScheduleMergeable(ctx context.Context, tx *sql.Tx, workspace string, number int64) error {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM changes WHERE workspace_slug = ? AND number = ?`, workspace, number).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errChangeNotFound(workspace, number)
	case err != nil:
		return err
	case state != StateDraft && state != StateOpen:
		return nil
	}
	if err := c.SetMergeable(ctx, tx, workspace, number, MergeableUnknown, nil); err != nil {
		return err
	}
	c.merge.pend(mergeKey{workspace, number})
	return nil
}

// ScheduleTargetMergeable is ScheduleMergeable for every draft and open
// change of the workspace whose target is branch, which moved, was created
// or was deleted; a branch that is the target of none schedules nothing.
func (c *Component) ScheduleTargetMergeable(ctx context.Context, tx *sql.Tx, workspace, branch string) error {
	numbers, err := c.ChangesInto(ctx, tx, workspace, branch)
	if err != nil {
		return err
	}
	for _, n := range numbers {
		if err := c.ScheduleMergeable(ctx, tx, workspace, n); err != nil {
			return err
		}
	}
	return nil
}

// TargetsUpdated is the ref-updated stage of the ref writer that schedules
// the checks of the changes whose target branch a write moved, created or
// deleted, whatever wrote it: a push, the source control API or a landing.
// It runs in tx, the write's transaction, and only reads and writes the
// store, as every stage must. It records no version.
func (c *Component) TargetsUpdated(ctx context.Context, tx *sql.Tx, in pushrule.StageInput) ([]*pushrule.Version, error) {
	for _, d := range in.Updates {
		branch, ok := strings.CutPrefix(d.Ref, "refs/heads/")
		if !ok || !d.Allowed {
			continue
		}
		if err := c.ScheduleTargetMergeable(ctx, tx, in.Workspace.Slug, branch); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// MergeableObserver is the version observer that schedules the check of a
// change with a new version, whatever recorded it, and of the draft and
// open changes that depend on it, whose onto-commit it moved.
func (c *Component) MergeableObserver() VersionObserver { return mergeableObserver{c} }

type mergeableObserver struct{ c *Component }

func (o mergeableObserver) VersionRecorded(ctx context.Context, tx *sql.Tx, ch Change, _ Version, _ Credential) error {
	if err := o.c.ScheduleMergeable(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
		return err
	}
	deps, err := dependants(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return err
	}
	for _, n := range deps {
		if err := o.c.ScheduleMergeable(ctx, tx, ch.WorkspaceSlug, n); err != nil {
			return err
		}
	}
	return nil
}

// RunMergeability runs the scheduled mergeability checks until ctx ends,
// a few at a time, and returns once the running ones have stopped: their
// git processes are ended with ctx. Checks scheduled before it runs are
// kept and run.
func (c *Component) RunMergeability(ctx context.Context) {
	var wg sync.WaitGroup
	for range mergeablePool {
		wg.Go(func() { c.runChecks(ctx) })
	}
	wg.Wait()
}

// runChecks runs one check after the other, each of a change that is due,
// and waits while none is.
func (c *Component) runChecks(ctx context.Context) {
	for ctx.Err() == nil {
		k, due, wait, changed, clock := c.merge.next(max(c.deps.MergeableDebounce, 0))
		if due {
			c.checkMergeable(ctx, k)
			c.merge.done(k)
			continue
		}
		var timer <-chan time.Time
		if wait >= 0 {
			timer = clock.After(wait)
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timer:
		}
	}
}

// checkMergeable computes the mergeable of the change k: it reads the
// change's head and onto-commit, trial-rebases the head onto it in the
// target's repository under the deadline, only the change's own commits for
// a change in a stack (ownBase), and stores the result when they and the
// change's base are still the change's, a conflict named with the landed
// changes it collided with (attribute), unless a restack stored its
// conflict since the check was scheduled (holdsRestackConflict), or a
// conflict of the change's head onto its target's head was
// (holdsTargetConflict); a pair that moved on is scheduled again. A
// missing target branch leaves mergeable unknown and re-evaluates the
// landing, which reports it. A trial rebase that fails or passes its
// deadline leaves mergeable unknown and is logged; the change's next
// trigger computes it again.
func (c *Component) checkMergeable(ctx context.Context, k mergeKey) {
	trial, deadline, gate := c.merge.seams()
	if gate != nil {
		gate(ctx, k.workspace, k.number)
	}
	var (
		ch         Change
		onto, base string
		live       bool
	)
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		if ch, onto, live, err = c.mergePair(ctx, tx, k.workspace, k.number); err != nil || !live {
			return err
		}
		if onto != "" {
			base, err = ownBase(ctx, tx, ch)
			return err
		}
		return c.reevaluateLanding(ctx, tx, k.workspace, k.number)
	})
	if err != nil || !live || onto == "" {
		c.checkFailed(ctx, k, err)
		return
	}
	repo, err := c.deps.Repos(ctx, k.workspace)
	if err != nil {
		c.checkFailed(ctx, k, err)
		return
	}
	tctx, cancel := context.WithTimeout(ctx, deadline)
	report, err := trial(tctx, repo, base, ch.HeadSHA, onto)
	cancel()
	if err != nil {
		c.checkFailed(ctx, k, err)
		return
	}
	c.attribute(ctx, k.workspace, report)
	var moved bool
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		now, ontoNow, live, err := c.mergePair(ctx, tx, k.workspace, k.number)
		if err != nil || !live {
			return err
		}
		if moved = now.HeadSHA != ch.HeadSHA || ontoNow != onto || baseColumn(now.BaseSHA) != baseColumn(ch.BaseSHA); moved {
			return nil
		}
		if holdsRestackConflict(now) {
			return nil
		}
		if held, err := c.holdsTargetConflict(ctx, tx, now); err != nil || held {
			return err
		}
		return c.StoreMergeability(ctx, tx, k.workspace, k.number, ch.HeadSHA, onto, report)
	})
	if err != nil {
		c.checkFailed(ctx, k, err)
		return
	}
	if moved {
		c.merge.pend(k)
	}
}

// holdsTargetConflict reports whether the change ch, read in tx, reads a
// conflict that was stored, since its check was last scheduled, for its
// current head onto the current head of its target branch: the conflict a
// land of it found, which names the changes that landed there. The check
// would replace it with a report of the same head that names none, or, for
// a change on an open change below, with what it finds onto that change.
// A trigger after it sets the mergeable unknown, so that its check stores
// what it computes.
func (c *Component) holdsTargetConflict(ctx context.Context, tx *sql.Tx, ch Change) (bool, error) {
	if ch.Mergeable != MergeableConflict {
		return false, nil
	}
	var head, onto sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT mergeable_head, mergeable_onto FROM changes WHERE workspace_slug = ? AND number = ?`,
		ch.WorkspaceSlug, ch.Number).Scan(&head, &onto); err != nil {
		return false, fmt.Errorf("read the mergeable of %s: %w", ch.Ref, err)
	}
	if head.String != ch.HeadSHA || onto.String == "" {
		return false, nil
	}
	repo, err := c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return false, err
	}
	target, found, err := repo.ResolveBranch(ctx, ch.TargetBranch)
	return found && target == onto.String, err
}

// checkFailed logs the error of the check of k, which leaves its
// mergeable as it is; nil, or an error of the stop, is not logged.
func (c *Component) checkFailed(ctx context.Context, k mergeKey, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	c.deps.Logger.Warn("the mergeability check failed", "workspace", k.workspace, "number", k.number, "error", err)
}

// mergePair reads through q the change number of the workspace and the
// commit its check rebases onto: the head of the change it depends on
// while that one is draft or open, otherwise the head of its target
// branch, "" when that branch does not exist. live is false for a change
// that is neither draft nor open, or that does not exist, and for one with
// a pending restack: its head still holds the old commits of the change
// below, which a check would replay onto the new ones and report as a
// conflict; the end of the restack schedules its check again.
func (c *Component) mergePair(ctx context.Context, q Querier, workspace string, number int64) (ch Change, onto string, live bool, err error) {
	ch, err = readChange(ctx, q, workspace, number)
	if errors.Is(err, ErrChangeNotFound) {
		return Change{}, "", false, nil
	}
	if err != nil || (ch.State != StateDraft && ch.State != StateOpen) {
		return ch, "", false, err
	}
	if pending, err := hasPendingRestack(ctx, q, workspace, number); err != nil || pending {
		return ch, "", false, err
	}
	onto, err = c.ontoOf(ctx, q, ch)
	return ch, onto, err == nil, err
}

// ontoOf is the commit a replay of the change ch, read through q, builds
// on: the head of the change it depends on while that one is draft or
// open, otherwise the head of its target branch, "" when that branch does
// not exist.
func (c *Component) ontoOf(ctx context.Context, q Querier, ch Change) (string, error) {
	if ch.DependsOn != nil {
		lower, err := readChange(ctx, q, ch.WorkspaceSlug, *ch.DependsOn)
		switch {
		case err == nil && (lower.State == StateDraft || lower.State == StateOpen):
			return lower.HeadSHA, nil
		case err != nil && !errors.Is(err, ErrChangeNotFound):
			return "", err
		}
	}
	repo, err := c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return "", err
	}
	onto, _, err := repo.ResolveBranch(ctx, ch.TargetBranch)
	return onto, err
}

// ReplayPair is what a mergeability check of the change ch, read through
// q, replays its head with (TrialRebase): onto, the commit it builds on,
// "" when its target branch does not exist (ontoOf), and base, the commit
// after which its own commits begin when it is in a stack, "" for
// onto..head (ownBase). A report the check stored is of this pair, so a
// read of the change's conflict that replays it computes what the check
// would.
func (c *Component) ReplayPair(ctx context.Context, q Querier, ch Change) (onto, base string, err error) {
	if onto, err = c.ontoOf(ctx, q, ch); err != nil || onto == "" {
		return "", "", err
	}
	base, err = ownBase(ctx, q, ch)
	return onto, base, err
}

// ownBase is, for the change ch read through q, the commit after which its
// head holds only its own commits, when ch is in a stack: it depends on a
// change, it needs a restack, or it must sit on the commit the change
// below landed as. Its head may then still carry the commits of the change
// below as they were when ch was built on them, which a trial of onto..head
// would replay and could report as the conflict; its check replays only
// its own commits, as its restack does, so that a check's report and a
// restack's describe the same commits. It is "" for any other change, and
// for one with no base, whose check replays onto..head.
func ownBase(ctx context.Context, q Querier, ch Change) (string, error) {
	if ch.DependsOn == nil && !ch.NeedsRestack {
		landed, err := landedBelow(ctx, q, ch)
		if err != nil || landed == "" {
			return "", err
		}
	}
	return baseColumn(ch.BaseSHA), nil
}

// StoreMergeability stores in tx, the caller's transaction, the result of
// a mergeability check of the change number of the workspace: its head
// rebased onto onto, clean for a nil report, otherwise conflict with the
// report, together with the pair and the value computed. It is the one
// writer of a computed mergeable and of parley.change.conflict, so that a
// conflict found by a check and one found by a restack are announced once.
//
// A transition is judged against the value the last check computed, which
// an unknown in between does not replace: a conflict after a clean result,
// or after none, writes parley.change.conflict by the hub; a conflict after
// a conflict replaces the report and writes nothing; a clean result clears
// the report. A landing blocked for a missing target is re-evaluated,
// since the check found the target again, and the announcement step
// (AnnounceLandable) runs last. An error leaves tx to be rolled back, the
// result and its events with it.
func (c *Component) StoreMergeability(ctx context.Context, tx *sql.Tx, workspace string, number int64, head, onto string, report *gitrepo.ConflictReport) error {
	var last sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT mergeable_last FROM changes WHERE workspace_slug = ? AND number = ?`, workspace, number).Scan(&last)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errChangeNotFound(workspace, number)
	case err != nil:
		return fmt.Errorf("read the mergeable of %s: %w", Ref(workspace, number), err)
	}
	value, conflict := MergeableClean, json.RawMessage(nil)
	if report != nil {
		b, err := json.Marshal(report)
		if err != nil {
			return err
		}
		value, conflict = MergeableConflict, b
	}
	if err := c.SetMergeable(ctx, tx, workspace, number, value, conflict); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE changes SET mergeable_head = ?, mergeable_onto = ?, mergeable_last = ?
WHERE workspace_slug = ? AND number = ?`, head, onto, value, workspace, number); err != nil {
		return fmt.Errorf("store the mergeable of %s: %w", Ref(workspace, number), err)
	}
	ch, err := readChange(ctx, tx, workspace, number)
	if err != nil {
		return err
	}
	if report != nil && last.String != MergeableConflict {
		if err := c.emitConflict(ctx, tx, ch, report); err != nil {
			return err
		}
	}
	if ch.Landing.Status == LandingBlocked && ch.Landing.Reason != nil && *ch.Landing.Reason == "target_not_found" {
		if err := c.reevaluateLanding(ctx, tx, workspace, number); err != nil {
			return err
		}
	}
	return c.AnnounceLandable(ctx, tx, workspace, number)
}

// emitConflict writes parley.change.conflict by the hub for the change ch,
// which the report says conflicts, naming its conflicted paths.
func (c *Component) emitConflict(ctx context.Context, tx *sql.Tx, ch Change, report *gitrepo.ConflictReport) error {
	files := make([]string, 0, len(report.Files))
	for _, f := range report.Files {
		files = append(files, f.Path)
	}
	return c.emit(ctx, tx, Caller{}, ch, EventConflict, map[string]any{
		"version": ch.Version, "head_sha": ch.HeadSHA, "files": files, "needs_restack": ch.NeedsRestack,
	})
}

// reevaluateLanding derives again, in tx, the landing of the change number
// of the workspace at its current version, as a new version derives it,
// with the cause LandingCause reads: the registered evaluator answers,
// given the cause; without one the change is blocked for its cause, or
// ready.
func (c *Component) reevaluateLanding(ctx context.Context, tx *sql.Tx, workspace string, number int64) error {
	ch, err := readChange(ctx, tx, workspace, number)
	if err != nil {
		return err
	}
	cause, err := c.LandingCause(ctx, tx, ch)
	if err != nil {
		return err
	}
	evaluator := c.collaborators().evaluator
	var v Version
	if evaluator != nil {
		current, err := readVersionsAfter(ctx, tx, workspace, number, ch.Version-1, 1)
		if err != nil {
			return err
		}
		if len(current) == 0 {
			return fmt.Errorf("re-evaluate the landing of %s: version %d is missing", ch.Ref, ch.Version)
		}
		v = current[0]
	}
	landing, err := landingOf(ctx, tx, evaluator, ch, v, cause)
	if err != nil {
		return err
	}
	return storeLanding(ctx, tx, workspace, number, landing, c.now())
}

// reasonDraft is the reason a draft cannot land for.
const reasonDraft = "draft"

// LandingCause is the reason the change ch cannot land for whatever the
// landing policy requires, read through q and in the repositories, nil for
// none: draft for a draft, which is not measured; otherwise the structural
// reason its head gives against its target branch, decided as a new
// version decides it (moveCause) but without the diff and the patch id a
// version's measurement adds, since a policy replacement asks it of every
// change in its transaction: target_not_found, nothing_to_land,
// unrelated_history or merge_commits.
func (c *Component) LandingCause(ctx context.Context, q Querier, ch Change) (*string, error) {
	if ch.State == StateDraft {
		return new(reasonDraft), nil
	}
	_, _, cause, err := c.moveCause(ctx, q, ch, ch.HeadSHA)
	return cause, err
}

// landingOf is the landing of the change ch at its version v that cause
// (nil for none) blocks: the answer of the evaluator, which is given the
// cause, or without one blocked for the cause with no requirements, or
// ready.
func landingOf(ctx context.Context, tx *sql.Tx, evaluator LandingEvaluator, ch Change, v Version, cause *string) (Landing, error) {
	switch {
	case evaluator != nil:
		return evaluator.EvaluateLanding(ctx, tx, ch, v, cause)
	case cause != nil:
		return blockedLanding(*cause), nil
	}
	return readyLanding(), nil
}

// mergeKey names a change.
type mergeKey struct {
	workspace string
	number    int64
}

// mergeClock is the clock the worker spaces the checks of a change with.
type mergeClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// trialFunc trial-rebases head onto onto in repo, the commits after base
// for a base, onto..head for none: nil for clean, the conflict report
// otherwise.
type trialFunc func(ctx context.Context, repo *gitrepo.Repo, base, head, onto string) (*gitrepo.ConflictReport, error)

// TrialRebase is the trial rebase of a mergeability check, a trialFunc:
// TrialRebaseFrom of the commits after base for a base, TrialRebase of
// onto..head for none.
func TrialRebase(ctx context.Context, repo *gitrepo.Repo, base, head, onto string) (*gitrepo.ConflictReport, error) {
	if base == "" {
		return repo.TrialRebase(ctx, onto, head)
	}
	return repo.TrialRebaseFrom(ctx, base, head, onto)
}

// mergeWorker is the set of changes whose check is due or waits, each
// once whatever number of triggers it had, and when each check of a change
// last started. Its zero value is ready.
type mergeWorker struct {
	mu      sync.Mutex
	pending []mergeKey
	queued  map[mergeKey]bool
	running map[mergeKey]bool
	started map[mergeKey]time.Time
	// changed is closed, and replaced, when a change is scheduled or a
	// check ends, which wakes the waiting goroutines.
	changed chan struct{}

	// The seams the tests set; nil and zero are the hub's.
	trial    trialFunc
	deadline time.Duration
	clock    mergeClock
	gate     func(ctx context.Context, workspace string, number int64)
}

// pend schedules a check of k, unless one is already waiting.
func (w *mergeWorker) pend(k mergeKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queued == nil {
		w.queued = map[mergeKey]bool{}
	}
	if !w.queued[k] {
		w.queued[k] = true
		w.pending = append(w.pending, k)
	}
	w.signal()
}

// drop removes k from the waiting checks, for a change whose move out of
// draft or open has committed. A check of k that runs stores nothing for
// such a change.
func (w *mergeWorker) drop(k mergeKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queued[k] {
		delete(w.queued, k)
		w.pending = slices.DeleteFunc(w.pending, func(p mergeKey) bool { return p == k })
	}
}

// next takes the first waiting change that has no running check and whose
// last check started at least debounce ago, and marks it running; due is
// false when there is none. wait is then how long until the next one is
// due, -1 for none; changed and clock are what to wait on.
func (w *mergeWorker) next(debounce time.Duration) (k mergeKey, due bool, wait time.Duration, changed <-chan struct{}, clock mergeClock) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.changed == nil {
		w.changed = make(chan struct{})
	}
	clock = w.clockLocked()
	now, wait := clock.Now(), time.Duration(-1)
	for i, p := range w.pending {
		if w.running[p] {
			continue
		}
		last, ok := w.started[p]
		if at := last.Add(debounce); ok && now.Before(at) {
			if d := at.Sub(now); wait < 0 || d < wait {
				wait = d
			}
			continue
		}
		w.pending = append(w.pending[:i], w.pending[i+1:]...)
		delete(w.queued, p)
		if w.running == nil {
			w.running, w.started = map[mergeKey]bool{}, map[mergeKey]time.Time{}
		}
		w.running[p], w.started[p] = true, now
		w.forget(now, debounce)
		return p, true, 0, w.changed, clock
	}
	return mergeKey{}, false, wait, w.changed, clock
}

// forget drops the start times that no longer delay a check.
func (w *mergeWorker) forget(now time.Time, debounce time.Duration) {
	for p, at := range w.started {
		if !w.running[p] && !w.queued[p] && !now.Before(at.Add(debounce)) {
			delete(w.started, p)
		}
	}
}

// done marks the check of k ended.
func (w *mergeWorker) done(k mergeKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.running, k)
	w.signal()
}

// signal wakes the waiting goroutines.
func (w *mergeWorker) signal() {
	if w.changed != nil {
		close(w.changed)
	}
	w.changed = make(chan struct{})
}

func (w *mergeWorker) clockLocked() mergeClock {
	if w.clock == nil {
		return wallClock{}
	}
	return w.clock
}

// seams are the trial rebase, its deadline and the gate a check runs
// with.
func (w *mergeWorker) seams() (trialFunc, time.Duration, func(context.Context, string, int64)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	trial, deadline := w.trial, w.deadline
	if trial == nil {
		trial = TrialRebase
	}
	if deadline <= 0 {
		deadline = mergeableDeadline
	}
	return trial, deadline, w.gate
}
