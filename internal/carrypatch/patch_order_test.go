package carrypatch_test

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
)

// unknownPatch is an id no patch has.
const unknownPatch = "00000000-0000-4000-8000-000000000000"

// gapFree fails the test when the patches ps of a workspace are not the
// patches ids, or when those that are not deleted do not hold the
// positions 1..N or a deleted one has a position or no deleted_at.
func gapFree(t *testing.T, ps []carrypatch.Patch, ids []string, after string) {
	t.Helper()
	var got []string
	live := 0
	for _, p := range ps {
		got = append(got, p.ID)
		switch {
		case p.Status == carrypatch.StatusDeleted && (p.Position != nil || p.DeletedAt == nil):
			t.Fatalf("after %s the deleted patch %s has position %v and deleted_at %v, want none and a time", after, p.BranchName, p.Position, p.DeletedAt)
		case p.Status == carrypatch.StatusDeleted:
		case p.Position == nil || *p.Position != live+1 || p.DeletedAt != nil:
			t.Fatalf("after %s the patch %s has position %v and deleted_at %v, want %d and none", after, p.BranchName, p.Position, p.DeletedAt, live+1)
		default:
			live++
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, ids) {
		t.Fatalf("after %s the patches are %v, want %v", after, got, ids)
	}
}

// liveIDs are the ids of the patches of ps that are not deleted, in order,
// and of the deleted ones.
func liveIDs(ps []carrypatch.Patch) (live, deleted []string) {
	for _, p := range ps {
		if p.Status == carrypatch.StatusDeleted {
			deleted = append(deleted, p.ID)
		} else {
			live = append(live, p.ID)
		}
	}
	return live, deleted
}

// wrongList is a reorder list that the list live, deleted beside it,
// refuses: one with a patch missing, one given twice, a deleted one or one
// of no patch added.
func wrongList(rng *rand.Rand, live, deleted []string) []string {
	ids := slices.Clone(live)
	rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	switch k := rng.IntN(4); {
	case k == 0 && len(ids) > 0:
		return ids[1:]
	case k == 1 && len(ids) > 0:
		return append(ids, ids[rng.IntN(len(ids))])
	case k == 2 && len(deleted) > 0:
		return append(ids, deleted[rng.IntN(len(deleted))])
	default:
		return append(ids, unknownPatch)
	}
}

func TestPatchOrder_StaysGapFree(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	const seeds, steps, patches = 200, 30, 6
	for seed := range uint64(seeds) {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			slug := fmt.Sprintf("order-%d", seed)
			all := h.carryOf(t, slug, patches)
			ids, _ := liveIDs(all)
			slices.Sort(ids)
			rng := rand.New(rand.NewPCG(seed, seed))
			ps := all
			for step := range steps {
				live, deleted := liveIDs(ps)
				id := all[rng.IntN(len(all))].ID
				var op string
				var r answer
				var want []string
				switch rng.IntN(5) {
				case 0:
					pos := rng.IntN(len(live) + 2)
					op = fmt.Sprintf("moving %s to %d", id, pos)
					r = h.update(t, slug, id, map[string]any{"position": pos})
				case 1:
					op = "deleting " + id
					r = h.remove(t, slug, id, nil)
				case 2:
					op = "restoring " + id
					r = h.restore(t, slug, id, nil)
				case 3:
					order := append([]string{}, live...)
					rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
					op = fmt.Sprintf("reordering to %v", order)
					if r = h.reorder(t, slug, map[string]any{"patch_ids": order}); r.status != http.StatusOK {
						t.Fatalf("step %d: %s = %d %s, want 200", step, op, r.status, r.raw)
					}
					want = order
				default:
					order := wrongList(rng, live, deleted)
					op = fmt.Sprintf("reordering to the wrong list %v", order)
					if r = h.reorder(t, slug, map[string]any{"patch_ids": order}); r.status != http.StatusBadRequest {
						t.Fatalf("step %d: %s = %d %s, want 400", step, op, r.status, r.raw)
					}
				}
				if r.status < 200 || r.status >= 500 {
					t.Fatalf("step %d: %s = %d %s, want a 2xx or 4xx answer", step, op, r.status, r.raw)
				}
				ps = h.patches(t, slug)
				gapFree(t, ps, ids, fmt.Sprintf("step %d, %s", step, op))
				if got, _ := liveIDs(ps); want != nil && !slices.Equal(got, want) {
					t.Fatalf("step %d: after %s the order is %v", step, op, got)
				}
			}
		})
	}
}

