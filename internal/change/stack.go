package change

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// StackStep gives the change record the commit a change must sit on once
// the change below it has landed, NULL for none: the landed commit, which
// no draft or open change has as its head, so that depends_on, which only
// ever names a draft or open change, cannot hold it. It applies after
// SchemaStep.
var StackStep = store.Step{
	Name:   "change_stack",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE changes ADD COLUMN restack_onto TEXT`)
		return err
	},
}

// StackLinker is the component's own stack linker, which links each new
// version to the change it is built on, read from the commit graph.
func (c *Component) StackLinker() StackLinker { return stacks{c} }

// Restacker is the component's own restacker, which records the restack an
// abandon asks for a reparented dependant.
func (c *Component) Restacker() Restacker { return stacks{c} }

// stacks keeps the stacks of changes: the links between them, which it
// stores through the component's setters, and the restacks they owe.
type stacks struct{ c *Component }

// RequestRestack records in tx a pending restack of the change ch for the
// trigger, onto the commit it must sit on when the restack runs: the head
// of the change it depends on, which the caller has set, or, with none,
// the head of its target branch. The restack replays only the commits
// after ch's base, so the commits of a change below it that is gone are
// left out. The depth of ch, and of every change above it, follows its
// link first.
func (s stacks) RequestRestack(ctx context.Context, tx *sql.Tx, ch Change, trigger string) error {
	if err := s.recomputeDepth(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
		return err
	}
	return s.c.pendRestack(ctx, tx, ch, trigger, "")
}

// LinkVersion links the change ch, whose new version is v, and measures
// the version from the head of the change it now depends on. The change
// dependsOn names, which only an open passes, for the first version, is
// the link when it can be one (else the invalid_dependency error); without
// it, the link is the nearest change below the head, or none, which
// leaves the merge base with the target branch that recording the version
// measured. The depth of ch, and of every change above it, follows the
// link, a restack ch was flagged as needing may be answered by the version
// (settle), and a restack ch has pending runs again (renewRestack). Each
// change directly above ch gets a pending restack onto the new head,
// recorded in tx, so that a version is never stored without the restacks
// it owes.
func (s stacks) LinkVersion(ctx context.Context, tx *sql.Tx, ch Change, v Version, dependsOn *int64) error {
	if err := s.link(ctx, tx, ch, v, dependsOn); err != nil {
		return err
	}
	if err := s.recomputeDepth(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
		return err
	}
	if err := s.settle(ctx, tx, ch, v); err != nil {
		return err
	}
	if err := s.renewRestack(ctx, tx, ch, v); err != nil {
		return err
	}
	return s.pendDependants(ctx, tx, ch)
}

// renewRestack records again in tx, as a newer record, the pending restack
// the change ch has, unless its new version v is a restack's own, whose
// restack removes the record. The worker keeps a record whose restack
// failed, or lost every swap, until a newer one (restackWorker.keep), and
// the change's mergeability is not computed while the record is pending
// (mergePair); the change's own version may put it on the commit it must
// sit on, or move the branch the swaps lost to, so its restack runs again
// and ends the record either way.
func (s stacks) renewRestack(ctx context.Context, tx *sql.Tx, ch Change, v Version) error {
	if v.Kind == KindRestack {
		return nil
	}
	res, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO change_restacks (workspace_slug, number, trigger, onto)
SELECT workspace_slug, number, trigger, onto FROM change_restacks WHERE workspace_slug = ? AND number = ?`, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return fmt.Errorf("record the restack of %s again: %w", ch.Ref, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	s.c.restacks.wake()
	return nil
}

// settle clears in tx the needs_restack of the change ch, set because a
// restack conflicted or automatic restacks are off, once its new version v
// sits on the commit it must (sitOn): the head of the change it now
// depends on; with none, the commit the change below it landed as, which
// it then no longer keeps, or else the head of its target branch. A change
// the version unlinked has no change below it to sit on, so its flag goes
// with the link. Any other flag, a version that does not sit on that
// commit, and a change with no commit to sit on leave the change as it is.
func (s stacks) settle(ctx context.Context, tx *sql.Tx, ch Change, v Version) error {
	now, err := readChange(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return err
	}
	reason := reasonOf(now)
	if !now.NeedsRestack || (reason != RestackConflict && reason != RestackAutoOff) {
		return nil
	}
	if now.DependsOn != nil || ch.DependsOn == nil {
		must, err := s.c.sitOn(ctx, tx, now, "")
		if err != nil || must == "" {
			return err
		}
		repo, err := s.c.deps.Repos(ctx, ch.WorkspaceSlug)
		if err != nil {
			return err
		}
		built, err := repo.IsAncestor(ctx, must, v.HeadSHA)
		if err != nil || !built {
			return err
		}
	}
	if err := s.c.clearNeedsRestack(ctx, tx, ch.WorkspaceSlug, ch.Number, reason); err != nil {
		return err
	}
	return clearRestackOnto(ctx, tx, ch.WorkspaceSlug, ch.Number)
}

// pendDependants records a pending restack, for a new version, of each
// direct dependant of the change ch, one already built on the new head
// too: its restack finds it so (up to date) and clears a flag that a
// conflict, or automatic restacks off, left on it below an older head.
// With automatic restacks off, pendRestack only flags the dependants that
// do not sit on the new head.
func (s stacks) pendDependants(ctx context.Context, tx *sql.Tx, ch Change) error {
	deps, err := dependants(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return err
	}
	for _, n := range deps {
		d, err := readChange(ctx, tx, ch.WorkspaceSlug, n)
		if err != nil {
			return err
		}
		if err := s.c.pendRestack(ctx, tx, d, TriggerVersion, ""); err != nil {
			return err
		}
	}
	return nil
}

// link links the change ch for its new version v, as LinkVersion says.
func (s stacks) link(ctx context.Context, tx *sql.Tx, ch Change, v Version, dependsOn *int64) error {
	if dependsOn != nil {
		return s.linkTo(ctx, tx, ch, *dependsOn)
	}
	lower, err := s.below(ctx, tx, ch, v.HeadSHA)
	if err != nil {
		return err
	}
	if lower == nil {
		return s.store(ctx, tx, ch, nil, nil)
	}
	return s.store(ctx, tx, ch, &lower.number, &lower.head)
}

// SetDependsOn links the change ch to the change a request names, as
// LinkVersion does an explicit link, or, for nil, unlinks it and measures
// it from its merge base with the target branch; the change already
// linked so is left as it is. The commit the change's mergeability is
// computed against moves with the link, so its check is scheduled again.
func (s stacks) SetDependsOn(ctx context.Context, tx *sql.Tx, ch Change, dependsOn *int64) error {
	if sameLink(ch.DependsOn, dependsOn) {
		return nil
	}
	// The other members of the request, a new target branch among them,
	// were stored before.
	ch, err := readChange(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return err
	}
	if dependsOn != nil {
		err = s.linkTo(ctx, tx, ch, *dependsOn)
	} else {
		err = s.unlink(ctx, tx, ch)
	}
	if err != nil {
		return err
	}
	if err := s.recomputeDepth(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
		return err
	}
	return s.c.ScheduleMergeable(ctx, tx, ch.WorkspaceSlug, ch.Number)
}

// linkTo links the change ch to the change number of its workspace, which
// a caller named, and, when ch is built on that change's head, measures ch
// from it. A change that is not built on it keeps its base and gets a
// pending restack onto it, recorded in tx, for the caller's request (or the
// auto_restack_off flag, as pendRestack says), so that needs_restack keeps
// meaning that a change is not built on the change below it.
func (s stacks) linkTo(ctx context.Context, tx *sql.Tx, ch Change, number int64) error {
	lower, err := checkDependency(ctx, tx, ch, number)
	if err != nil {
		return err
	}
	repo, err := s.c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return err
	}
	built, err := repo.IsAncestor(ctx, lower.HeadSHA, ch.HeadSHA)
	if err != nil {
		return err
	}
	if built {
		return s.store(ctx, tx, ch, &number, &lower.HeadSHA)
	}
	if err := s.store(ctx, tx, ch, &number, nil); err != nil {
		return err
	}
	return s.c.pendRestack(ctx, tx, ch, TriggerManual, "")
}

