package carrypatch_test

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestDeletePatch_SoftDeletes(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	cases := []struct {
		letter string
		want   []string
	}{
		{"A", []string{"patch/b", "patch/c"}},
		{"B", []string{"patch/a", "patch/c"}},
		{"C", []string{"patch/a", "patch/b"}},
	}
	for i, c := range cases {
		// Serial: each row advances the hub's clock and reads its stamp.
		t.Run(c.letter, func(t *testing.T) {
			slug := fmt.Sprintf("delete-%d", i)
			ps := h.carryList(t, slug, workspace.StatusReady, workspace.MarkerNone)
			h.alter(t, slug, ps["B"].ID, func(p *carrypatch.Patch) { p.Status, p.ConflictFiles = carrypatch.StatusConflict, []string{"a.go"} })
			dir := h.bareCopy(t, slug)
			branch := ps[c.letter].BranchName
			refs := []string{"refs/heads/" + branch, carrypatch.ReplacedRefPrefix + branch}
			before := []string{refIn(dir, refs[0]), refIn(dir, refs[1])}
			h.advance(time.Minute)

			r := h.remove(t, slug, ps[c.letter].ID, nil)
			if r.status != http.StatusNoContent || len(r.raw) != 0 {
				t.Fatalf("DELETE %s = %d %q, want 204 with an empty body", branch, r.status, r.raw)
			}
			g := h.get(t, "/workspaces/"+slug+"/patches/"+ps[c.letter].ID)
			if g.status != http.StatusOK || g.patch["status"] != "deleted" || g.patch["position"] != nil ||
				g.patch["deleted_at"] != h.stamp() || !reflect.DeepEqual(g.patch["conflict_files"], []any{}) {
				t.Errorf("GET the deleted patch = %d %s, want status deleted, position null, deleted_at %s and conflict_files []",
					g.status, g.raw, h.stamp())
			}
			list := h.get(t, "/workspaces/"+slug+"/patches")
			if got := list.branches(); !slices.Equal(got, c.want) {
				t.Errorf("the list after deleting %s is %v, want %v", branch, got, c.want)
			}
			for i, it := range list.Items {
				if it["position"] != float64(i+1) {
					t.Errorf("the list after deleting %s has %v at %v, want %d", branch, it["branch_name"], it["position"], i+1)
				}
			}
			if got := h.get(t, "/workspaces/"+slug+"/patches?include_deleted=true").branches(); !slices.Equal(got, append(c.want, branch)) {
				t.Errorf("the list with the deleted patches is %v, want %v", got, append(c.want, branch))
			}
			for j, ref := range refs {
				if got := refIn(dir, ref); got != before[j] {
					t.Errorf("the delete moved %s from %q to %q", ref, before[j], got)
				}
			}
		})
	}
}

func TestDeletePatch_EmitsOneEvent(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	if r := h.remove(t, "carry", ps["B"].ID, nil); r.status != http.StatusNoContent {
		t.Fatalf("DELETE B = %d %s, want 204", r.status, r.raw)
	}
	e := h.patchEvents(t, "carry", "parley.patch.delete")
	if len(e) != 1 {
		t.Fatalf("DELETE wrote %d parley.patch.delete events, want 1", len(e))
	}
	if got := e[0]; got.ResourceType != "patch" || got.ResourceID != ps["B"].ID || got.ActorID == nil || *got.ActorID != h.owner ||
		!reflect.DeepEqual(got.Metadata, map[string]any{"branch_name": "patch/b", "reason": "api"}) {
		t.Errorf("the event names %s %s by %s with %v; want patch %s by %s with branch_name patch/b and reason api",
			got.ResourceType, got.ResourceID, str(got.ActorID), got.Metadata, ps["B"].ID, h.owner)
	}
	if u := h.patchEvents(t, "carry", "parley.patch.update"); len(u) != 0 {
		t.Errorf("DELETE wrote %d parley.patch.update events, want none", len(u))
	}
	if n := h.rebuilds.calls(); n != 0 {
		t.Errorf("DELETE asked for %d rebuilds, want none", n)
	}

	t.Run("a failed event write keeps the patch", func(t *testing.T) {
		before := h.patches(t, "carry")
		h.failEvents.Store(true)
		defer h.failEvents.Store(false)
		if r := h.remove(t, "carry", ps["A"].ID, nil); r.status != http.StatusInternalServerError {
			t.Errorf("DELETE A with a failing event log = %d %s, want 500", r.status, r.raw)
		}
		if after := h.patches(t, "carry"); !reflect.DeepEqual(after, before) {
			t.Errorf("a failed delete changed the patches:\n got %+v\nwant %+v", after, before)
		}
		if a := h.stored(t, "carry", ps["A"].ID); a.Status != carrypatch.StatusActive {
			t.Errorf("after a failed delete A is %s, want active", a.Status)
		}
	})
}