func TestPatchOrder_ConcurrentWritesSerialize(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	// A request that changes positions.
	type request struct {
		method, path string
		body         any
		status       int
	}
	move := func(slug string, p carrypatch.Patch, pos, n int) request {
		// The description is new on every move, so that each one changes
		// the patch and writes its event, whatever the position it finds.
		return request{http.MethodPatch, "/workspaces/" + slug + "/patches/" + p.ID,
			map[string]any{"position": pos, "description": fmt.Sprintf("move %d", n)}, http.StatusOK}
	}
	reorder := func(slug string, ps []carrypatch.Patch) request {
		ids, _ := liveIDs(ps)
		slices.Reverse(ids)
		return request{http.MethodPost, "/workspaces/" + slug + "/patches/reorder", map[string]any{"patch_ids": ids}, http.StatusOK}
	}
	deleteThenRestore := func(slug string, p carrypatch.Patch) []request {
		path := "/workspaces/" + slug + "/patches/" + p.ID
		return []request{{http.MethodDelete, path, nil, http.StatusNoContent}, {http.MethodPost, path + "/restore", nil, http.StatusOK}}
	}
	// A reorder names every patch that is not deleted, so a delete running
	// beside it would make it a wrong list rather than a valid one; the
	// mixes keep each request valid whatever runs first.
	mixes := []struct {
		name string
		// goroutines are the requests each goroutine sends, in turn, on a
		// list of 8 patches.
		goroutines func(slug string, ps []carrypatch.Patch) [][]request
	}{
		{"moves and reorders", func(slug string, ps []carrypatch.Patch) [][]request {
			var g [][]request
			for i := range 16 {
				if i%2 == 0 {
					g = append(g, []request{move(slug, ps[i/2], 8-i/2, i)})
				} else {
					g = append(g, []request{reorder(slug, ps)})
				}
			}
			return g
		}},
		{"moves, deletes and restores", func(slug string, ps []carrypatch.Patch) [][]request {
			// Patches 5 to 8 are deleted and restored; the moves take patches
			// 1 to 4 to positions 1 to 4, which the list always has.
			var g [][]request
			for i := range 4 {
				g = append(g, deleteThenRestore(slug, ps[4+i]))
			}
			for i := range 12 {
				g = append(g, []request{move(slug, ps[i%4], 4-i%4, i)})
			}
			return g
		}},
	}
	for i, m := range mixes {
		t.Run(m.name, func(t *testing.T) {
			slug := fmt.Sprintf("concurrent-%d", i)
			ps := h.carryOf(t, slug, 8)
			goroutines := m.goroutines(slug, ps)
			start := make(chan struct{})
			var wg sync.WaitGroup
			sent := 0
			for _, reqs := range goroutines {
				sent += len(reqs)
				wg.Go(func() {
					<-start
					for _, rq := range reqs {
						if r := h.send(t, h.key, rq.method, rq.path, rq.body); r.status != rq.status {
							t.Errorf("%s %s = %d %s, want %d", rq.method, rq.path, r.status, r.raw, rq.status)
						}
					}
				})
			}
			close(start)
			wg.Wait()
			ids, _ := liveIDs(ps)
			slices.Sort(ids)
			gapFree(t, h.patches(t, slug), ids, m.name)
			if live, deleted := liveIDs(h.patches(t, slug)); len(live) != 8 || len(deleted) != 0 {
				t.Errorf("after %s %d patches are not deleted and %d are, want 8 and none", m.name, len(live), len(deleted))
			}
			if e := h.patchEvents(t, slug, ""); len(e) != sent {
				t.Errorf("%d requests changed the list and wrote %d parley.patch.* events, want one each", sent, len(e))
			}
		})
	}
}
