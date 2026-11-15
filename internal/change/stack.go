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

// stacks keeps the stacks of changes: the links between them, which it
// stores through the component's setters.
type stacks struct{ c *Component }

// LinkVersion links the change ch, whose new version is v, and measures
// the version from the head of the change it now depends on. The change
// dependsOn names, which only an open passes, for the first version, is
// the link when it can be one (else the invalid_dependency error); without
// it, the link is the nearest change below the head, or none, which
// leaves the merge base with the target branch that recording the version
// measured. The depth of ch, and of every change above it, follows the
// link. Each change directly above ch that is not built on the new head
// gets a pending restack onto it, recorded in tx, so that a version is
// never stored without the restacks it owes.
func (s stacks) LinkVersion(ctx context.Context, tx *sql.Tx, ch Change, v Version, dependsOn *int64) error {
	if err := s.link(ctx, tx, ch, v, dependsOn); err != nil {
		return err
	}
	if err := s.recomputeDepth(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
		return err
	}
	return s.pendDependants(ctx, tx, ch, v)
}

// pendDependants records a pending restack, for a new version, of each
// direct dependant of the change ch whose head does not have the head of
// ch's new version v. Ancestry is asked of the target's repository, which
// holds every version head of its changes.
func (s stacks) pendDependants(ctx context.Context, tx *sql.Tx, ch Change, v Version) error {
	deps, err := dependants(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil || len(deps) == 0 {
		return err
	}
	repo, err := s.c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return err
	}
	for _, n := range deps {
		d, err := readChange(ctx, tx, ch.WorkspaceSlug, n)
		if err != nil {
			return err
		}
		built, err := repo.IsAncestor(ctx, v.HeadSHA, d.HeadSHA)
		if err != nil {
			return err
		}
		if !built {
			if err := s.c.pendRestack(ctx, tx, d, TriggerVersion, ""); err != nil {
				return err
			}
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
// from it. A change that is not built on it keeps its base until it is
// restacked onto it.
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
	var base *string
	if built {
		base = &lower.HeadSHA
	}
	return s.store(ctx, tx, ch, &number, base)
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
// the one that moved.
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
			// Whether the change, back within the limit, must be restacked
			// onto the change below it is decided here once the change
			// is unflagged.
			err = s.c.SetNeedsRestack(ctx, tx, workspace, n, false, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
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