func TestDeletePatch_TwiceIsANoOp(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	path := "/workspaces/carry/patches/" + ps["B"].ID
	if r := h.remove(t, "carry", ps["B"].ID, ignoredBody); r.status != http.StatusNoContent {
		t.Fatalf("DELETE B = %d %s, want 204", r.status, r.raw)
	}
	before, n := h.get(t, path), len(h.patchEvents(t, "carry", "parley.patch.delete"))
	h.advance(time.Hour)
	if r := h.remove(t, "carry", ps["B"].ID, nil); r.status != http.StatusNoContent || len(r.raw) != 0 {
		t.Errorf("DELETE of a deleted patch = %d %q, want 204 with an empty body", r.status, r.raw)
	}
	if got := h.get(t, path); !reflect.DeepEqual(got.patch, before.patch) {
		t.Errorf("DELETE of a deleted patch changed it to %s, want %s", got.raw, before.raw)
	}
	if got := len(h.patchEvents(t, "carry", "parley.patch.delete")); got != n {
		t.Errorf("DELETE of a deleted patch wrote %d parley.patch.delete events, want none", got-n)
	}
	if got := h.order(t, "carry"); !slices.Equal(got, []string{"patch/a", "patch/c"}) {
		t.Errorf("the order is %v, want [patch/a patch/c]", got)
	}
}

