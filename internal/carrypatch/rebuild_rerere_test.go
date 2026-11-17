package carrypatch_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// resolvedA is the content a.txt is resolved to.
const resolvedA = "1\n2\n3 resolved\n"

// seedResolution records in the repository dir the resolution of the
// conflict that replaying commit onto onto meets, as a developer resolving
// it once in a working tree of the repository with rerere on would: the
// files of resolved get the content given, every other conflicted path is
// left unresolved.
func seedResolution(t *testing.T, dir, onto, commit string, resolved map[string]string) {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "seed")
	gitrepotest.Git(t, dir, "worktree", "add", "--quiet", "--detach", wt, onto)
	defer gitrepotest.Git(t, dir, "worktree", "remove", "--force", wt)
	rerere := func(args ...string) error {
		_, err := gitrepotest.Run(filepath.Join(dir, "worktrees", "seed"), "", append([]string{"-C", wt, "-c", "rerere.enabled=true"}, args...)...)
		return err
	}
	if err := rerere("cherry-pick", "--no-commit", commit); err == nil {
		t.Fatalf("%s applies onto %s; want a conflict to resolve", commit, onto)
	}
	for name, content := range resolved {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := rerere("rerere"); err != nil {
		t.Fatal(err)
	}
}

// fetch fetches the upstream into the hub's copy, as an attempt would.
func (f *conflictFixture) fetch(t *testing.T) {
	t.Helper()
	if err := f.comp.EnsureSetup(context.Background(), f.record(t)); err != nil {
		t.Fatal(err)
	}
}

// entries are the remembered-resolution entries of the fixture's workspace.
func (f *conflictFixture) entries(t *testing.T) []carrypatch.RerereEntry {
	t.Helper()
	e, err := f.comp.RerereEntries(context.Background(), f.slug)
	if err != nil {
		t.Fatalf("the remembered-resolution entries of %s: %v", f.slug, err)
	}
	return e
}

// rrCacheIDs are the ids of the conflicts the rr-cache of the repository
// dir holds.
func rrCacheIDs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "rr-cache"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Name())
	}
	return ids
}

// sameEntries reports whether got are the entries want.
func sameEntries(got, want []carrypatch.RerereEntry) bool {
	return slices.EqualFunc(got, want, func(a, b carrypatch.RerereEntry) bool {
		return a.ID == b.ID && a.Path == b.Path && a.Resolved == b.Resolved && a.RecordedAt.Equal(b.RecordedAt)
	})
}

// fileIn is the content of path in rev of the repository dir.
func fileIn(t *testing.T, dir, rev, path string) string {
	t.Helper()
	return string(gitrepotest.Blob(t, dir, rev+":"+path))
}

// another is a second carry-patch workspace slug of the fixture's hub, set
// up like the fixture's own, with its own copy, which the fixture's attempts
// rebuild when they are run through it.
func (f *conflictFixture) another(t *testing.T, slug string) *conflictFixture {
	t.Helper()
	w := f.record(t)
	f.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, func(n *workspace.Workspace) {
		n.IntegrationBranch, n.UpstreamURL, n.GitURL = w.IntegrationBranch, w.UpstreamURL, w.GitURL
	})
	g := &rebuildFixture{addHub: f.addHub, slug: slug, upstream: f.upstream, origin: f.origin,
		copy: f.repoPath(slug), comp: f.comp, queue: f.queue, secrets: f.secrets, runner: f.runner}
	gitrepotest.Git(t, g.copy, "init", "--quiet", "--bare", g.copy)
	if err := g.comp.EnsureSetup(context.Background(), g.record(t)); err != nil {
		t.Fatalf("set up the workspace %s: %v", slug, err)
	}
	g.base = refIn(g.copy, "refs/heads/deploy")
	return &conflictFixture{rebuildFixture: g, describer: f.describer, cut: refIn(g.copy, "refs/remotes/upstream/main")}
}

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
	c := commitFiles(t, f.copy, f.cut, "C", map[string]string{"a.txt": "1\n2\n3 p1\n", "b.txt": "1\n2\n3 p1 in b\n"})
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
	c := commitFiles(t, f.copy, f.cut, "C", map[string]string{"a.txt": "1\n2\n3 p1\n"})
	f.putPatch(t, "p1", c)
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
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/p1", m)
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
	want := []carrypatch.RerereEntry{{ID: entries[0].ID, Path: "a.txt", Resolved: true, RecordedAt: learnedAt}}
	if got := f.entries(t); !sameEntries(got, want) {
		t.Errorf("the entries after the learning = %+v, want %+v", got, want)
	}

	// Without the merge commit, the resolution stays learned.
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/p1", c)
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
	c := commitFiles(t, f.copy, f.cut, "C", map[string]string{"a.txt": "1\n2\n3 p1\n"})
	other := commitFiles(t, f.copy, f.cut, "other", map[string]string{"o.txt": "o\n"})
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
