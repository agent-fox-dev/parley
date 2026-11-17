package carrypatch_test

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// The provider names the branch of every patch of the workspace that is
// not deleted, whatever its status, and nothing of another workspace.
func TestExcludedBranches_NonDeletedPatches(t *testing.T) {
	t.Parallel()
	var comp *carrypatch.Component
	h := &readHub{Hub: hubtest.Start(t, func(c *hub.Composition) { comp = c.CarryPatch() })}
	h.put(t,
		livePatch("cp", "patch/a", 1),
		withEdit(livePatch("cp", "patch/b", 2), withStatus(carrypatch.StatusConflict, "a.txt")),
		withEdit(livePatch("cp", "patch/c", 3), withStatus(carrypatch.StatusDisabled)),
		withEdit(livePatch("cp", "patch/d", 4), withStatus(carrypatch.StatusMergedUpstream)),
		deletedPatch("cp", "patch/e", at(10, 0)),
		livePatch("other", "patch/x", 1),
	)

	ctx := context.Background()
	names, err := comp.ExcludedBranches(ctx, "cp")
	if err != nil {
		t.Fatalf("ExcludedBranches(cp): %v", err)
	}
	slices.Sort(names)
	if want := []string{"patch/a", "patch/b", "patch/c", "patch/d"}; !slices.Equal(names, want) {
		t.Errorf("ExcludedBranches(cp) = %v, want %v", names, want)
	}
	if names, err := comp.ExcludedBranches(ctx, "empty-ws"); err != nil || len(names) != 0 {
		t.Errorf("ExcludedBranches(empty-ws) = %v, %v; want none", names, err)
	}
}

// withEdit is p changed by edit.
func withEdit(p carrypatch.Patch, edit func(p *carrypatch.Patch)) carrypatch.Patch {
	edit(&p)
	return p
}

// A patch list that cannot be read makes the provider fail, and the sync
// stops before any branch is compared.
func TestExcludedBranches_StoreFailure(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.addPatch(t, "patch/a")
	f.originAhead(t, "feat", "origin")
	if _, err := f.Store.Main().Exec(`ALTER TABLE patches RENAME TO patches_hidden`); err != nil {
		t.Fatalf("hide the patch list: %v", err)
	}

	names, err := f.comp.ExcludedBranches(context.Background(), f.slug)
	if err == nil || len(names) != 0 {
		t.Errorf("ExcludedBranches(%s) = %v, %v; want an error and no names", f.slug, names, err)
	}

	before, ws := f.heads(t), f.record(t)
	r := f.sync(t, "")
	if r.status != http.StatusInternalServerError || r.Error.ErrorType != "internal" {
		t.Fatalf("sync = %d %s, want 500 internal", r.status, r.raw)
	}
	if got := f.heads(t); !maps.Equal(got, before) {
		t.Errorf("branches = %v, want %v unchanged", got, before)
	}
	if after := f.record(t); !equalTime(after.LastSyncAt, ws.LastSyncAt) {
		t.Errorf("last_sync_at = %v, want %v unchanged", after.LastSyncAt, ws.LastSyncAt)
	}
}
