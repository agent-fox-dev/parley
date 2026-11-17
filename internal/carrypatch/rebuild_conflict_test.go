package carrypatch_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestRebuild_ConflictUndoesThePatchAndMarksIt(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	p1, commits := f.conflictingPatch(t, "p1", "b.txt", "a.txt")
	base := f.moveOn(t)

	out, id := f.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	files := []string{"a.txt", "b.txt"}
	if got := f.stored(t, p1.ID); got.Status != carrypatch.StatusConflict || !slices.Equal(got.ConflictFiles, files) {
		t.Errorf("p1 in the store has status %s and conflict_files %q, want conflict and %q", got.Status, got.ConflictFiles, files)
	}
	results := f.patchResults(t, id)
	if len(results) != 1 {
		t.Fatalf("patch_results = %v, want one", results)
	}
	r := results[0]
	if r["status"] != "conflict" || !reflect.DeepEqual(r["conflict_files"], []any{"a.txt", "b.txt"}) || r["new_head_sha"] != nil || r["conflict"] == nil {
		t.Errorf("p1's result = %v, want conflict with conflict_files %q, new_head_sha null and a conflict report", r, files)
	}
	updates := f.patchEvents(t, carrypatch.EventPatchUpdate)
	if len(updates) != 1 {
		t.Fatalf("parley.patch.update events = %+v, want one", updates)
	}
	e := updates[0]
	want := map[string]any{"branch_name": "p1", "changed": []any{"status"}, "from": "active", "to": "conflict", "reason": "rebuild_conflict"}
	if e.ResourceType != "patch" || e.ResourceID != p1.ID || e.ActorType != event.ActorSystem || e.ActorID != nil || !reflect.DeepEqual(e.Metadata, want) {
		t.Errorf("the parley.patch.update = %+v, want resource patch %s, actor system and the metadata %v", e, p1.ID, want)
	}
	if deploy := refIn(f.copy, "refs/heads/deploy"); deploy != base {
		t.Errorf("deploy = %s, want the upstream base %s", deploy, base)
	}
	if _, err := gitrepotest.Run(f.copy, "", "merge-base", "--is-ancestor", commits[0], "refs/heads/deploy"); err == nil {
		t.Errorf("the first commit of p1, %s, is in deploy; want the patch undone", commits[0])
	}

	// The patch is conflict already: a later conflict changes no status.
	if out, _ := f.attemptWith(t, continueRebase, nil); out.Kind != attemptCompleted {
		t.Fatalf("the second attempt = %+v, want completed", out)
	}
	if got := f.patchEvents(t, carrypatch.EventPatchUpdate); len(got) != 1 {
		t.Errorf("parley.patch.update events after a second conflict = %+v, want the first alone", got)
	}
}

func TestRebuild_ConflictReportMembers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		strategy string
		// first stores an applying patch before the conflicting one.
		first bool
	}{
		{"rebase, the conflicting patch first", "rebase", false},
		{"rebase, the conflicting patch after an applied patch", "rebase", true},
		{"merge", "merge", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startConflictFixture(t, nil)
			if tc.first {
				f.addPatch(t, "p0")
			}
			p1, commits := f.conflictingPatch(t, "p1", "a.txt")
			f.moveOn(t)

			out, id := f.attemptWith(t, carrypatch.RebuildOptions{Strategy: &tc.strategy, FailMode: new("continue")}, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			results := f.patchResults(t, id)
			target, _ := f.result(t, id)["upstream_head_sha"].(string)
			if tc.first {
				target, _ = results[0]["new_head_sha"].(string)
			}
			source := commits[1]
			commit := commits[1]
			if tc.strategy == "merge" {
				commit = source
			}
			rep, ok := results[len(results)-1]["conflict"].(map[string]any)
			if !ok {
				t.Fatalf("p1's result = %v, want a conflict report", results[len(results)-1])
			}
			if keys := slices.Sorted(maps.Keys(rep)); !slices.Equal(keys, []string{"base_sha", "change", "commit_sha", "files", "head_sha", "landed_changes", "patch_id", "target_sha"}) {
				t.Errorf("the members of the report = %q, want the eight of a rebuild's conflict report", keys)
			}
			mergeBase := gitrepotest.Git(t, f.copy, "merge-base", source, target)
			wantMembers := map[string]any{"patch_id": p1.ID, "target_sha": target, "head_sha": source, "base_sha": mergeBase,
				"commit_sha": commit, "landed_changes": []any{}, "change": nil}
			for k, v := range wantMembers {
				if !reflect.DeepEqual(rep[k], v) {
					t.Errorf("%s of the report = %v, want %v", k, rep[k], v)
				}
			}
			// The three-way base the describer is given: the replayed commit's
			// parent, or the merge base of a merged patch.
			threeWay := commits[0]
			if tc.strategy == "merge" {
				threeWay = mergeBase
			}
			if got, want := f.describer.recorded(), []describeCall{{threeWay, target, commit, []string{"a.txt"}}}; !reflect.DeepEqual(got, want) {
				t.Errorf("the describer's calls = %+v, want %+v", got, want)
			}
			raw, err := json.Marshal([]gitrepo.ConflictFile{describedFile(f.copy, target, commit, "a.txt")})
			if err != nil {
				t.Fatal(err)
			}
			var wantFiles []any
			if err := json.Unmarshal(raw, &wantFiles); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rep["files"], wantFiles) {
				t.Errorf("files of the report = %v, want the describer's entries with landed_by []: %v", rep["files"], wantFiles)
			}
		})
	}
}