func TestRestorePatch_AppendsAsActive(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ways := []struct {
		name   string
		delete func(t *testing.T, slug string, ps map[string]carrypatch.Patch)
	}{
		{"deleted by a caller", func(t *testing.T, slug string, ps map[string]carrypatch.Patch) {
			if r := h.remove(t, slug, ps["A"].ID, nil); r.status != http.StatusNoContent {
				t.Fatalf("DELETE A = %d %s, want 204", r.status, r.raw)
			}
		}},
		{"removed by a rebuild as merged upstream", func(t *testing.T, slug string, ps map[string]carrypatch.Patch) {
			// A rebuild leaves the list gap-free and may leave the files of a
			// conflict behind.
			deleted := at(10, 0)
			h.alter(t, slug, ps["A"].ID, func(p *carrypatch.Patch) {
				p.Status, p.Position, p.DeletedAt, p.ConflictFiles = carrypatch.StatusDeleted, nil, &deleted, []string{"a.go"}
			})
			h.alter(t, slug, ps["B"].ID, func(p *carrypatch.Patch) { p.Position = new(1) })
			h.alter(t, slug, ps["C"].ID, func(p *carrypatch.Patch) { p.Position = new(2) })
		}},
	}
	for i, w := range ways {
		// Serial: each row advances the hub's clock and reads its stamp.
		t.Run(w.name, func(t *testing.T) {
			slug := fmt.Sprintf("restore-%d", i)
			ps := h.carryList(t, slug, workspace.StatusReady, workspace.MarkerNone)
			w.delete(t, slug, ps)
			h.advance(time.Minute)
			r := h.restore(t, slug, ps["A"].ID, nil)
			if r.status != http.StatusOK || r.patch["status"] != "active" || r.patch["position"] != float64(3) ||
				r.patch["deleted_at"] != nil || !reflect.DeepEqual(r.patch["conflict_files"], []any{}) || r.patch["updated_at"] != h.stamp() {
				t.Errorf("restore = %d %s, want 200 with status active, position 3, deleted_at null, conflict_files [] and updated_at %s",
					r.status, r.raw, h.stamp())
			}
			if got := h.order(t, slug); !slices.Equal(got, []string{"patch/b", "patch/c", "patch/a"}) {
				t.Errorf("the order after the restore is %v, want [patch/b patch/c patch/a]", got)
			}
		})
	}
	t.Run("the only patch", func(t *testing.T) {
		h.add(t, "alone", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
		x := deletedPatch("alone", "patch/x", at(10, 0))
		h.put(t, x)
		r := h.restore(t, "alone", x.ID, ignoredBody)
		if r.status != http.StatusOK || r.patch["status"] != "active" || r.patch["position"] != float64(1) {
			t.Errorf("restore of the only patch = %d %s, want 200 with status active at position 1", r.status, r.raw)
		}
	})
}

func TestRestorePatch_EmitsOneEvent(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	if r := h.remove(t, "carry", ps["A"].ID, nil); r.status != http.StatusNoContent {
		t.Fatalf("DELETE A = %d %s, want 204", r.status, r.raw)
	}
	if r := h.restore(t, "carry", ps["A"].ID, nil); r.status != http.StatusOK {
		t.Fatalf("restore A = %d %s, want 200", r.status, r.raw)
	}
	e := h.patchEvents(t, "carry", "parley.patch.restore")
	if len(e) != 1 {
		t.Fatalf("restore wrote %d parley.patch.restore events, want 1", len(e))
	}
	if got := e[0]; got.ResourceType != "patch" || got.ResourceID != ps["A"].ID || got.ActorID == nil || *got.ActorID != h.owner ||
		!reflect.DeepEqual(got.Metadata, map[string]any{"branch_name": "patch/a", "position": float64(3)}) {
		t.Errorf("the event names %s %s by %s with %v; want patch %s by %s with branch_name patch/a and position 3",
			got.ResourceType, got.ResourceID, str(got.ActorID), got.Metadata, ps["A"].ID, h.owner)
	}
	if n := h.rebuilds.calls(); n != 0 {
		t.Errorf("restore asked for %d rebuilds, want none", n)
	}

	t.Run("a failed event write keeps the patch deleted", func(t *testing.T) {
		if r := h.remove(t, "carry", ps["B"].ID, nil); r.status != http.StatusNoContent {
			t.Fatalf("DELETE B = %d %s, want 204", r.status, r.raw)
		}
		before := h.patches(t, "carry")
		h.failEvents.Store(true)
		defer h.failEvents.Store(false)
		if r := h.restore(t, "carry", ps["B"].ID, nil); r.status != http.StatusInternalServerError {
			t.Errorf("restore B with a failing event log = %d %s, want 500", r.status, r.raw)
		}
		if after := h.patches(t, "carry"); !reflect.DeepEqual(after, before) {
			t.Errorf("a failed restore changed the patches:\n got %+v\nwant %+v", after, before)
		}
		if b := h.stored(t, "carry", ps["B"].ID); b.Status != carrypatch.StatusDeleted {
			t.Errorf("after a failed restore B is %s, want deleted", b.Status)
		}
	})
}

func TestRestorePatch_RefusesNonDeleted(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	for _, status := range []string{carrypatch.StatusActive, carrypatch.StatusDisabled, carrypatch.StatusConflict, carrypatch.StatusMergedUpstream} {
		t.Run(status, func(t *testing.T) {
			h.alter(t, "carry", ps["B"].ID, func(p *carrypatch.Patch) { p.Status = status })
			before := h.patches(t, "carry")
			r := h.restore(t, "carry", ps["B"].ID, nil)
			if r.status != http.StatusConflict || r.Error.ErrorType != "patch_not_deleted" {
				t.Errorf("restore of a patch in status %s = %d %s, want 409 patch_not_deleted", status, r.status, r.raw)
			}
			if after := h.patches(t, "carry"); !reflect.DeepEqual(after, before) {
				t.Errorf("a refused restore changed the patches:\n got %+v\nwant %+v", after, before)
			}
		})
	}
	if e := h.patchEvents(t, "carry", "parley.patch.restore"); len(e) != 0 {
		t.Errorf("refused restores wrote %d parley.patch.restore events, want none", len(e))
	}
}

func TestRestorePatch_RefusesWhenBranchIsRegisteredAgain(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	x := deletedPatch("carry", "patch/a", at(10, 0))
	h.put(t, x)
	before := h.patches(t, "carry")
	r := h.restore(t, "carry", x.ID, nil)
	if r.status != http.StatusConflict || r.Error.ErrorType != "patch_exists" {
		t.Errorf("restore of a branch registered again = %d %s, want 409 patch_exists", r.status, r.raw)
	}
	if after := h.patches(t, "carry"); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused restore changed the patches:\n got %+v\nwant %+v", after, before)
	}
	if e := h.patchEvents(t, "carry", "parley.patch.restore"); len(e) != 0 {
		t.Errorf("a refused restore wrote %d parley.patch.restore events, want none", len(e))
	}
}
