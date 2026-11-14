package gitrepo_test

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// The fork, made from the parent, has its own branch work, an outdated
// main, a branch gone the parent does not have, and version refs of change
// 7 the parent lacks and of changes 8 and 9; the copy names main, v1 and
// gone, takes every ref of change 7 and prunes refs/parley/changes/.
func TestCopyRefs_SetsTheNamedRefsAtTheSourcesValuesAndDeletesTheRest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	parent, head, release, v1 := forkParent(t, 3)
	first := gitrepotest.Git(t, parent, "rev-parse", "main~2")
	for ref, id := range map[string]string{"refs/parley/changes/7/1": first, "refs/parley/changes/7/2": head, "refs/parley/changes/8/1": release} {
		gitrepotest.Git(t, parent, "update-ref", ref, id)
	}
	fork := newFork(t, parent)
	for _, ref := range []string{"refs/heads/work", "refs/heads/main", "refs/heads/gone", "refs/parley/changes/7/3", "refs/parley/changes/8/1", "refs/parley/changes/9/1"} {
		gitrepotest.Git(t, fork, "update-ref", ref, first)
	}
	c := gitrepo.RefCopy{
		Names:    []string{"refs/heads/main", "refs/tags/v1", "refs/heads/gone"},
		Prefixes: []string{"refs/parley/changes/7/"},
		Prune:    "refs/parley/changes/",
	}
	got, err := (gitrepo.Git{}).CopyRefs(ctx, parent, fork, c)
	if err != nil {
		t.Fatalf("CopyRefs: %v", err)
	}
	want := gitrepo.Copied{
		Set:     []string{"refs/heads/main", "refs/parley/changes/7/1", "refs/parley/changes/7/2", "refs/tags/v1"},
		Deleted: []string{"refs/heads/gone", "refs/parley/changes/7/3", "refs/parley/changes/8/1", "refs/parley/changes/9/1"},
	}
	if !slices.Equal(got.Set, want.Set) || !slices.Equal(got.Deleted, want.Deleted) {
		t.Errorf("CopyRefs = %+v, want %+v", got, want)
	}
	wantRefs := map[string]string{
		"refs/heads/work": first, "refs/heads/main": head, "refs/tags/v1": v1,
		"refs/parley/changes/7/1": first, "refs/parley/changes/7/2": head,
	}
	if refs := gitrepotest.Refs(t, fork, ""); !maps.Equal(refs, wantRefs) {
		t.Errorf("the fork's refs are %v, want %v", refs, wantRefs)
	}
	// The fork borrows the parent's objects: the copy moved refs, no object.
	if out := gitrepotest.Git(t, fork, "count-objects", "-v"); !strings.Contains(out, "count: 0\n") || !strings.Contains(out, "in-pack: 0\n") {
		t.Errorf("the fork holds objects of its own after the copy:\n%s", out)
	}
	if again, err := (gitrepo.Git{}).CopyRefs(ctx, parent, fork, c); err != nil || len(again.Set)+len(again.Deleted) != 0 {
		t.Errorf("a second copy = %+v, %v; want nothing changed", again, err)
	}
}

func TestCopyRefs_FailsWithoutChangingTheDestinationWhenTheSourceIsNoRepository(t *testing.T) {
	t.Parallel()
	parent, head, _, _ := forkParent(t, 1)
	fork := newFork(t, parent)
	gitrepotest.Git(t, fork, "update-ref", "refs/parley/changes/9/1", head)
	_, err := (gitrepo.Git{}).CopyRefs(context.Background(), filepath.Join(t.TempDir(), "missing.git"), fork,
		gitrepo.RefCopy{Names: []string{"refs/heads/main"}, Prune: "refs/parley/changes/"})
	if err == nil {
		t.Error("CopyRefs from a missing repository = nil, want an error")
	}
	if refs := gitrepotest.Refs(t, fork, ""); !maps.Equal(refs, map[string]string{"refs/parley/changes/9/1": head}) {
		t.Errorf("the fork's refs are %v, want them unchanged", refs)
	}
}
