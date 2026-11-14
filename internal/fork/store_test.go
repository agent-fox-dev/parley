package fork_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/fork"
)

func TestStore_InsertReadCountAndList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t, nil)
	older := w.seedFork(t, "demo--a", func(f *fork.Fork) { f.TrackingBranches = []string{"release", "main", "main"} })
	w.advance(time.Second)
	w.seedFork(t, "demo--c")
	w.seedFork(t, "demo--b")

	got, found, err := fork.BySlug(ctx, w.Store.Main(), "demo--a")
	if err != nil || !found {
		t.Fatalf("BySlug(demo--a) = %v, %v", found, err)
	}
	if !slices.Equal(got.TrackingBranches, []string{"main", "release"}) || !slices.Equal(got.TrackingTags, []string{"v1"}) ||
		*got.ParentSlug != parentSlug || *got.ForkPoint != *older.ForkPoint || *got.DefaultBranch != "main" {
		t.Errorf("BySlug(demo--a) = %+v, want the stored fork with its names sorted once", got)
	}
	for _, slug := range []string{parentSlug, "nothing"} {
		if _, found, err := fork.BySlug(ctx, w.Store.Main(), slug); found || err != nil {
			t.Errorf("BySlug(%s) = %v, %v; want not found", slug, found, err)
		}
	}
	if n, err := fork.CountOf(ctx, w.Store.Main(), parentSlug); n != 3 || err != nil {
		t.Errorf("CountOf = %d, %v; want 3", n, err)
	}
	list, err := fork.ListOf(ctx, w.Store.Main(), parentSlug, fork.ListFilter{})
	var slugs []string
	for _, f := range list {
		slugs = append(slugs, f.Slug)
	}
	if err != nil || !slices.Equal(slugs, []string{"demo--b", "demo--c", "demo--a"}) {
		t.Errorf("ListOf = %v, %v; want newest first, then by slug", slugs, err)
	}

	dup := older
	dup.TrackingBranches = nil
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return fork.Insert(ctx, tx, dup) }); !errors.Is(err, access.ErrDuplicateSlug) {
		t.Errorf("an insert of a slug in use = %v, want access.ErrDuplicateSlug", err)
	}
	if got, _, _ := fork.BySlug(ctx, w.Store.Main(), "demo--a"); len(got.TrackingBranches) != 2 {
		t.Errorf("the refused insert changed the stored fork's names to %v", got.TrackingBranches)
	}
}
