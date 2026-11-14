package fork

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// SchemaStep gives the workspace records what a fork keeps beside the
// workspace fields: the names of the branches and of the tags its parent
// had when it was created, each a JSON array of names, NULL for a
// repository. They are columns of the workspace table rather than a table
// of their own, so that they go with the fork's record whenever it is
// removed. The base branch is the record's default_branch. It applies after
// workspace.SchemaStep.
var SchemaStep = store.Step{
	Name:   "fork_fields",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, col := range []string{"tracking_branches TEXT", "tracking_tags TEXT"} {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE workspaces ADD COLUMN `+col); err != nil {
				return err
			}
		}
		return nil
	},
}

// Fork is the record of a task fork: its workspace record, whose
// DefaultBranch is the fork's base branch, and the names of the branches
// and the tags its parent had when the fork was created, which the fork
// tracks for as long as it lives.
type Fork struct {
	workspace.Workspace
	TrackingBranches []string
	TrackingTags     []string
}

// Insert stores the fork f in tx through workspace.Insert, so that a slug
// in use is an error matching access.ErrDuplicateSlug and a record the
// access rule refuses (a parent without a record, a parent that is a fork
// or in another org) one matching access.ErrInvalidRecord. A record that is
// not of kind fork is refused the same way, before anything is written.
// The tracking names are stored sorted and without repeats.
func Insert(ctx context.Context, tx *sql.Tx, f Fork) error {
	if f.Kind != access.KindFork {
		return fmt.Errorf("%w: a fork of kind %q", access.ErrInvalidRecord, f.Kind)
	}
	if err := workspace.Insert(ctx, tx, f.Workspace); err != nil {
		return err
	}
	branches, err := encodeNames(f.TrackingBranches)
	if err != nil {
		return fmt.Errorf("insert fork %q: %w", f.Slug, err)
	}
	tags, err := encodeNames(f.TrackingTags)
	if err != nil {
		return fmt.Errorf("insert fork %q: %w", f.Slug, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET tracking_branches = ?, tracking_tags = ? WHERE slug = ?`,
		branches, tags, f.Slug); err != nil {
		return fmt.Errorf("insert fork %q: %w", f.Slug, err)
	}
	return nil
}

// BySlug reads the fork whose slug is exactly slug through q, the store or
// the caller's transaction. A slug no workspace has, and the slug of a
// repository workspace, is found = false with a nil error.
func BySlug(ctx context.Context, q access.Querier, slug string) (Fork, bool, error) {
	w, found, err := workspace.BySlug(ctx, q, slug)
	if err != nil || !found || w.Kind != access.KindFork {
		return Fork{}, false, err
	}
	f, err := withNames(ctx, q, w)
	if errors.Is(err, sql.ErrNoRows) {
		// Removed between the two reads.
		return Fork{}, false, nil
	}
	if err != nil {
		return Fork{}, false, err
	}
	return f, true, nil
}

// CountOf is the number of forks of the workspace parent, read through q.
func CountOf(ctx context.Context, q access.Querier, parent string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE kind = ? AND parent_slug = ?`,
		access.KindFork, parent).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count the forks of %q: %w", parent, err)
	}
	return n, nil
}

// ListOf reads the forks of the workspace parent through q, newest first,
// those created at the same instant by slug.
func ListOf(ctx context.Context, q access.Querier, parent string) ([]Fork, error) {
	rows, err := q.QueryContext(ctx, `SELECT slug FROM workspaces WHERE kind = ? AND parent_slug = ? ORDER BY created_at DESC, slug`,
		access.KindFork, parent)
	if err != nil {
		return nil, fmt.Errorf("list the forks of %q: %w", parent, err)
	}
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the forks of %q: %w", parent, err)
		}
		slugs = append(slugs, slug)
	}
	// Read before the forks are, since q may be a transaction, which holds
	// one connection.
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("list the forks of %q: %w", parent, err)
	}
	forks := make([]Fork, 0, len(slugs))
	for _, slug := range slugs {
		f, found, err := BySlug(ctx, q, slug)
		if err != nil {
			return nil, err
		}
		if found {
			forks = append(forks, f)
		}
	}
	return forks, nil
}

// withNames is the fork w with its tracking names read through q.
func withNames(ctx context.Context, q access.Querier, w workspace.Workspace) (Fork, error) {
	var branches, tags sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT tracking_branches, tracking_tags FROM workspaces WHERE slug = ?`, w.Slug).
		Scan(&branches, &tags); err != nil {
		return Fork{}, fmt.Errorf("read fork %q: %w", w.Slug, err)
	}
	f := Fork{Workspace: w}
	for _, c := range []struct {
		col   sql.NullString
		names *[]string
	}{{branches, &f.TrackingBranches}, {tags, &f.TrackingTags}} {
		if !c.col.Valid {
			continue
		}
		if err := json.Unmarshal([]byte(c.col.String), c.names); err != nil {
			return Fork{}, fmt.Errorf("the tracking names of fork %q: %w", w.Slug, err)
		}
	}
	return f, nil
}

// encodeNames is the stored form of a list of names: a JSON array, sorted
// and without repeats.
func encodeNames(names []string) (string, error) {
	sorted := slices.Compact(slices.Sorted(slices.Values(names)))
	if sorted == nil {
		sorted = []string{}
	}
	b, err := json.Marshal(sorted)
	return string(b), err
}
