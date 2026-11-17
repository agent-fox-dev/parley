package carrypatch_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// recordEntries records an entry of the workspace slug for each of ids, at
// the path <id>.txt.
func recordEntries(t *testing.T, h *hubtest.Hub, slug string, ids ...string) {
	t.Helper()
	ctx := context.Background()
	conflicts := make([]gitrepo.RerereConflict, 0, len(ids))
	for _, id := range ids {
		conflicts = append(conflicts, gitrepo.RerereConflict{ID: id, Path: id + ".txt"})
	}
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		return carrypatch.RecordRerereEntries(ctx, tx, slug, conflicts, time.Now())
	}); err != nil {
		t.Fatalf("record the entries of %s: %v", slug, err)
	}
}

// countEntries is the number of entries of the workspace slug.
func countEntries(t *testing.T, h *hubtest.Hub, slug string) int {
	t.Helper()
	n, err := carrypatch.CountRerereEntries(context.Background(), h.Store.Main(), slug)
	if err != nil {
		t.Fatalf("count the entries of %s: %v", slug, err)
	}
	return n
}

func TestRemoveRerereEntry_ForgetsOneEntryOfTheWorkspace(t *testing.T) {
	t.Parallel()
	h, _, _ := storeHub(t)
	ctx := context.Background()
	x, y := strings.Repeat("a", 40), strings.Repeat("b", 40)
	recordEntries(t, h, "a", x, y)
	recordEntries(t, h, "b", x)

	for _, tc := range []struct {
		slug, id string
		removed  bool
	}{
		{"a", x, true},
		{"a", x, false},
		{"a", strings.Repeat("c", 40), false},
	} {
		var removed bool
		if err := h.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
			removed, err = carrypatch.RemoveRerereEntry(ctx, tx, tc.slug, tc.id)
			return err
		}); err != nil || removed != tc.removed {
			t.Errorf("RemoveRerereEntry(%s, %s) = %v, %v; want %v", tc.slug, tc.id, removed, err, tc.removed)
		}
	}
	if a, b := countEntries(t, h, "a"), countEntries(t, h, "b"); a != 1 || b != 1 {
		t.Errorf("the entries of a and b number %d and %d, want 1 each", a, b)
	}
}

func TestRemoveRerereEntries_RemovesOnlyThatWorkspacesEntries(t *testing.T) {
	t.Parallel()
	h, _, _ := storeHub(t)
	ctx := context.Background()
	recordEntries(t, h, "a", strings.Repeat("a", 40), strings.Repeat("b", 40))
	recordEntries(t, h, "b", strings.Repeat("a", 40))

	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return carrypatch.RemoveRerereEntries(ctx, tx, "a") }); err != nil {
		t.Fatalf("RemoveRerereEntries(a): %v", err)
	}
	if a, b := countEntries(t, h, "a"), countEntries(t, h, "b"); a != 0 || b != 1 {
		t.Errorf("after the remove of a, the entries of a and b number %d and %d, want 0 and 1", a, b)
	}
}
