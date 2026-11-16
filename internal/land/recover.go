package land

import (
	"context"
	"errors"
	"fmt"
)

// Recover hands every attempt the store holds as running to the pool
// again, each with a deadline of Deps.Timeout from now: a stop of the hub
// leaves the attempts it was running running, and the next start runs
// them to a final status. It runs at the start, once the land journal is
// reconciled, so that a land the stop left half recorded is complete and
// final before it, and before the hub serves requests, so that no attempt
// is handed over twice; the pool runs them while requests are served. An
// attempt keeps its id, its creation and who asked for it, whose land its
// events are.
//
// An attempt whose entry in the land journal Reconcile kept is left out:
// its target may have moved, which only the record of that land may end,
// so it stays running, its entry kept for the next start to complete,
// rather than run again.
func (c *Component) Recover(ctx context.Context) error {
	running, err := readAttempts(ctx, c.deps.Store.Main(), `SELECT `+attemptColumns+` FROM land_attempts a
WHERE a.status = 'running' AND NOT EXISTS (SELECT 1 FROM land_journal j WHERE j.land_id = a.id)
ORDER BY a.created_at, a.id`)
	if err != nil {
		return err
	}
	now := c.now()
	for _, a := range running {
		c.hand(a, now, true)
	}
	return nil
}

// rerun runs the attempt a again after a start, from the land's
// preconditions, with what the attempt stores: its changes at their
// versions, its strategy, trigger, session and requested override, and
// who asked for it. A precondition that no longer holds, such as a change
// abandoned while the hub was stopped, ends it failed internal: there is
// no request left to refuse.
func (c *Component) rerun(ctx context.Context, a Attempt) error {
	if err := c.readmit(ctx, a); err != nil {
		return c.fail(ctx, a, fmt.Errorf("land attempt %s cannot run again: %w", a.ID, err))
	}
	return c.run(ctx, a)
}

// readmit checks the preconditions of the land a stores, as the request
// that created it had them checked, leaving a itself out of the running
// attempts. The versions are not compared: the attempt lands the ones it
// stores, as it would have without the stop.
func (c *Component) readmit(ctx context.Context, a Attempt) error {
	if len(a.Covered) == 0 {
		return errors.New("it covers no change")
	}
	top, err := c.deps.Changes.ReadChange(ctx, nil, a.Workspace, a.Covered[len(a.Covered)-1].Number)
	if err != nil {
		return err
	}
	_, err = c.admit(ctx, c.deps.Store.Main(), top, nil, len(a.Covered) > 1, a.ID)
	return err
}
