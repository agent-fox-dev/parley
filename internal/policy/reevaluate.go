package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
)

// ErrChangeNotFound is the answer of a re-evaluation for a number that
// names no change of the workspace, or a slug that names no workspace. It
// is the change component's 404 change_not_found.
var ErrChangeNotFound = change.ErrChangeNotFound

// ErrChangeClosed is the answer of a re-evaluation for a change that has
// landed or was abandoned, whose landing is kept as it was when it closed.
var ErrChangeClosed = errors.New("the change is closed")

// Reevaluate evaluates again, in a store transaction of its own, the
// landing of the change number of the workspace, as ReevaluateIn does, and
// returns it with the revision of the policy it was evaluated against.
// The revision is read inside that transaction, never before it: write
// transactions run one at a time, so one that races a replacement of the
// policy either runs first, and the replacement's pass evaluates the change
// again, or runs after it, against the new revision.
func (c *Component) Reevaluate(ctx context.Context, workspace string, number int64) (Result, int64, error) {
	var (
		res Result
		rev int64
	)
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		res, rev, err = c.ReevaluateIn(ctx, tx, workspace, number)
		return err
	})
	if err != nil {
		return Result{}, 0, err
	}
	return res, rev, nil
}

// ReevaluateIn evaluates again, in tx, the landing of the draft or open
// change number of the workspace: its current version, with the cause the
// change component reads for it, against the current revision of the
// policy of its target workspace (revision 0 for a task fork), with the
// installed evidence source. It stores the result as the change's landing
// and then runs the change component's announcement step, the one writer
// of parley.change.ready, in tx, and returns the result with the revision.
// The owners of the triggers call it: a new approval or its withdrawal, a
// check report, a replacement of the policy.
//
// A change that does not exist is ErrChangeNotFound and one that is closed
// ErrChangeClosed, with nothing written. Any other error leaves tx to be
// rolled back, the landing and the event with it.
func (c *Component) ReevaluateIn(ctx context.Context, tx *sql.Tx, workspace string, number int64) (Result, int64, error) {
	ch, err := c.deps.Changes.ReadChange(ctx, tx, workspace, number)
	if err != nil {
		return Result{}, 0, err
	}
	if ch.State != change.StateDraft && ch.State != change.StateOpen {
		return Result{}, 0, fmt.Errorf("re-evaluate %s, which is %s: %w", ch.Ref, ch.State, ErrChangeClosed)
	}
	cause, err := c.deps.Changes.LandingCause(ctx, tx, ch)
	if err != nil {
		return Result{}, 0, err
	}
	res, rev, err := c.evaluate(ctx, tx, ch, ch.Version, cause, c)
	if err != nil {
		return Result{}, 0, err
	}
	l, err := res.Landing()
	if err != nil {
		return Result{}, 0, err
	}
	if err := c.deps.Changes.SetLanding(ctx, tx, ch.WorkspaceSlug, ch.Number, l); err != nil {
		return Result{}, 0, err
	}
	if err := c.deps.Changes.AnnounceLandable(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
		return Result{}, 0, err
	}
	return res, rev, nil
}

// reevaluateWorkspace evaluates again, in tx, the transaction that has just
// stored a new revision of the workspace's policy, the landing of each of
// its draft and open changes, in ascending number order, against that
// revision, and announces each one the new landing makes landable. Landed
// and abandoned changes keep the landing they closed with. The new revision
// and every landing commit together or not at all, so the first failure
// fails the replacement as a whole. It is the hub's failure whatever it
// says, a refusal of the change component included, so it is answered 500,
// never as the caller's error.
func (c *Component) reevaluateWorkspace(ctx context.Context, tx *sql.Tx, workspace string) error {
	numbers, err := c.deps.Changes.DraftAndOpen(ctx, tx, workspace)
	if err != nil {
		return fmt.Errorf("list the changes of %s to re-evaluate: %v", workspace, err)
	}
	for _, n := range numbers {
		if _, _, err := c.ReevaluateIn(ctx, tx, workspace, n); err != nil {
			return fmt.Errorf("re-evaluate %s against the new policy: %v", change.Ref(workspace, n), err)
		}
	}
	return nil
}

// EvaluateLanding is the landing the change component stores for the
// change ch at its version v, in tx, its transaction, with cause the
// reason it cannot land for whatever the policy requires: the evaluation
// against the current policy of its target workspace, read through tx. The
// change component's callers run the announcement step. Component is the
// change component's LandingEvaluator.
func (c *Component) EvaluateLanding(ctx context.Context, tx *sql.Tx, ch change.Change, v change.Version, cause *string) (change.Landing, error) {
	res, _, err := c.evaluate(ctx, tx, ch, v.Number, cause, c)
	if err != nil {
		return change.Landing{}, err
	}
	return res.Landing()
}

// EvaluateVersion evaluates, without storing anything, the landing of the
// version of the change ch whose commits are those from base ("" for
// none) to head, against the current policy of its target workspace, with
// the evidence of that version, and returns it with the revision. A land
// of a version older than the change's current one is judged by it: the
// stored landing is the current version's.
func (c *Component) EvaluateVersion(ctx context.Context, ch change.Change, version int64, base, head string) (Result, int64, error) {
	files, err := c.filesBetween(ctx, ch.WorkspaceSlug, base, head)
	if err != nil {
		return Result{}, 0, err
	}
	return c.evaluate(ctx, c.deps.Store.Main(), ch, version, nil, listedFiles(files))
}

// evaluate evaluates, through q, the version of the change ch with cause
// and the files files reads, against the current revision of the policy
// that governs changes into its target workspace, and returns the result
// with that revision.
func (c *Component) evaluate(ctx context.Context, q Querier, ch change.Change, version int64, cause *string, files ChangeFiles) (Result, int64, error) {
	rev, err := c.governing(ctx, q, ch.WorkspaceSlug)
	if err != nil {
		return Result{}, 0, err
	}
	res, err := Evaluate(ctx, q, &rev.Document, factsOf(ch, version, cause), files, c.InstalledEvidence())
	if err != nil {
		return Result{}, 0, err
	}
	return res, rev.Number, nil
}

// governing reads, through q, the current revision of the policy that
// governs the changes into the workspace slug: its own, or revision 0 for
// a task fork, which has no policy of its own.
func (c *Component) governing(ctx context.Context, q Querier, slug string) (Revision, error) {
	w, found, err := c.deps.Workspaces.Workspace(ctx, q, slug)
	if err != nil {
		return Revision{}, err
	}
	if found && w.Kind == access.KindFork {
		return revisionZero, nil
	}
	return CurrentRevision(ctx, q, slug)
}
