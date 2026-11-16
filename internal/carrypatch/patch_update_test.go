package carrypatch_test

import (
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// patchMembers are the members of a patch object without a backup ref.
var patchMembers = []string{"added_at", "branch_name", "conflict_files", "deleted_at", "description", "id", "origin_sha",
	"origin_sync_state", "origin_synced_at", "position", "status", "updated_at", "upstream_pr_url", "workspace_slug"}

// stamp is the hub's clock as an answer shows it, cut to the second.
func (h *patchHub) stamp() string { return h.clock().Truncate(time.Second).Format(time.RFC3339) }

func TestUpdatePatch_TextFields(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	const url = "https://example.test/pull/7"
	cases := []struct {
		body                    any
		description, upstreamPR any // nil for null
		changes                 bool
	}{
		{map[string]any{"description": "net fix"}, "net fix", nil, true},
		{map[string]any{"upstream_pr_url": url}, "net fix", url, true},
		{map[string]any{"description": nil}, "net fix", url, false},
		{map[string]any{"description": ""}, nil, url, true},
		{map[string]any{"upstream_pr_url": ""}, nil, nil, true},
		{map[string]any{"branch_name": "x", "colour": "red", "description": "d"}, "d", nil, true},
	}
	updated := h.get(t, "/workspaces/carry/patches/"+ps["A"].ID).patch["updated_at"]
	for _, c := range cases {
		h.advance(time.Minute)
		a := h.update(t, "carry", ps["A"].ID, c.body)
		if a.status != http.StatusOK {
			t.Fatalf("PATCH %v = %d %s, want 200", c.body, a.status, a.raw)
		}
		if got := slices.Sorted(maps.Keys(a.patch)); !slices.Equal(got, patchMembers) {
			t.Errorf("PATCH %v answered the members %v, want %v", c.body, got, patchMembers)
		}
		if a.patch["description"] != c.description || a.patch["upstream_pr_url"] != c.upstreamPR || a.patch["branch_name"] != "patch/a" {
			t.Errorf("PATCH %v = description %v, upstream_pr_url %v, branch_name %v; want %v, %v, patch/a",
				c.body, a.patch["description"], a.patch["upstream_pr_url"], a.patch["branch_name"], c.description, c.upstreamPR)
		}
		if c.changes {
			updated = h.stamp()
		}
		if a.patch["updated_at"] != updated {
			t.Errorf("PATCH %v: updated_at = %v, want %v", c.body, a.patch["updated_at"], updated)
		}
		if got := h.get(t, "/workspaces/carry/patches/"+ps["A"].ID); !reflect.DeepEqual(got.patch, a.patch) {
			t.Errorf("PATCH %v answered %s, but GET answers %s", c.body, a.raw, got.raw)
		}
	}
}

func TestUpdatePatch_NoChange(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	h.alter(t, "carry", ps["B"].ID, func(p *carrypatch.Patch) { p.Description = new("d") })
	path := "/workspaces/carry/patches/" + ps["B"].ID
	for _, body := range []any{
		map[string]any{},
		map[string]any{"status": nil, "position": nil},
		map[string]any{"status": "active"},
		map[string]any{"position": 2},
		map[string]any{"description": "d"},
		map[string]any{"branch_name": "other"},
	} {
		before := h.get(t, path)
		h.advance(time.Minute)
		a := h.update(t, "carry", ps["B"].ID, body)
		if a.status != http.StatusOK || !reflect.DeepEqual(a.patch, before.patch) {
			t.Errorf("PATCH %v = %d %s, want 200 with the patch as it was: %s", body, a.status, a.raw, before.raw)
		}
	}
	if e := h.patchEvents(t, "carry", "parley.patch.update"); len(e) != 0 {
		t.Errorf("updates that change nothing wrote %d parley.patch.update events, want none", len(e))
	}
}

func TestUpdatePatch_Status(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	cases := []struct {
		from      string
		files     []string
		to        string
		wantFiles []any
	}{
		{carrypatch.StatusActive, nil, carrypatch.StatusDisabled, []any{}},
		{carrypatch.StatusDisabled, nil, carrypatch.StatusActive, []any{}},
		{carrypatch.StatusActive, nil, carrypatch.StatusMergedUpstream, []any{}},
		{carrypatch.StatusMergedUpstream, nil, carrypatch.StatusActive, []any{}},
		{carrypatch.StatusActive, nil, carrypatch.StatusConflict, []any{}},
		{carrypatch.StatusConflict, []string{"a.go"}, carrypatch.StatusActive, []any{}},
		{carrypatch.StatusConflict, []string{"a.go"}, carrypatch.StatusDisabled, []any{}},
		{carrypatch.StatusConflict, []string{"a.go"}, carrypatch.StatusMergedUpstream, []any{}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s %v to %s", c.from, c.files, c.to), func(t *testing.T) {
			h.alter(t, "carry", ps["A"].ID, func(p *carrypatch.Patch) { p.Status, p.ConflictFiles = c.from, c.files })
			a := h.update(t, "carry", ps["A"].ID, map[string]any{"status": c.to})
			if a.status != http.StatusOK || a.patch["status"] != c.to || !reflect.DeepEqual(a.patch["conflict_files"], c.wantFiles) ||
				a.patch["position"] != float64(1) {
				t.Errorf("PATCH status %s = %d %s, want 200 with status %s, conflict_files %v and position 1",
					c.to, a.status, a.raw, c.to, c.wantFiles)
			}
			if got := h.stored(t, "carry", ps["A"].ID); got.Status != c.to || len(got.ConflictFiles) != len(c.wantFiles) {
				t.Errorf("the stored patch has status %s and conflict_files %v, want %s and %v", got.Status, got.ConflictFiles, c.to, c.wantFiles)
			}
		})
	}
}

