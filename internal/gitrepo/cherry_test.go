package gitrepo_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// bareRepo is a bare repository made by git init in a directory of the
// test's own, opened as a Repo, and its directory.
func bareRepo(t *testing.T) (*gitrepo.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	repo, err := gitrepo.Git{}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, dir
}

func TestCherry_TellsWhichCommitsUpstreamHoldsAnEquivalentOf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, dir := bareRepo(t)
	base := gitrepotest.Commit(t, dir, "base", map[string]string{"base.txt": "base\n"})
	// Upstream took the change of p1 as a commit of its own, under another
	// message, so with another id.
	up := gitrepotest.Commit(t, dir, "take a", map[string]string{"a.txt": "a\n"}, base)
	p1 := gitrepotest.Commit(t, dir, "add a", map[string]string{"a.txt": "a\n"}, base)
	p2 := gitrepotest.Commit(t, dir, "add b", map[string]string{"b.txt": "b\n"}, p1)
	merge := gitrepotest.Commit(t, dir, "merge", map[string]string{"m.txt": "m\n"}, p2, base)
	p3 := gitrepotest.Commit(t, dir, "add c", map[string]string{"c.txt": "c\n"}, merge)
	upAll := gitrepotest.Commit(t, dir, "take b and c", map[string]string{"b.txt": "b\n", "c.txt": "c\n"}, up)
	upB := gitrepotest.Commit(t, dir, "take b", map[string]string{"b.txt": "b\n"}, up)

	for _, c := range []struct {
		name           string
		upstream, head string
		want           []gitrepo.CherryCommit
	}{
		{"one of two taken", up, p2, []gitrepo.CherryCommit{{ID: p1, Equivalent: true}, {ID: p2}}},
		{"both taken, one by one", upB, p2, []gitrepo.CherryCommit{{ID: p1, Equivalent: true}, {ID: p2, Equivalent: true}}},
		// A merge commit on the head's side is left out.
		{"merge commit left out", up, p3, []gitrepo.CherryCommit{{ID: p1, Equivalent: true}, {ID: p2}, {ID: p3}}},
		// A squash of two commits is no commit's equivalent.
		{"squash of two", upAll, p3, []gitrepo.CherryCommit{{ID: p1, Equivalent: true}, {ID: p2}, {ID: p3}}},
		{"head is upstream's ancestor", up, base, nil},
	} {
		got, err := repo.Cherry(ctx, c.upstream, c.head)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: Cherry = %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
}

func TestSubjects_ListsTheSubjectLinesNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, dir := bareRepo(t)
	var ids []string
	parent := []string{}
	for _, msg := range []string{"one", "two", "three (#3)\n\nwith a body", "four", "five"} {
		id := gitrepotest.Commit(t, dir, msg, map[string]string{"f.txt": msg}, parent...)
		ids, parent = append(ids, id), []string{id}
	}
	for _, c := range []struct {
		name     string
		from, to string
		limit    int
		want     []string
	}{
		{"everything the commit reaches", "", ids[4], 0, []string{"five", "four", "three (#3)", "two", "one"}},
		{"the newest two", "", ids[4], 2, []string{"five", "four"}},
		{"a range", ids[1], ids[4], 0, []string{"five", "four", "three (#3)"}},
		{"a range cut", ids[1], ids[4], 1, []string{"five"}},
		{"an empty range", ids[4], ids[4], 0, nil},
	} {
		got, err := repo.Subjects(ctx, c.from, c.to, c.limit)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: Subjects = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func TestCherryAndSubjects_TakeFullIDsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, dir := bareRepo(t)
	id := gitrepotest.Commit(t, dir, "one", map[string]string{"f.txt": "1"})
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/main", id)
	for _, rev := range []string{"main", "--output=/tmp/x", id[:12], "-" + id[1:]} {
		if _, err := repo.Cherry(ctx, rev, id); !errors.Is(err, gitrepo.ErrNotFound) {
			t.Errorf("Cherry(%q, id) = %v, want ErrNotFound", rev, err)
		}
		if _, err := repo.Cherry(ctx, id, rev); !errors.Is(err, gitrepo.ErrNotFound) {
			t.Errorf("Cherry(id, %q) = %v, want ErrNotFound", rev, err)
		}
		if _, err := repo.Subjects(ctx, rev, id, 0); !errors.Is(err, gitrepo.ErrNotFound) {
			t.Errorf("Subjects(%q, id) = %v, want ErrNotFound", rev, err)
		}
		if _, err := repo.Subjects(ctx, "", rev, 0); !errors.Is(err, gitrepo.ErrNotFound) {
			t.Errorf("Subjects(\"\", %q) = %v, want ErrNotFound", rev, err)
		}
	}
}
