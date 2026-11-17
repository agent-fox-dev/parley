package carrypatch_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestRebuild_ReplaysARecordedResolution(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	p1, commits := f.conflictingPatch(t, "p1", "a.txt")
	base := f.moveOn(t)
	f.fetch(t)
	seedResolution(t, f.copy, base, commits[1], map[string]string{"a.txt": resolvedA})

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	results := f.patchResults(t, id)
	if len(results) != 1 || results[0]["status"] != "success" {
		t.Fatalf("patch_results = %v, want p1 a success", results)
	}
	if got := commitsIn(t, f.copy, base, "refs/heads/deploy", "%s"); !slices.Equal(got, []string{"p1 adds", "p1 changes"}) {
		t.Errorf("base..deploy = %q, want both commits of p1", got)
	}
	if got := fileIn(t, f.copy, "refs/heads/deploy", "a.txt"); got != resolvedA || strings.Contains(got, "<<<<<<<") {
		t.Errorf("a.txt in deploy = %q, want the recorded resolution %q", got, resolvedA)
	}
	if got := f.stored(t, p1.ID); got.Status != carrypatch.StatusActive || len(got.ConflictFiles) != 0 {
		t.Errorf("p1 in the store has status %s and conflict_files %q, want active with none", got.Status, got.ConflictFiles)
	}
}

func TestRebuild_ReportsOnlyPathsLeftInConflict(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	// The two conflicts differ: git gives one conflict met at two paths one
	// id, and one resolution.
	c := gitrepotest.Commit(t, f.copy, "C", map[string]string{"a.txt": "1\n2\n3 p1\n", "b.txt": "1\n2\n3 p1 in b\n"}, f.cut)
	p1 := f.putPatch(t, "p1", c)
	base := f.moveOn(t)
	f.fetch(t)
	seedResolution(t, f.copy, base, c, map[string]string{"a.txt": resolvedA})

	out, id := f.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	want := []string{"b.txt"}
	if got := f.stored(t, p1.ID); got.Status != carrypatch.StatusConflict || !slices.Equal(got.ConflictFiles, want) {
		t.Errorf("p1 in the store has status %s and conflict_files %q, want conflict and %q", got.Status, got.ConflictFiles, want)
	}
	results := f.patchResults(t, id)
	if len(results) != 1 {
		t.Fatalf("patch_results = %v, want one", results)
	}
	if got := results[0]["conflict_files"]; !reflect.DeepEqual(got, []any{"b.txt"}) {
		t.Errorf("conflict_files of p1's result = %v, want %q", got, want)
	}
	report, _ := results[0]["conflict"].(map[string]any)
	files, _ := report["files"].([]any)
	var paths []string
	for _, file := range files {
		if m, ok := file.(map[string]any); ok {
			p, _ := m["path"].(string)
			paths = append(paths, p)
		}
	}
	if !slices.Equal(paths, want) {
		t.Errorf("the paths of the conflict report's files = %q, want %q", paths, want)
	}
}

func TestRebuild_RecordsUnresolvedConflicts(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	f.conflictingPatch(t, "p1", "a.txt")
	f.moveOn(t)

	if out, _ := f.attemptWith(t, continueRebase, nil); out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	ids := rrCacheIDs(t, f.copy)
	if len(ids) != 1 {
		t.Fatalf("the rr-cache of the copy holds %q, want the one conflict", ids)
	}
	want := []carrypatch.RerereEntry{{ID: ids[0], Path: "a.txt", Resolved: false, RecordedAt: f.clock()}}
	if got := f.entries(t); !sameEntries(got, want) {
		t.Errorf("the entries after the first run = %+v, want %+v", got, want)
	}

	f.advance(time.Minute)
	if out, _ := f.attemptWith(t, continueRebase, nil); out.Kind != attemptCompleted {
		t.Fatalf("the second attempt = %+v, want completed", out)
	}
	if got := f.entries(t); !sameEntries(got, want) {
		t.Errorf("the entries after a second run met the same conflict = %+v, want the first run's alone, %+v", got, want)
	}
}

func TestRebuild_ResolutionsArePerWorkspace(t *testing.T) {
	t.Parallel()
	a := startConflictFixture(t, nil)
	b := a.another(t, "beta")
	_, commitsA := a.conflictingPatch(t, "p1", "a.txt")
	pb, _ := b.conflictingPatch(t, "p1", "a.txt")
	base := a.moveOn(t)
	a.fetch(t)
	seedResolution(t, a.copy, base, commitsA[1], map[string]string{"a.txt": resolvedA})

	out, id := b.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt of beta = %+v, want completed", out)
	}
	if results := b.patchResults(t, id); len(results) != 1 || results[0]["status"] != "conflict" {
		t.Errorf("beta's patch_results = %v, want p1 a conflict", results)
	}
	if got := b.stored(t, pb.ID); got.Status != carrypatch.StatusConflict {
		t.Errorf("beta's p1 has status %s, want conflict", got.Status)
	}
	ids := rrCacheIDs(t, b.copy)
	if len(ids) != 1 {
		t.Fatalf("the rr-cache of beta's copy holds %q, want its own conflict", ids)
	}
	want := []carrypatch.RerereEntry{{ID: ids[0], Path: "a.txt", Resolved: false, RecordedAt: b.clock()}}
	if got := b.entries(t); !sameEntries(got, want) {
		t.Errorf("beta's entries = %+v, want its own unresolved one, %+v", got, want)
	}
	if got := a.entries(t); len(got) != 0 {
		t.Errorf("acme's entries = %+v, want none: no run of acme met a conflict", got)
	}
}

