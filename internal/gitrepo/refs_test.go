package gitrepo_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
