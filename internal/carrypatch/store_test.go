package carrypatch_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// storeHub is a hub whose store holds, for workspace a, the live patch of
// branch live and the deleted patch of branch gone, and for workspace b the
// live patch of branch other.
func storeHub(t *testing.T) (*hubtest.Hub, *carrypatch.Component, map[string]carrypatch.Patch) {
	t.Helper()
	var comp *carrypatch.Component
	h := hubtest.Start(t, func(c *hub.Composition) { comp = c.CarryPatch() })
	now := time.Now().UTC()
	patches := map[string]carrypatch.Patch{
		"live":  {ID: "6f1d7f2e-0d55-4c1c-9d0e-000000000001", Workspace: "a", BranchName: "live", Position: new(1), Status: carrypatch.StatusActive},
		"gone":  {ID: "6f1d7f2e-0d55-4c1c-9d0e-000000000002", Workspace: "a", BranchName: "gone", Status: carrypatch.StatusDeleted, DeletedAt: &now},
		"other": {ID: "6f1d7f2e-0d55-4c1c-9d0e-000000000003", Workspace: "b", BranchName: "other", Position: new(1), Status: carrypatch.StatusActive},
	}
	ctx := context.Background()
	err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, p := range patches {
			p.AddedAt, p.UpdatedAt = now, now
			if err := carrypatch.InsertPatch(ctx, tx, p); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store the patches: %v", err)
	}
	return h, comp, patches
}

func TestIsPatchBranch_NamesTheLivePatchesOfTheWorkspace(t *testing.T) {
	t.Parallel()
	_, comp, _ := storeHub(t)
	cases := []struct {
		slug, branch string
		want         bool
	}{
		{"a", "live", true},
		{"a", "gone", false},
		{"a", "other", false},
		{"b", "other", true},
		{"a", "unknown", false},
	}
	for _, c := range cases {
		got, err := comp.IsPatchBranch(context.Background(), c.slug, c.branch)
		if err != nil || got != c.want {
			t.Errorf("IsPatchBranch(%s, %s) = %v, %v; want %v", c.slug, c.branch, got, err, c.want)
		}
	}
}

func TestRemoveWorkspace_RemovesOnlyThatWorkspacesPatches(t *testing.T) {
	t.Parallel()
	h, _, patches := storeHub(t)
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return carrypatch.RemoveWorkspace(ctx, tx, "a") }); err != nil {
		t.Fatalf("RemoveWorkspace(a): %v", err)
	}
	for name, p := range patches {
		_, found, err := carrypatch.ReadPatch(ctx, h.Store.Main(), p.Workspace, p.ID)
		if err != nil || found != (p.Workspace == "b") {
			t.Errorf("after the remove of a, the patch %s of %s is found %v (%v), want %v", name, p.Workspace, found, err, p.Workspace == "b")
		}
	}
}
