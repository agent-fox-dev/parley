package fork_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/fork"
)

func TestStore_InsertReadAndCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t, nil)
	older := w.seedFork(t, "demo--a", func(f *fork.Fork) { f.TrackingBranches = []string{"release", "main", "main"} })
	w.seedFork(t, "demo--b")
	w.seedFork(t, "demo--c")

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

	dup := older
	dup.TrackingBranches = nil
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return fork.Insert(ctx, tx, dup) }); !errors.Is(err, access.ErrDuplicateSlug) {
		t.Errorf("an insert of a slug in use = %v, want access.ErrDuplicateSlug", err)
	}
	if got, _, _ := fork.BySlug(ctx, w.Store.Main(), "demo--a"); len(got.TrackingBranches) != 2 {
		t.Errorf("the refused insert changed the stored fork's names to %v", got.TrackingBranches)
	}
}