func TestRebuild_FailFastStopsAndPublishesNothing(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	f.addPatch(t, "p1")
	f.conflictingPatch(t, "feature/b", "a.txt")
	p3 := f.addPatch(t, "p3")
	f.moveOn(t)
	// A run that publishes nothing leaves a missing integration branch
	// missing.
	gitrepotest.Git(t, f.copy, "update-ref", "-d", "refs/heads/deploy")
	stored := f.stored(t, p3.ID)

	out, id := f.attemptWith(t, carrypatch.RebuildOptions{FailMode: new("fail_fast")}, nil)
	if out.Kind != attemptPermanent || out.Reason != "conflict" {
		t.Fatalf("the attempt = %+v, want permanent with reason conflict", out)
	}
	if !strings.Contains(out.Text, "feature/b") {
		t.Errorf("the attempt's text %q does not name the branch feature/b", out.Text)
	}
	results := f.patchResults(t, id)
	if len(results) != 2 {
		t.Fatalf("patch_results = %v, want the results of p1 and feature/b", results)
	}
	var conflict any
	if err := json.Unmarshal(out.Conflict, &conflict); err != nil || conflict == nil || !reflect.DeepEqual(conflict, results[1]["conflict"]) {
		t.Errorf("the attempt's conflict = %s (%v), want the report of feature/b's result, %v", out.Conflict, err, results[1]["conflict"])
	}
	res := f.result(t, id)
	for _, k := range []string{"patches_applied", "patches_skipped", "patches_conflicted", "patches_removed"} {
		if res[k] != float64(0) {
			t.Errorf("%s = %v, want 0", k, res[k])
		}
	}
	if res["integration_head_sha"] != nil {
		t.Errorf("integration_head_sha = %v, want null", res["integration_head_sha"])
	}
	if got := refIn(f.copy, "refs/heads/deploy"); got != "" {
		t.Errorf("deploy = %s, want it still missing", got)
	}
	if got := f.stored(t, p3.ID); !reflect.DeepEqual(got, stored) {
		t.Errorf("p3 after the attempt = %+v, want it unchanged, %+v", got, stored)
	}
}

func TestRebuild_ContinuePublishesWithoutTheConflictingPatch(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	f.addPatch(t, "p1")
	p2, _ := f.conflictingPatch(t, "p2", "a.txt")
	f.addPatch(t, "p3")
	base := f.moveOn(t)

	out, id := f.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	results := f.patchResults(t, id)
	var statuses []string
	for _, r := range results {
		s, _ := r["status"].(string)
		statuses = append(statuses, s)
	}
	if want := []string{"success", "conflict", "success"}; !slices.Equal(statuses, want) {
		t.Fatalf("the statuses of patch_results = %q, want %q", statuses, want)
	}
	log := commitsIn(t, f.copy, base, refIn(f.copy, "refs/heads/deploy"), "%P %s")
	if want := []string{base + " p1", results[0]["new_head_sha"].(string) + " p3"}; !slices.Equal(log, want) {
		t.Errorf("base..deploy (parent and subject) = %q, want p1 on the base and p3 on p1's new head: %q", log, want)
	}
	if got := f.stored(t, p2.ID); got.Status != carrypatch.StatusConflict {
		t.Errorf("p2 in the store has status %s, want conflict", got.Status)
	}
}