func TestUpdatePatch_Position(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	// list stores the workspace slug with the patches A, B, C and D at 1 to
	// 4 and the deleted patch X, and returns them by letter.
	list := func(slug string) map[string]carrypatch.Patch {
		ps := h.carryList(t, slug, workspace.StatusReady, workspace.MarkerNone)
		ps["D"], ps["X"] = livePatch(slug, "patch/d", 4), deletedPatch(slug, "patch/x", at(10, 0))
		h.put(t, ps["D"], ps["X"])
		return ps
	}
	cases := []struct {
		move string
		to   int
		want []string
	}{
		{"A", 3, []string{"patch/b", "patch/c", "patch/a", "patch/d"}},
		{"D", 1, []string{"patch/d", "patch/a", "patch/b", "patch/c"}},
		{"B", 4, []string{"patch/a", "patch/c", "patch/d", "patch/b"}},
		{"C", 2, []string{"patch/a", "patch/c", "patch/b", "patch/d"}},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("%s to %d", c.move, c.to), func(t *testing.T) {
			slug := fmt.Sprintf("move-%d", i)
			ps := list(slug)
			h.advance(time.Minute)
			a := h.update(t, slug, ps[c.move].ID, map[string]any{"position": c.to})
			if a.status != http.StatusOK || a.patch["position"] != float64(c.to) {
				t.Fatalf("PATCH position %d = %d %s, want 200 at position %d", c.to, a.status, a.raw, c.to)
			}
			if got := h.order(t, slug); !slices.Equal(got, c.want) {
				t.Errorf("the order after moving %s to %d is %v, want %v", c.move, c.to, got, c.want)
			}
			if x := h.stored(t, slug, ps["X"].ID); x.Position != nil {
				t.Errorf("the deleted patch X is at %d, want no position", *x.Position)
			}
			// Only a patch whose position changed is stamped.
			for _, letter := range []string{"A", "B", "C", "D"} {
				got := h.stored(t, slug, ps[letter].ID)
				moved := *got.Position != *ps[letter].Position
				if stamped := !got.UpdatedAt.Equal(ps[letter].UpdatedAt); stamped != moved {
					t.Errorf("patch %s went from %d to %d with updated_at %v; want it stamped only when it moved",
						letter, *ps[letter].Position, *got.Position, got.UpdatedAt)
				}
			}
		})
	}
	t.Run("a failed commit keeps the order", func(t *testing.T) {
		ps := list("move-failed")
		h.failEvents.Store(true)
		defer h.failEvents.Store(false)
		a := h.update(t, "move-failed", ps["A"].ID, map[string]any{"position": 3})
		if a.status != http.StatusInternalServerError {
			t.Errorf("PATCH position 3 with a failing event log = %d %s, want 500", a.status, a.raw)
		}
		if got := h.order(t, "move-failed"); !slices.Equal(got, []string{"patch/a", "patch/b", "patch/c", "patch/d"}) {
			t.Errorf("the order after a failed move is %v, want the one before", got)
		}
	})
}

func TestUpdatePatch_RejectsInvalidBody(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	before := h.patches(t, "carry")
	for _, body := range []any{
		map[string]any{"status": "deleted"},
		map[string]any{"status": "paused"},
		map[string]any{"status": ""},
		map[string]any{"status": 5},
		map[string]any{"position": 0},
		map[string]any{"position": -1},
		map[string]any{"position": 4},
		`{"position": 1.5}`,
		map[string]any{"position": "2"},
		map[string]any{"description": 7},
		map[string]any{"upstream_pr_url": []any{}},
		"[1]",
		"{bad",
		map[string]any{"status": "disabled", "position": 9},
	} {
		a := h.update(t, "carry", ps["A"].ID, body)
		if a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" {
			t.Errorf("PATCH %v = %d %s, want 400 invalid_request", body, a.status, a.raw)
		}
	}
	if after := h.patches(t, "carry"); !reflect.DeepEqual(after, before) {
		t.Errorf("rejected updates changed the patches:\n got %+v\nwant %+v", after, before)
	}
	if e := h.patchEvents(t, "carry", "parley.patch.update"); len(e) != 0 {
		t.Errorf("rejected updates wrote %d parley.patch.update events, want none", len(e))
	}
}

