package gitrepo_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestTrialMerge_GivesTheMergedTreeOrTheConflicts(t *testing.T) {
	t.Parallel()
	f := newCompareFixture(t)
	ctx := context.Background()
	// A root commit of its own shares no ancestor with main.
	unrelated := gitrepotest.Commit(t, f.dir, "unrelated", map[string]string{"y.txt": "y\n"})
	conflict := func(kind, base, ours, theirs string) []gitrepo.ConflictFile {
		return []gitrepo.ConflictFile{{Path: "f.txt", Kind: kind,
			Base: f.side(t, base, "f.txt"), Ours: f.side(t, ours, "f.txt"), Theirs: f.side(t, theirs, "f.txt")}}
	}
	refs := gitrepotest.Git(t, f.dir, "for-each-ref")
	for _, c := range []struct {
		name, ours, theirs string
		theirsID           string
		base               string
		// files are the paths the merged tree holds, for a clean merge.
		files     []string
		conflicts []gitrepo.ConflictFile
	}{
		{name: "a clean merge", ours: "main", theirs: "feature", base: "main~1",
			files: []string{"a.txt", "b.txt", "bin", "f.txt", "r1.txt", "sub", "t.txt", "x.txt"}},
		{name: "a clean merge of commits that share no ancestor", ours: "main", theirsID: unrelated,
			files: []string{"bin", "f.txt", "r1.txt", "sub", "t.txt", "x.txt", "y.txt"}},
		{name: "a merge of a commit ours holds", ours: "main", theirs: "main~1", base: "main~1",
			files: []string{"bin", "f.txt", "r1.txt", "sub", "t.txt", "x.txt"}},
		{name: "a content conflict", ours: "main", theirs: "both-edit", base: "main~1",
			conflicts: conflict(gitrepo.ConflictContent, "main~1", "main", "both-edit")},
		{name: "an add/add conflict", ours: "main", theirs: "both-add", base: "main~2",
			conflicts: conflict(gitrepo.ConflictAddAdd, "main~2", "main", "both-add")},
		{name: "a modify/delete conflict", ours: "main", theirs: "head-deletes", base: "main~1",
			conflicts: conflict(gitrepo.ConflictModifyDelete, "main~1", "main", "head-deletes")},
	} {
		t.Run(c.name, func(t *testing.T) {
			theirs := c.theirsID
			if theirs == "" {
				theirs = f.id(t, c.theirs)
			}
			got, err := f.repo.TrialMerge(ctx, f.id(t, c.ours), theirs)
			if err != nil {
				t.Fatalf("TrialMerge(%s, %s) = %v", c.ours, theirs, err)
			}
			base := ""
			if c.base != "" {
				base = f.id(t, c.base)
			}
			if got.Base != base {
				t.Errorf("the merge base = %q, want %q", got.Base, base)
			}
			if !reflect.DeepEqual(got.Conflicts, c.conflicts) {
				t.Errorf("the conflicts =\n%s\nwant\n%s", describeFiles(got.Conflicts), describeFiles(c.conflicts))
			}
			if c.conflicts != nil {
				return
			}
			if names := strings.Fields(gitrepotest.Git(t, f.dir, "ls-tree", "--name-only", got.Tree)); !reflect.DeepEqual(names, c.files) {
				t.Errorf("the merged tree %s holds %q, want %q", got.Tree, names, c.files)
			}
		})
	}
	if after := gitrepotest.Git(t, f.dir, "for-each-ref"); after != refs {
		t.Errorf("the refs changed from\n%s\nto\n%s", refs, after)
	}
}

func TestTrialMerge_ReportsAFailureThatIsNoConflictAsAnError(t *testing.T) {
	t.Parallel()
	f := newCompareFixture(t)
	ctx := context.Background()
	main := f.id(t, "main")
	for _, c := range []struct{ name, ours, theirs string }{
		{"an id of no object", main, strings.Repeat("ab", 20)},
		{"a tree", main, f.id(t, "main^{tree}")},
		{"a name that is not a full id", main, "feature"},
		{"an option", "--help", main},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := f.repo.TrialMerge(ctx, c.ours, c.theirs)
			if err == nil || got.Conflicts != nil || got.Tree != "" {
				t.Errorf("TrialMerge(%s, %s) = %+v, %v; want an error and no result", c.ours, c.theirs, got, err)
			}
			if c.theirs == "feature" && !errors.Is(err, gitrepo.ErrNotFound) {
				t.Errorf("TrialMerge of a name = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestTrialMerge_MergesFromEveryMergeBaseAsGitMergeDoes(t *testing.T) {
	t.Parallel()
	f := newCompareFixture(t)
	ctx := context.Background()
	// A criss-cross: x and y change the same line, and o and t merge them
	// both, o keeping x's line and t keeping y's, so that o and t have two
	// merge bases. Either base alone merges o and t cleanly; git merge,
	// which merges from every merge base, finds the conflict.
	base := gitrepotest.Commit(t, f.dir, "base", map[string]string{"cc.txt": "0\n"})
	x := gitrepotest.Commit(t, f.dir, "x", map[string]string{"cc.txt": "x\n"}, base)
	y := gitrepotest.Commit(t, f.dir, "y", map[string]string{"cc.txt": "y\n"}, base)
	o := gitrepotest.Commit(t, f.dir, "o", map[string]string{"cc.txt": "x\n"}, x, y)
	theirs := gitrepotest.Commit(t, f.dir, "t", map[string]string{"cc.txt": "y\n"}, y, x)
	if bases := strings.Fields(gitrepotest.Git(t, f.dir, "merge-base", "--all", o, theirs)); len(bases) != 2 {
		t.Fatalf("the merge bases of o and t = %q, want x and y", bases)
	}
	got, err := f.repo.TrialMerge(ctx, o, theirs)
	if err != nil {
		t.Fatalf("TrialMerge(o, t) = %v", err)
	}
	if len(got.Conflicts) != 1 || got.Conflicts[0].Path != "cc.txt" {
		t.Errorf("the conflicts = %s, want cc.txt in conflict as git merge finds it", describeFiles(got.Conflicts))
	}
}
