package gitrepo_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// threeCommits is a repository whose main holds three commits, oldest
// first, opened as a Repo.
func threeCommits(t *testing.T) (repo *gitrepo.Repo, c1, c2, c3 string) {
	t.Helper()
	o := gitrepotest.NewOrigin(t)
	c1, c2, c3 = o.Commit("main", "one"), o.Commit("main", "two"), o.Commit("main", "three")
	repo, err := gitrepo.Git{}.Open(context.Background(), o.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return repo, c1, c2, c3
}

func TestCompareAndSwapRef(t *testing.T) {
	t.Parallel()
	repo, c1, c2, c3 := threeCommits(t)
	ctx := context.Background()
	const ref = "refs/heads/b"
	cases := []struct {
		name              string
		at, newID, expect string // "" is no ref
		locked            bool
		wantErr           error
		want              string
	}{
		{"expected old equals current", c1, c2, c1, false, nil, c2},
		{"expected old differs from current", c1, c3, c2, false, gitrepo.ErrRefMoved, c1},
		{"expected absent and ref absent", "", c1, "", false, nil, c1},
		{"expected absent and ref exists", c1, c2, "", false, gitrepo.ErrRefMoved, c1},
		{"another writer holds the ref", c1, c2, c1, true, gitrepo.ErrRefMoved, c1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.at == "" {
				gitrepotest.Git(t, repo.Dir(), "update-ref", "-d", ref)
			} else {
				gitrepotest.Git(t, repo.Dir(), "update-ref", ref, c.at)
			}
			if c.locked {
				lock := filepath.Join(repo.Dir(), ref+".lock")
				if err := os.WriteFile(lock, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(lock)
			}
			err := repo.CompareAndSwapRef(ctx, ref, c.newID, c.expect)
			if !errors.Is(err, c.wantErr) {
				t.Errorf("CompareAndSwapRef = %v, want %v", err, c.wantErr)
			}
			if got := gitrepotest.Git(t, repo.Dir(), "for-each-ref", "--format=%(objectname)", ref); got != c.want {
				t.Errorf("%s = %q, want %s", ref, got, c.want)
			}
		})
	}
}

func TestCompareAndSwapRef_NameInTheWayOfAnotherRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct {
		name          string
		existing, ref string
	}{
		{"the new name is below an existing ref", "refs/heads/feat", "refs/heads/feat/x"},
		{"an existing ref is below the new name", "refs/heads/topic/a", "refs/heads/topic"},
		{"a tag below an existing tag", "refs/tags/v1", "refs/tags/v1/rc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo, c1, c2, _ := threeCommits(t)
			gitrepotest.Git(t, repo.Dir(), "update-ref", c.existing, c1)
			err := repo.CompareAndSwapRef(ctx, c.ref, c2, "")
			if !errors.Is(err, gitrepo.ErrRefNameConflict) || errors.Is(err, gitrepo.ErrRefMoved) {
				t.Errorf("CompareAndSwapRef = %v, want an error matching only ErrRefNameConflict", err)
			}
			got := gitrepotest.Refs(t, repo.Dir(), "")
			if _, created := got[c.ref]; got[c.existing] != c1 || created {
				t.Errorf("refs = %v, want %s at %s and no %s", got, c.existing, c1, c.ref)
			}
		})
	}
}

func TestIsAncestor(t *testing.T) {
	t.Parallel()
	repo, c1, c2, c3 := threeCommits(t)
	for _, c := range []struct {
		name                 string
		ancestor, descendant string
		want                 bool
	}{
		{"an older commit", c1, c3, true},
		{"a newer commit", c3, c2, false},
		{"the commit itself", c2, c2, true},
	} {
		got, err := repo.IsAncestor(context.Background(), c.ancestor, c.descendant)
		if err != nil || got != c.want {
			t.Errorf("%s: IsAncestor = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

func TestSetRef_PointsTheRefAtTheCommitWhateverItHeld(t *testing.T) {
	t.Parallel()
	repo, c1, c2, _ := threeCommits(t)
	ctx := context.Background()
	const ref = "refs/parley/changes/1/1"
	for _, id := range []string{c1, c2} {
		if err := repo.SetRef(ctx, ref, id); err != nil {
			t.Fatalf("SetRef(%s): %v", id, err)
		}
		if got := gitrepotest.Refs(t, repo.Dir(), "")[ref]; got != id {
			t.Errorf("after SetRef(%s), %s = %q", id, ref, got)
		}
	}
}

func TestDeleteRef_RemovesTheRefAndAcceptsOneThatIsGone(t *testing.T) {
	t.Parallel()
	repo, c1, _, _ := threeCommits(t)
	ctx := context.Background()
	const ref, other = "refs/parley/changes/1/1", "refs/parley/changes/1/2"
	for _, r := range []string{ref, other} {
		if err := repo.SetRef(ctx, r, c1); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := repo.DeleteRef(ctx, ref); err != nil {
			t.Fatalf("DeleteRef(%s): %v", ref, err)
		}
	}
	if got := gitrepotest.Refs(t, repo.Dir(), "refs/parley/changes/"); !reflect.DeepEqual(got, map[string]string{"1/2": c1}) {
		t.Errorf("refs after DeleteRef = %v, want only %s", got, other)
	}
}

// A commit made in a fork is not in its parent's repository: SetRefFrom
// brings it there under the ref, and points the ref at a commit the parent
// already holds without copying anything.
func TestSetRefFrom_BringsACommitOfAnotherRepositoryUnderTheRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	parent, head, _, _ := forkParent(t, 2)
	fork := newFork(t, parent)
	own := gitrepotest.PushCommit(t, parent, fork, "work", "only in the fork\n")
	if readable(parent, own) {
		t.Fatal("the parent holds the fork's commit before the copy")
	}
	repo, err := gitrepo.Git{}.Open(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ ref, id string }{
		{"refs/parley/changes/1/1", own},
		{"refs/parley/changes/2/1", head},
		{"refs/parley/changes/1/1", head},
	} {
		if err := repo.SetRefFrom(ctx, fork, c.ref, c.id); err != nil {
			t.Fatalf("SetRefFrom(%s, %s): %v", c.ref, c.id, err)
		}
		if got := gitrepotest.Refs(t, parent, "")[c.ref]; got != c.id {
			t.Errorf("after SetRefFrom, %s = %q, want %s", c.ref, got, c.id)
		}
	}
	if !readable(parent, own) {
		t.Error("the parent cannot read the fork's commit after the copy")
	}
}
