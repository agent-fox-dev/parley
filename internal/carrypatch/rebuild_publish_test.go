package carrypatch_test

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"reflect"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/jobs"
)

func TestRebuild_PublishMovesOnlyTheIntegrationBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		exists bool
	}{
		{"deploy exists and is moved", true},
		{"deploy does not exist and is created", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			f.addPatch(t, "p1")
			f.addPatch(t, "p2")
			var previous any
			if tc.exists {
				h0 := gitrepotest.Commit(t, f.copy, "H0", map[string]string{"h0.txt": "h0\n"}, f.base)
				gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/deploy", h0)
				previous = h0
			} else {
				gitrepotest.Git(t, f.copy, "update-ref", "-d", "refs/heads/deploy")
			}
			before, err := visibleRefs(f.copy)
			if err != nil {
				t.Fatal(err)
			}

			out, id := f.attempt(t, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			res, results := f.result(t, id), f.patchResults(t, id)
			if len(results) != 2 {
				t.Fatalf("patch_results = %v, want two", results)
			}
			deploy := refIn(f.copy, "refs/heads/deploy")
			if last := results[1]["new_head_sha"]; deploy == "" || last != deploy || res["integration_head_sha"] != deploy {
				t.Errorf("deploy = %q, the last new_head_sha = %v, integration_head_sha = %v; want all three the run's head", deploy, last, res["integration_head_sha"])
			}
			if got := res["previous_integration_head_sha"]; got != previous {
				t.Errorf("previous_integration_head_sha = %v, want %v", got, previous)
			}
			after, err := visibleRefs(f.copy)
			if err != nil {
				t.Fatal(err)
			}
			delete(after, "refs/heads/deploy")
			delete(before, "refs/heads/deploy")
			if !maps.Equal(after, before) {
				t.Errorf("the refs other than deploy after the publish = %v, want them as before, %v", after, before)
			}
		})
	}
}

func TestRebuild_PublishesTheUpstreamBaseWhenNothingApplies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		statuses []string
	}{
		{"no patch at all", nil},
		{"one disabled and one merged upstream patch", []string{carrypatch.StatusDisabled, carrypatch.StatusMergedUpstream}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			for i, status := range tc.statuses {
				f.addPatch(t, fmt.Sprintf("p%d", i+1), withStatus(status))
			}
			base := f.moveUpstream(t, "U2", map[string]string{"u2.txt": "u2\n"})

			out, id := f.attempt(t, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			res := f.result(t, id)
			deploy := refIn(f.copy, "refs/heads/deploy")
			if deploy != base || res["integration_head_sha"] != base || res["upstream_head_sha"] != base {
				t.Errorf("deploy = %s, integration_head_sha = %v, upstream_head_sha = %v; want all three the upstream base %s",
					deploy, res["integration_head_sha"], res["upstream_head_sha"], base)
			}
		})
	}
}

func TestRebuild_LostCompareAndSwapIsRetryable(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	p1 := f.addPatch(t, "p1", withStatus(carrypatch.StatusMergedUpstream))
	p2 := f.addPatch(t, "p2", withStatus(carrypatch.StatusConflict, "p2.txt"))
	f.moveUpstream(t, "U2", map[string]string{"u2.txt": "u2\n"})
	w := gitrepotest.Commit(t, f.copy, "W", map[string]string{"w.txt": "w\n"}, f.base)
	before := map[string]carrypatch.Patch{p1.ID: f.stored(t, p1.ID), p2.ID: f.stored(t, p2.ID)}
	f.hooks.beforeSwap(func() {
		if _, err := gitrepotest.Run(f.copy, "", "update-ref", "refs/heads/deploy", w); err != nil {
			t.Errorf("move deploy to W: %v", err)
		}
	})

	out, id := f.attempt(t, nil)
	if out.Kind != attemptRetryable || out.Reason != "ref_moved" {
		t.Errorf("the attempt = %+v, want retryable with reason ref_moved", out)
	}
	if got := refIn(f.copy, "refs/heads/deploy"); got != w {
		t.Errorf("deploy = %s, want the other writer's W, %s", got, w)
	}
	for id, want := range before {
		if got := f.stored(t, id); !reflect.DeepEqual(got, want) {
			t.Errorf("%s after the attempt = %+v, want it as before, %+v", want.BranchName, got, want)
		}
	}
	if got := f.patchEvents(t, carrypatch.EventPatchDelete); len(got) != 0 {
		t.Errorf("parley.patch.delete events = %+v, want none", got)
	}
	if got := cleanUpdates(f.patchEvents(t, carrypatch.EventPatchUpdate)); len(got) != 0 {
		t.Errorf("parley.patch.update events with reason rebuild_clean = %+v, want none", got)
	}
	if got := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildComplete); len(got) != 0 {
		t.Errorf("parley.rebuild.complete events = %+v, want none", got)
	}
	if res := f.result(t, id); res["integration_head_sha"] != nil || res["previous_integration_head_sha"] != nil {
		t.Errorf("integration_head_sha = %v, previous_integration_head_sha = %v; want both null", res["integration_head_sha"], res["previous_integration_head_sha"])
	}
}

