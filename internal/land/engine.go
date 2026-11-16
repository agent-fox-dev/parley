package land

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
)

// run runs the running attempt a to its final status: it asks the policy
// gate about each change it covers, once, replays the changes onto the
// head of the target (replay.go), and moves the target to the result and
// records the land as one step (commit.go). Every final status is written
// by finish.
//
// A target that moved between the replay and the compare-and-swap has the
// changes built again on its new head and the swap tried again, each try
// counted in attempts, until Deps.MaxAttempts tries lost, which ends the
// attempt failed contention.
//
// An error that leaves the attempt without a final status is returned:
// one after the target moved leaves it running, since only the record of
// the land may end it then.
func (c *Component) run(ctx context.Context, a Attempt) error {
	ws, found, err := c.deps.Workspaces.Workspace(ctx, c.deps.Store.Main(), a.Workspace)
	if err == nil && !found {
		err = fmt.Errorf("workspace %s not found", a.Workspace)
	}
	if err != nil {
		return c.fail(ctx, a, err)
	}
	repo, err := c.deps.Repos(ctx, a.Workspace)
	if err != nil {
		return c.fail(ctx, a, err)
	}
	revision, blocked, err := c.judge(ctx, a)
	if err != nil {
		return c.fail(ctx, a, err)
	}
	a.PolicyRevision = &revision
	if blocked != nil {
		a.Status, a.Blocked = StatusBlocked, blocked
		return c.end(ctx, a)
	}
	for {
		a.Attempts++
		onto, found, err := repo.ResolveBranch(ctx, a.TargetBranch)
		if err == nil && !found {
			// A land never creates its target: a branch deleted since the
			// change was opened ends the attempt.
			err = fmt.Errorf("target branch %s of %s not found", a.TargetBranch, a.Workspace)
		}
		if err != nil {
			return c.fail(ctx, a, err)
		}
		b, report, err := c.build(ctx, repo, a, onto)
		switch {
		case err != nil:
			return c.fail(ctx, a, err)
		case report != nil:
			a.Status, a.Conflict = StatusConflict, report
			return c.end(ctx, a)
		}
		if c.seams.beforeSwap != nil {
			c.seams.beforeSwap(ctx, a.ID, a.Attempts)
		}
		moved, err := c.commit(ctx, repo, ws, a, b)
		switch {
		case errors.Is(err, errStopped):
			return err
		case errors.Is(err, errTargetMoved):
			// Another writer moved the target since it was read: the
			// changes are built again on its new head, up to the limit.
			if a.Attempts >= c.deps.MaxAttempts {
				a.Status, a.Error = StatusFailed, new(ErrorContention)
				return c.end(ctx, a)
			}
		case err != nil && !moved:
			return c.fail(ctx, a, err)
		default:
			return err
		}
	}
}

// judge asks the policy gate about each change a covers, bottom first, and
// returns the revision of the policy that answered, the highest when the
// answers differ, and the change whose answer was not ready, nil when
// every change may land.
func (c *Component) judge(ctx context.Context, a Attempt) (int64, *Blocked, error) {
	gate, _ := c.policy()
	caller := callerOf(a)
	var revision int64
	for _, cov := range a.Covered {
		ch, err := c.deps.Changes.ReadChange(ctx, nil, a.Workspace, cov.Number)
		if err != nil {
			return 0, nil, err
		}
		answer, err := gate.Evaluate(ctx, GateInput{Workspace: a.Workspace, Change: ch, Version: cov.Version, Caller: caller})
		if err != nil {
			return 0, nil, fmt.Errorf("the policy gate on %s: %w", change.Ref(a.Workspace, cov.Number), err)
		}
		revision = max(revision, answer.Revision)
		if answer.Status != change.LandingReady {
			return revision, &Blocked{Change: cov.Number, Status: answer.Status, Requirements: answer.Requirements}, nil
		}
	}
	return revision, nil, nil
}

// callerOf is who asked for the attempt a, as a change names its caller.
func callerOf(a Attempt) change.Caller {
	caller := change.Caller{Credential: change.Credential{Type: a.RequestedBy.ActorType, ID: a.CredentialID}}
	if a.RequestedBy.ActorID != nil {
		caller.UserID = *a.RequestedBy.ActorID
	}
	if a.RequestedBy.Username != nil {
		caller.Username = *a.RequestedBy.Username
	}
	return caller
}

// fail ends the attempt a failed, as internal, for the reason err, which
// it returns with any error of its own.
func (c *Component) fail(ctx context.Context, a Attempt, err error) error {
	a.Status, a.Error = StatusFailed, new(ErrorInternal)
	return errors.Join(err, c.end(ctx, a))
}

// end writes the final status a holds, in a store transaction of its own.
// An attempt whose context ended by its deadline ends failed timeout
// whatever a holds, once: the pool may have ended it so already. One whose
// context ended by the stop of the hub is not ended: it stays running for
// the next start to run it again.
func (c *Component) end(ctx context.Context, a Attempt) error {
	timedOut := false
	if ctx.Err() != nil {
		cause := context.Cause(ctx)
		if !errors.Is(cause, errTimedOut) {
			return fmt.Errorf("land attempt %s left running: %w", a.ID, cause)
		}
		timedOut = true
		a.Status, a.Error, a.FinishedAt = StatusFailed, new(ErrorTimeout), nil
		a.OntoSHA, a.LandedSHA, a.Noop, a.Commits, a.Conflict, a.Blocked = nil, nil, false, nil, nil, nil
	}
	ctx = context.WithoutCancel(ctx)
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.finish(ctx, tx, a) })
	if timedOut && errors.Is(err, errFinal) {
		return nil
	}
	return err
}

// finish writes, in tx, the final status of the attempt a with its result
// members, finished now unless a says when. It is the one place an attempt
// becomes final.
//
// The attempt's entry in the land journal goes with it: a final attempt
// has nothing left to complete at the next start.
func (c *Component) finish(ctx context.Context, tx *sql.Tx, a Attempt) error {
	if a.FinishedAt == nil {
		a.FinishedAt = new(c.now())
	}
	if err := finishAttempt(ctx, tx, a); err != nil {
		return err
	}
	return unjournal(ctx, tx, a.ID)
}

// fault is asked at each point of the atomic step whether the engine is
// to stop there, as a stop of the process would stop it; it is nil but in
// tests.
func (c *Component) fault(point string) error {
	if c.seams.fault == nil {
		return nil
	}
	if err := c.seams.fault(point); err != nil {
		return fmt.Errorf("%w %s: %w", errStopped, point, err)
	}
	return nil
}

// errStopped is the error of an engine stopped at a point of the atomic
// step: it writes nothing more, as a hub process that stops there.
var errStopped = errors.New("stopped")

// now is the component's clock, in UTC.
func (c *Component) now() time.Time { return c.deps.Now().UTC() }