// unlink unlinks the change ch and measures it from the merge base of its
// head with its target branch; with the target branch missing it keeps its
// base, as a version recorded then does.
func (s stacks) unlink(ctx context.Context, tx *sql.Tx, ch Change) error {
	repo, err := s.c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return err
	}
	target, found, err := repo.ResolveBranch(ctx, ch.TargetBranch)
	if err != nil {
		return err
	}
	var base *string
	if found {
		mergeBase, err := repo.MergeBase(ctx, ch.HeadSHA, target)
		if err != nil {
			return err
		}
		base = &mergeBase
	}
	return s.store(ctx, tx, ch, nil, base)
}

// store stores link as the depends_on of the change ch and base, unless
// nil, as the base of its current version, each only when it differs from
// what ch has.
func (s stacks) store(ctx context.Context, tx *sql.Tx, ch Change, link *int64, base *string) error {
	if !sameLink(ch.DependsOn, link) {
		if err := s.c.SetDependsOn(ctx, tx, ch.WorkspaceSlug, ch.Number, link); err != nil {
			return err
		}
	}
	if base == nil || baseColumn(ch.BaseSHA) == *base {
		return nil
	}
	return s.c.SetBase(ctx, tx, ch.WorkspaceSlug, ch.Number, *base)
}

// checkDependency reads the change number of the workspace of the change
// ch, which a caller names as the one ch depends on, and answers 400
// invalid_dependency when it cannot be: no such change, ch itself, a
// change into another target branch, one that is neither draft nor open,
// or one that already sits on ch, which would close a cycle.
func checkDependency(ctx context.Context, q Querier, ch Change, number int64) (Change, error) {
	refuse := func(format string, args ...any) (Change, error) {
		return Change{}, server.NewError("invalid_dependency", http.StatusBadRequest, "depends_on: "+fmt.Sprintf(format, args...))
	}
	if number == ch.Number {
		return refuse("%s cannot depend on itself", ch.Ref)
	}
	lower, err := readChange(ctx, q, ch.WorkspaceSlug, number)
	switch {
	case errors.Is(err, ErrChangeNotFound):
		return refuse("no change %s", Ref(ch.WorkspaceSlug, number))
	case err != nil:
		return Change{}, err
	case lower.TargetBranch != ch.TargetBranch:
		return refuse("%s targets %s, not %s", lower.Ref, lower.TargetBranch, ch.TargetBranch)
	case lower.State != StateDraft && lower.State != StateOpen:
		return refuse("%s is %s", lower.Ref, lower.State)
	}
	seen := map[int64]bool{number: true}
	for down := lower.DependsOn; down != nil && !seen[*down]; {
		if *down == ch.Number {
			return refuse("%s already depends on %s", lower.Ref, ch.Ref)
		}
		seen[*down] = true
		next, err := linkOf(ctx, q, ch.WorkspaceSlug, *down)
		if err != nil {
			return Change{}, err
		}
		down = next
	}
	return lower, nil
}