func TestRebuild_PublishUpdatesThePatchList(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	p1 := f.addPatch(t, "p1", withStatus(carrypatch.StatusMergedUpstream))
	p2 := f.addPatch(t, "p2", withStatus(carrypatch.StatusConflict, "p2.txt"))
	p3, _ := f.conflictingPatch(t, "p3", "a.txt")
	p4 := f.addPatch(t, "p4", withStatus(carrypatch.StatusMergedUpstream))
	p5 := f.addPatch(t, "p5")
	f.moveOn(t)
	ctx := context.Background()
	f.hooks.beforePublish(func() {
		// A caller finds p4 not merged after all while the run goes on.
		if err := f.Store.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE patches SET status = ? WHERE workspace_slug = ? AND id = ?`, carrypatch.StatusActive, f.slug, p4.ID)
			return err
		}); err != nil {
			t.Errorf("set p4 active: %v", err)
		}
	})
	// What a reader sees while the publish's transaction is open, before the
	// swap.
	var mu sync.Mutex
	var id string
	var atSwap struct {
		p1, p2 string
		head   any
	}
	f.hooks.beforeSwap(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range []struct {
			id  string
			got *string
		}{{p1.ID, &atSwap.p1}, {p2.ID, &atSwap.p2}} {
			p, _, err := carrypatch.ReadPatch(ctx, f.Store.Main(), f.slug, r.id)
			if err != nil {
				t.Errorf("read a patch at the swap: %v", err)
			}
			*r.got = p.Status
		}
		j, err := f.queue.Get(ctx, id)
		if err != nil {
			t.Errorf("read the job at the swap: %v", err)
		}
		atSwap.head = resultMember(j.Result, "integration_head_sha")
	})

	out, _ := f.attemptWith(t, continueRebase, func(_ context.Context, j jobs.Job) {
		mu.Lock()
		defer mu.Unlock()
		id = j.ID
	})
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if atSwap.p1 != carrypatch.StatusMergedUpstream || atSwap.p2 != carrypatch.StatusConflict || atSwap.head != nil {
		t.Errorf("at the swap a reader saw p1 %s, p2 %s and integration_head_sha %v; want merged_upstream, conflict and null",
			atSwap.p1, atSwap.p2, atSwap.head)
	}
	deploy := refIn(f.copy, "refs/heads/deploy")
	if got := f.result(t, id)["integration_head_sha"]; got != deploy {
		t.Errorf("integration_head_sha after the attempt = %v, want deploy %s", got, deploy)
	}

	got := f.stored(t, p1.ID)
	if got.Status != carrypatch.StatusDeleted || got.Position != nil || got.DeletedAt == nil || !got.DeletedAt.Equal(f.clock()) {
		t.Errorf("p1 has status %s, position %s, deleted_at %v; want deleted, none, %v", got.Status, position(got.Position), got.DeletedAt, f.clock())
	}
	if got := f.stored(t, p2.ID); got.Status != carrypatch.StatusActive || got.ConflictFiles == nil || len(got.ConflictFiles) != 0 {
		t.Errorf("p2 has status %s and conflict_files %q, want active and []", got.Status, got.ConflictFiles)
	}
	if got := f.stored(t, p3.ID); got.Status != carrypatch.StatusConflict {
		t.Errorf("p3 has status %s, want conflict", got.Status)
	}
	if got := f.stored(t, p4.ID); got.Status != carrypatch.StatusActive || got.DeletedAt != nil {
		t.Errorf("p4 has status %s, deleted_at %v; want active and not deleted", got.Status, got.DeletedAt)
	}
	want := map[string]int{p2.ID: 1, p3.ID: 2, p4.ID: 3, p5.ID: 4}
	for id, pos := range want {
		if got := f.stored(t, id); got.Position == nil || *got.Position != pos {
			t.Errorf("%s has position %s, want %d", got.BranchName, position(got.Position), pos)
		}
	}

	deletes := f.patchEvents(t, carrypatch.EventPatchDelete)
	if len(deletes) != 1 || deletes[0].ResourceID != p1.ID || deletes[0].ActorType != event.ActorSystem ||
		!reflect.DeepEqual(deletes[0].Metadata, map[string]any{"branch_name": "p1", "reason": "merged_upstream"}) {
		t.Errorf("parley.patch.delete events = %+v, want one for p1 by system with reason merged_upstream", deletes)
	}
	clean := cleanUpdates(f.patchEvents(t, carrypatch.EventPatchUpdate))
	wantMeta := map[string]any{"branch_name": "p2", "changed": []any{"status"}, "from": "conflict", "to": "active", "reason": "rebuild_clean"}
	if len(clean) != 1 || clean[0].ResourceID != p2.ID || clean[0].ActorType != event.ActorSystem || !reflect.DeepEqual(clean[0].Metadata, wantMeta) {
		t.Errorf("parley.patch.update events with reason rebuild_clean = %+v, want one for p2 by system, %v", clean, wantMeta)
	}
}