func TestUpdatePatch_RefusesDeletedPatch(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	x := deletedPatch("carry", "patch/x", at(10, 0))
	h.put(t, x)
	path := "/workspaces/carry/patches/" + x.ID
	before := h.get(t, path)
	for _, body := range []any{
		map[string]any{},
		map[string]any{"description": "d"},
		map[string]any{"status": "active"},
		map[string]any{"position": 1},
		map[string]any{"position": 99},
	} {
		a := h.update(t, "carry", x.ID, body)
		if a.status != http.StatusConflict || a.Error.ErrorType != "patch_deleted" {
			t.Errorf("PATCH %v on a deleted patch = %d %s, want 409 patch_deleted", body, a.status, a.raw)
		}
		if got := h.get(t, path); !reflect.DeepEqual(got.patch, before.patch) {
			t.Errorf("PATCH %v changed the deleted patch to %s, want %s", body, got.raw, before.raw)
		}
	}
	if a := h.update(t, "carry", x.ID, map[string]any{"status": "deleted"}); a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" {
		t.Errorf("PATCH status deleted on a deleted patch = %d %s, want 400 invalid_request", a.status, a.raw)
	}
}

func TestUpdatePatch_EmitsOneEvent(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	cases := []struct {
		body     map[string]any
		changed  []any
		from, to any // nil when the status does not change
	}{
		{map[string]any{"description": "d"}, []any{"description"}, nil, nil},
		{map[string]any{"status": "disabled"}, []any{"status"}, "active", "disabled"},
		{map[string]any{"position": 3}, []any{"position"}, nil, nil},
		{map[string]any{"status": "disabled", "position": 2, "upstream_pr_url": "u", "description": "same as stored"},
			[]any{"status", "position", "upstream_pr_url"}, "active", "disabled"},
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(c.body), func(t *testing.T) {
			slug := fmt.Sprintf("events-%d", i)
			ps := h.carryList(t, slug, workspace.StatusReady, workspace.MarkerNone)
			h.alter(t, slug, ps["A"].ID, func(p *carrypatch.Patch) { p.Description = new("same as stored") })
			if a := h.update(t, slug, ps["A"].ID, c.body); a.status != http.StatusOK {
				t.Fatalf("PATCH %v = %d %s, want 200", c.body, a.status, a.raw)
			}
			e := h.patchEvents(t, slug, "parley.patch.update")
			if len(e) != 1 {
				t.Fatalf("PATCH %v wrote %d parley.patch.update events, want 1", c.body, len(e))
			}
			got := e[0]
			if got.ResourceType != "patch" || got.ResourceID != ps["A"].ID || got.ActorID == nil || *got.ActorID != h.owner {
				t.Errorf("the event names %s %s by %v, want patch %s by %s", got.ResourceType, got.ResourceID, str(got.ActorID), ps["A"].ID, h.owner)
			}
			if got.Metadata["branch_name"] != "patch/a" || !reflect.DeepEqual(got.Metadata["changed"], c.changed) {
				t.Errorf("the event's metadata is %v, want branch_name patch/a and changed %v", got.Metadata, c.changed)
			}
			_, hasReason := got.Metadata["reason"]
			if got.Metadata["from"] != c.from || got.Metadata["to"] != c.to || hasReason != (c.from != nil) ||
				(hasReason && got.Metadata["reason"] != "api") {
				t.Errorf("the event's metadata is %v, want from %v, to %v and reason api only for a status change", got.Metadata, c.from, c.to)
			}
			if n := h.rebuilds.calls(); n != 0 {
				t.Errorf("an update asked for %d rebuilds, want none", n)
			}
		})
	}
	t.Run("a failed event write keeps the patch", func(t *testing.T) {
		ps := h.carryList(t, "events-failed", workspace.StatusReady, workspace.MarkerNone)
		path := "/workspaces/events-failed/patches/" + ps["A"].ID
		before := h.get(t, path)
		h.failEvents.Store(true)
		defer h.failEvents.Store(false)
		if a := h.update(t, "events-failed", ps["A"].ID, map[string]any{"description": "d", "status": "disabled"}); a.status != http.StatusInternalServerError {
			t.Errorf("PATCH with a failing event log = %d %s, want 500", a.status, a.raw)
		}
		if got := h.get(t, path); !reflect.DeepEqual(got.patch, before.patch) {
			t.Errorf("a failed update left the patch at %s, want %s", got.raw, before.raw)
		}
	})
}