// recomputeDepth flags stack_too_deep each change of the workspace, from
// the change number up through its dependants, that sits deeper in its
// stack than the limit, and unflags one flagged so that no longer does. A
// change past the limit keeps its link and its versions: the flag only
// keeps it out of the restacks, whose cost grows with every change above
// the one that moved. A change back within the limit missed the restacks
// of the moves below it, so it gets a pending restack when it does not sit
// on what it must (unless it has one pending already, whose commit to sit
// on is read when it runs).
func (s stacks) recomputeDepth(ctx context.Context, tx *sql.Tx, workspace string, number int64) error {
	above, err := stackAbove(ctx, tx, workspace, number)
	if err != nil {
		return err
	}
	depth, err := depthOf(ctx, tx, workspace, number)
	if err != nil {
		return err
	}
	// stackAbove lists a change after the change it depends on, so the
	// depth of each change above is one more than that of its link.
	depths := map[int64]int{number: depth}
	for _, n := range append([]int64{number}, above...) {
		if n != number {
			link, err := linkOf(ctx, tx, workspace, n)
			if err != nil {
				return err
			}
			if link == nil {
				return fmt.Errorf("depth of %s: it has no link", Ref(workspace, n))
			}
			depths[n] = depths[*link] + 1
		}
		var reason *string
		err := tx.QueryRowContext(ctx, `SELECT restack_reason FROM changes WHERE workspace_slug = ? AND number = ?`, workspace, n).Scan(&reason)
		if err != nil {
			return fmt.Errorf("depth of %s: %w", Ref(workspace, n), err)
		}
		deep, flagged := depths[n] > s.c.deps.MaxStackDepth, reason != nil && *reason == RestackTooDeep
		switch {
		case deep && !flagged:
			err = s.c.SetNeedsRestack(ctx, tx, workspace, n, true, new(RestackTooDeep))
		case !deep && flagged:
			err = s.unflagDeep(ctx, tx, workspace, n)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// unflagDeep clears in tx the stack_too_deep of the change number of the
// workspace, now within the limit, and records a pending restack of it when
// it does not sit on the commit it must, as recomputeDepth says.
func (s stacks) unflagDeep(ctx context.Context, tx *sql.Tx, workspace string, number int64) error {
	if err := s.c.SetNeedsRestack(ctx, tx, workspace, number, false, nil); err != nil {
		return err
	}
	pending, err := hasPendingRestack(ctx, tx, workspace, number)
	if err != nil || pending {
		return err
	}
	ch, err := readChange(ctx, tx, workspace, number)
	if err != nil {
		return err
	}
	behind, err := s.c.behind(ctx, tx, ch, "")
	if err != nil || !behind {
		return err
	}
	return s.c.pendRestack(ctx, tx, ch, TriggerVersion, "")
}

// tooDeep reports whether the change number of the workspace sits deeper
// in its stack than the limit, read through q. The depth is counted from
// the links, which restack_reason follows only once recomputeDepth ran,
// and which a restack conflict replaces.
func (c *Component) tooDeep(ctx context.Context, q Querier, workspace string, number int64) (bool, error) {
	depth, err := depthOf(ctx, q, workspace, number)
	return depth > c.deps.MaxStackDepth, err
}

// depthOf is the depth of the change number of the workspace in its stack:
// the number of changes in its chain of depends_on links down to a change
// with none, itself included. A loop, which the links never close, ends
// the count where the chain meets itself.
func depthOf(ctx context.Context, q Querier, workspace string, number int64) (int, error) {
	seen := map[int64]bool{number: true}
	link, err := linkOf(ctx, q, workspace, number)
	for err == nil && link != nil && !seen[*link] {
		seen[*link] = true
		link, err = linkOf(ctx, q, workspace, *link)
	}
	return len(seen), err
}

// linkOf is the depends_on of the change number of the workspace, nil for
// none or for no such change.
func linkOf(ctx context.Context, q Querier, workspace string, number int64) (*int64, error) {
	var link *int64
	err := q.QueryRowContext(ctx, `SELECT depends_on FROM changes WHERE workspace_slug = ? AND number = ?`, workspace, number).Scan(&link)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return link, err
}

// candidate is a change a new head may be built on: its number and head.
type candidate struct {
	number int64
	head   string
}

// below is the change the head of the change ch is built on, nil for none:
// of the other draft and open changes of its workspace and target branch,
// the one whose head is a proper ancestor of head with the fewest commits
// between them, the lowest number on a tie. A change with the same head is
// never one, so that two changes never sit on each other. Ancestry is
// asked of the target's repository, which holds the head of every version
// of its changes under their version refs, also of a fork's change, which
// another fork's repository does not.
func (s stacks) below(ctx context.Context, q Querier, ch Change, head string) (*candidate, error) {
	rows, err := q.QueryContext(ctx, `SELECT number, head_sha FROM changes WHERE workspace_slug = ? AND target_branch = ?
	AND state IN ('draft', 'open') AND number <> ? AND head_sha <> ? ORDER BY number`, ch.WorkspaceSlug, ch.TargetBranch, ch.Number, head)
	if err != nil {
		return nil, fmt.Errorf("find the changes below %s: %w", ch.Ref, err)
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.number, &c.head); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(candidates) == 0 {
		return nil, err
	}
	repo, err := s.c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return nil, err
	}
	var nearest *candidate
	distance := 0
	for _, c := range candidates {
		ancestor, err := repo.IsAncestor(ctx, c.head, head)
		if err != nil {
			return nil, err
		}
		if !ancestor {
			continue
		}
		between, err := repo.CommitsBetween(ctx, c.head, head, 0)
		if err != nil {
			return nil, err
		}
		if nearest == nil || len(between) < distance {
			nearest, distance = &c, len(between)
		}
	}
	return nearest, nil
}

// sameLink reports whether a and b name the same change, or both none.
func sameLink(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