func TestRebuild_NonConflictFailuresAreRetryable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// prepare makes the replay of p2, the second patch, fail; deps
		// changes the component's dependencies.
		prepare func(t *testing.T, f *rebuildFixture, p2 string)
		deps    func(t *testing.T, d *carrypatch.Deps)
		reason  string
	}{
		{"a commit of the second patch is missing", func(t *testing.T, f *rebuildFixture, p2 string) {
			f.hooks.beforePatch(func(i int) {
				if i == 1 {
					if err := os.Remove(filepath.Join(f.copy, "objects", p2[:2], p2[2:])); err != nil {
						t.Errorf("remove the commit of p2: %v", err)
					}
				}
			})
		}, nil, "git_error"},
		{"the replay of the second patch outlasts the limit", nil, func(t *testing.T, d *carrypatch.Deps) {
			d.RebuildGitDeadline = slowGitLimit
		}, "timeout"},
		{"the hub stops before the second patch", func(t *testing.T, f *rebuildFixture, _ string) {
			f.hooks.beforePatch(func(i int) {
				if i == 1 {
					f.runner.cancelAttempt()
				}
			})
		}, nil, "shutdown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixtureWith(t, func(d *carrypatch.Deps) {
				if tc.deps != nil {
					tc.deps(t, d)
				}
			})
			f.addPatch(t, "p1")
			var pid string
			var p2 carrypatch.Patch
			if tc.reason == "timeout" {
				// The replay of p2 checks out slow.txt, whose filter sleeps.
				pid = slowFilter(t, f.copy)
				p2 = f.putPatch(t, "p2", gitrepotest.Commit(t, f.copy, "p2", map[string]string{"slow.txt": "slow\n"}, f.base))
			} else {
				p2 = f.addPatch(t, "p2")
			}
			if tc.prepare != nil {
				tc.prepare(t, f, refIn(f.copy, "refs/heads/p2"))
			}
			before := refIn(f.copy, "refs/heads/deploy")

			out, id := f.attempt(t, nil)
			if out.Kind != attemptRetryable || out.Reason != tc.reason {
				t.Errorf("the attempt = %+v, want retryable with reason %s", out, tc.reason)
			}
			if results := f.patchResults(t, id); len(results) != 1 {
				t.Errorf("patch_results = %v, want p1's result alone", results)
			}
			if got := f.stored(t, p2.ID); got.Status != carrypatch.StatusActive || len(got.ConflictFiles) != 0 {
				t.Errorf("p2 in the store has status %s and conflict_files %q, want active and none", got.Status, got.ConflictFiles)
			}
			if got := refIn(f.copy, "refs/heads/deploy"); got != before {
				t.Errorf("deploy = %s, want it where it was, %s", got, before)
			}
			if pid != "" {
				waitGone(t, pid)
			}
		})
	}
}

func TestRebuild_MergeConflictFilesDescribeTheMergeBase(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, func(d *carrypatch.Deps) { d.ConflictFiles = nil })
	// The first commit of p1 meets upstream's change of a.txt; the second,
	// the branch's tip, leaves a.txt alone.
	first := gitrepotest.Commit(t, f.copy, "p1 changes", map[string]string{"a.txt": "1\n2\n3 p1\n"}, f.cut)
	tip := gitrepotest.Commit(t, f.copy, "p1 adds", map[string]string{"p1.txt": "p1\n"}, first)
	f.putPatch(t, "p1", tip)
	target := f.moveOn(t)

	out, id := f.attemptWith(t, carrypatch.RebuildOptions{Strategy: new("merge"), FailMode: new("continue")}, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	rep, ok := f.patchResults(t, id)[0]["conflict"].(map[string]any)
	if !ok {
		t.Fatalf("p1's result = %v, want a conflict report", f.patchResults(t, id)[0])
	}
	if rep["base_sha"] != f.cut {
		t.Errorf("base_sha of the report = %v, want the merge base %s", rep["base_sha"], f.cut)
	}
	raw, err := json.Marshal([]gitrepo.ConflictFile{{Path: "a.txt", Kind: gitrepo.ConflictContent,
		Base: blobSide(f.copy, f.cut, "a.txt"), Ours: blobSide(f.copy, target, "a.txt"), Theirs: blobSide(f.copy, tip, "a.txt"),
		LandedBy: []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	var want []any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep["files"], want) {
		t.Errorf("files of the report = %v, want a.txt of the merge base, upstream and p1's tip: %v", rep["files"], want)
	}
}
