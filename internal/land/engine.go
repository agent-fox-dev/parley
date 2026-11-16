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
	a.Attempts++
	onto, found, err := repo.ResolveBranch(ctx, a.TargetBranch)
	if err == nil && !found {
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
	moved, err := c.commit(ctx, repo, ws, a, b)
	switch {
	case errors.Is(err, errTargetMoved):
		// The target is no longer the head the commits were built on.
		a.Status, a.Error = StatusFailed, new(ErrorContention)
		return c.end(ctx, a)
	case err != nil && !moved:
		return c.fail(ctx, a, err)
	}
	return err
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

// end writes the final status a holds, in a store transaction of its own;
// the attempt's own context may have ended, its outcome is still written.
func (c *Component) end(ctx context.Context, a Attempt) error {
	ctx = context.WithoutCancel(ctx)
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.finish(ctx, tx, a) })
}

// finish writes, in tx, the final status of the attempt a with its result
// members, finished now unless a says when. It is the one place an attempt
// becomes final.
func (c *Component) finish(ctx context.Context, tx *sql.Tx, a Attempt) error {
	if a.FinishedAt == nil {
		a.FinishedAt = new(c.now())
	}
	return finishAttempt(ctx, tx, a)
}

// now is the component's clock, in UTC.
func (c *Component) now() time.Time { return c.deps.Now().UTC() }
