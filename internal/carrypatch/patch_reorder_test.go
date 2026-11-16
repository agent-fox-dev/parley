package carrypatch_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// idsIn are the ids of the patches ps names by letter, in the order of
// letters.
func idsIn(ps map[string]carrypatch.Patch, letters ...string) []string {
	ids := make([]string, len(letters))
	for i, l := range letters {
		ids[i] = ps[l].ID
	}
	return ids
}

// nextCursorIsNull reports whether the answer has the member next_cursor,
// set to null.
func nextCursorIsNull(a answer) bool {
	var members map[string]json.RawMessage
	return json.Unmarshal(a.raw, &members) == nil && string(members["next_cursor"]) == "null"
}

func TestReorderPatches(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	orders := []struct {
		name    string
		letters []string
	}{
		{"C,A,B", []string{"C", "A", "B"}},
		{"B,A,C", []string{"B", "A", "C"}},
		{"A,B,C, the current order", []string{"A", "B", "C"}},
		{"C,B,A", []string{"C", "B", "A"}},
	}
	for i, o := range orders {
		t.Run(o.name, func(t *testing.T) {
			slug := fmt.Sprintf("reorder-%d", i)
			ps := h.carryList(t, slug, workspace.StatusReady, workspace.MarkerNone)
			x := deletedPatch(slug, "patch/x", at(10, 0))
			h.put(t, x)
			h.advance(time.Minute)
			ids := idsIn(ps, o.letters...)

			r := h.reorder(t, slug, map[string]any{"patch_ids": ids})
			if r.status != http.StatusOK || !nextCursorIsNull(r) {
				t.Fatalf("reorder = %d %s, want 200 with next_cursor null", r.status, r.raw)
			}
			var got []string
			for j, it := range r.Items {
				got = append(got, fmt.Sprint(it["id"]))
				if it["position"] != float64(j+1) {
					t.Errorf("item %d %v is at %v, want %d", j, it["branch_name"], it["position"], j+1)
				}
			}
			if !reflect.DeepEqual(got, ids) {
				t.Errorf("the answer lists %v, want %v", got, ids)
			}
			if g := h.get(t, "/workspaces/"+slug+"/patches"); !reflect.DeepEqual(g.Items, r.Items) {
				t.Errorf("GET the list = %s, want the reorder's items %s", g.raw, r.raw)
			}
			if p := h.stored(t, slug, x.ID); p.Position != nil {
				t.Errorf("the deleted patch is at %d, want no position", *p.Position)
			}
			for j, l := range o.letters {
				p, want := h.stored(t, slug, ps[l].ID), ps[l].UpdatedAt
				if *ps[l].Position != j+1 {
					want = h.clock().Truncate(time.Second)
				}
				if !p.UpdatedAt.Equal(want) {
					t.Errorf("%s moved from %d to %d and has updated_at %s, want %s", l, *ps[l].Position, j+1, p.UpdatedAt, want)
				}
			}
		})
	}
	t.Run("a workspace with no patch that is not deleted", func(t *testing.T) {
		h.add(t, "empty", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
		h.put(t, deletedPatch("empty", "patch/x", at(10, 0)))
		r := h.reorder(t, "empty", map[string]any{"patch_ids": []string{}})
		if r.status != http.StatusOK || r.Items == nil || len(r.Items) != 0 || !nextCursorIsNull(r) {
			t.Errorf("reorder of an empty list = %d %s, want 200 with items [] and next_cursor null", r.status, r.raw)
		}
		if e := h.patchEvents(t, "empty", ""); len(e) != 0 {
			t.Errorf("reorder of an empty list wrote %d parley.patch.* events, want none", len(e))
		}
	})
	t.Run("a failed event write keeps the order", func(t *testing.T) {
		ps := h.carryList(t, "failing", workspace.StatusReady, workspace.MarkerNone)
		before := h.patches(t, "failing")
		h.failEvents.Store(true)
		defer h.failEvents.Store(false)
		if r := h.reorder(t, "failing", map[string]any{"patch_ids": idsIn(ps, "C", "A", "B")}); r.status != http.StatusInternalServerError {
			t.Errorf("reorder with a failing event log = %d %s, want 500", r.status, r.raw)
		}
		if after := h.patches(t, "failing"); !reflect.DeepEqual(after, before) {
			t.Errorf("a failed reorder changed the patches:\n got %+v\nwant %+v", after, before)
		}
	})
}

func TestReorderPatches_EmitsOneEvent(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	for i, letters := range [][]string{{"C", "A", "B"}, {"A", "B", "C"}} {
		t.Run(strings.Join(letters, ","), func(t *testing.T) {
			slug := fmt.Sprintf("emit-%d", i)
			ps := h.carryList(t, slug, workspace.StatusReady, workspace.MarkerNone)
			ids := idsIn(ps, letters...)
			if r := h.reorder(t, slug, map[string]any{"patch_ids": ids}); r.status != http.StatusOK {
				t.Fatalf("reorder = %d %s, want 200", r.status, r.raw)
			}
			e := h.patchEvents(t, slug, carrypatch.EventPatchReorder)
			if len(e) != 1 {
				t.Fatalf("reorder wrote %d parley.patch.reorder events, want 1", len(e))
			}
			want := map[string]any{"patch_ids": []any{ids[0], ids[1], ids[2]}}
			if got := e[0]; got.ResourceType != "patch" || got.ResourceID != ids[0] || got.ActorID == nil || *got.ActorID != h.owner ||
				!reflect.DeepEqual(got.Metadata, want) {
				t.Errorf("the event names %s %s by %s with %v; want patch %s by %s with %v",
					got.ResourceType, got.ResourceID, str(got.ActorID), got.Metadata, ids[0], h.owner, want)
			}
			if u := h.patchEvents(t, slug, carrypatch.EventPatchUpdate); len(u) != 0 {
				t.Errorf("reorder wrote %d parley.patch.update events, want none", len(u))
			}
			if n := h.rebuilds.calls(); n != 0 {
				t.Errorf("reorder asked for %d rebuilds, want none", n)
			}
		})
	}
}

func TestReorderPatches_RejectsIncompleteOrWrongLists(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	x := deletedPatch("carry", "patch/x", at(10, 0))
	h.put(t, x)
	a, b, c, unknown := ps["A"].ID, ps["B"].ID, ps["C"].ID, uuid.New().String()
	list := func(ids ...any) map[string]any { return map[string]any{"patch_ids": append([]any{}, ids...)} }
	rows := []struct {
		name     string
		body     any
		contains []string
	}{
		{"an id given twice", list(a, b, b), []string{b}},
		{"an id given twice in a list too long", list(a, b, c, a), []string{a}},
		{"an id of no patch", list(a, b, unknown), []string{unknown}},
		{"the id of a deleted patch", list(a, b, c, x.ID), []string{x.ID}},
		{"a patch missing", list(a, b), []string{"2", "3"}},
		{"no ids", list(), []string{"0", "3"}},
		{"no patch_ids", map[string]any{}, nil},
		{"patch_ids null", map[string]any{"patch_ids": nil}, nil},
		{"patch_ids a string", map[string]any{"patch_ids": "A"}, nil},
		{"patch_ids of numbers", list(1, 2, 3), nil},
		{"patch_ids holding null", list(a, nil, c), nil},
		{"a body that is not JSON", "{bad", nil},
	}
	before := h.patches(t, "carry")
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			r := h.reorder(t, "carry", row.body)
			if r.status != http.StatusBadRequest || r.Error.ErrorType != "invalid_request" {
				t.Errorf("reorder = %d %s, want 400 invalid_request", r.status, r.raw)
			}
			for _, s := range row.contains {
				if !strings.Contains(r.Error.Message, s) {
					t.Errorf("the message %q does not name %s", r.Error.Message, s)
				}
			}
			if after := h.patches(t, "carry"); !reflect.DeepEqual(after, before) {
				t.Errorf("a refused reorder changed the patches:\n got %+v\nwant %+v", after, before)
			}
			if e := h.patchEvents(t, "carry", carrypatch.EventPatchReorder); len(e) != 0 {
				t.Errorf("a refused reorder wrote %d parley.patch.reorder events, want none", len(e))
			}
		})
	}
}
