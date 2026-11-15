package change

import (
	"context"
	"database/sql"
	"fmt"

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

// LinkVersion links the change ch, whose new version is v, to the nearest
// change below its head, or to none, and measures the version from that
// change's head or, with none, leaves the merge base with the target
// branch that recording the version measured.
func (s stacks) LinkVersion(ctx context.Context, tx *sql.Tx, ch Change, v Version, _ *int64) error {
	lower, err := s.below(ctx, tx, ch, v.HeadSHA)
	if err != nil {
		return err
	}
	var link *int64
	if lower != nil {
		link = &lower.number
	}
	if !sameLink(ch.DependsOn, link) {
		if err := s.c.SetDependsOn(ctx, tx, ch.WorkspaceSlug, ch.Number, link); err != nil {
			return err
		}
	}
	if lower == nil || baseColumn(ch.BaseSHA) == lower.head {
		return nil
	}
	return s.c.SetBase(ctx, tx, ch.WorkspaceSlug, ch.Number, lower.head)
}

// SetDependsOn stores the link a request names, and writes nothing when the
// change already has it.
func (s stacks) SetDependsOn(ctx context.Context, tx *sql.Tx, ch Change, dependsOn *int64) error {
	if sameLink(ch.DependsOn, dependsOn) {
		return nil
	}
	return s.c.SetDependsOn(ctx, tx, ch.WorkspaceSlug, ch.Number, dependsOn)
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