func TestRebuild_LearnsAResolutionFromAMergeOnThePatchBranch(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	c := gitrepotest.Commit(t, f.copy, "C", map[string]string{"a.txt": "1\n2\n3 p1\n"}, f.cut)
	p1 := f.putPatch(t, "p1", c)
	u2 := f.moveOn(t)

	// The first run meets the conflict and has nothing to learn it from.
	out, id := f.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the first attempt = %+v, want completed", out)
	}
	if results := f.patchResults(t, id); len(results) != 1 || results[0]["status"] != "conflict" {
		t.Fatalf("the first run's patch_results = %v, want p1 a conflict", results)
	}
	entries := f.entries(t)
	if len(entries) != 1 || entries[0].Path != "a.txt" || entries[0].Resolved {
		t.Fatalf("the entries after the first run = %+v, want one for a.txt, unresolved", entries)
	}

	// The developer merges the upstream into the patch branch and resolves.
	m := commitAs(t, f.copy, "Merge upstream", "Dev <dev@example.invalid> 1500000000 +0000",
		map[string]string{"a.txt": resolvedA, "b.txt": "1\n2\n3 upstream\n"}, c, u2)
	gitrepotest.SetRef(t, f.copy, "refs/heads/p1", m)
	f.advance(time.Minute)
	learnedAt := f.clock()

	out, id = f.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the second attempt = %+v, want completed", out)
	}
	if results := f.patchResults(t, id); len(results) != 1 || results[0]["status"] != "success" {
		t.Fatalf("the second run's patch_results = %v, want p1 a success", results)
	}
	if got := fileIn(t, f.copy, "refs/heads/deploy", "a.txt"); got != resolvedA {
		t.Errorf("a.txt in deploy = %q, want the merge commit's %q", got, resolvedA)
	}
	if got := f.stored(t, p1.ID); got.Status != carrypatch.StatusActive || len(got.ConflictFiles) != 0 {
		t.Errorf("p1 after the learning has status %s and conflict_files %q, want active with none", got.Status, got.ConflictFiles)
	}
	want := []carrypatch.RerereEntry{{ID: entries[0].ID, Path: "a.txt", Resolved: true, RecordedAt: learnedAt}}
	if got := f.entries(t); !sameEntries(got, want) {
		t.Errorf("the entries after the learning = %+v, want %+v", got, want)
	}

	// Without the merge commit, the resolution stays learned.
	gitrepotest.SetRef(t, f.copy, "refs/heads/p1", c)
	f.moveUpstream(t, "U4", map[string]string{"c.txt": "c\n"})
	out, id = f.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the third attempt = %+v, want completed", out)
	}
	if results := f.patchResults(t, id); len(results) != 1 || results[0]["status"] != "success" {
		t.Fatalf("the third run's patch_results = %v, want p1 a success", results)
	}
	if got := fileIn(t, f.copy, "refs/heads/deploy", "a.txt"); got != resolvedA {
		t.Errorf("a.txt in deploy after the third run = %q, want the learned %q", got, resolvedA)
	}
}

func TestRebuild_UnrelatedMergeCommitsTeachNothing(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	c := gitrepotest.Commit(t, f.copy, "C", map[string]string{"a.txt": "1\n2\n3 p1\n"}, f.cut)
	other := gitrepotest.Commit(t, f.copy, "other", map[string]string{"o.txt": "o\n"}, f.cut)
	m := commitAs(t, f.copy, "Merge other", "Dev <dev@example.invalid> 1500000000 +0000", map[string]string{"o.txt": "o\n"}, c, other)
	p1 := f.putPatch(t, "p1", m)
	f.moveOn(t)

	out, id := f.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	if results := f.patchResults(t, id); len(results) != 1 || results[0]["status"] != "conflict" {
		t.Errorf("patch_results = %v, want p1 a conflict", results)
	}
	if got := f.stored(t, p1.ID); got.Status != carrypatch.StatusConflict {
		t.Errorf("p1 has status %s, want conflict", got.Status)
	}
	if got := f.entries(t); len(got) != 1 || got[0].Path != "a.txt" || got[0].Resolved {
		t.Errorf("the entries = %+v, want one for a.txt, unresolved", got)
	}
}
