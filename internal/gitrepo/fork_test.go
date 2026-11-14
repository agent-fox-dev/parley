package gitrepo_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// forkParent is a bare repository whose main holds n commits, with the
// branch release one commit off main's last-but-one and the annotated tag v1
// on main's first, written in one run of git fast-import, which keeps fewer
// than 100 objects loose and stores more as a pack. It returns the
// repository's directory and the heads of main, release and v1 (the tag
// object's id).
func forkParent(t *testing.T, n int) (dir, mainHead, release, v1 string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "parent.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	var s strings.Builder
	commit := func(ref string, mark int, from int) {
		msg, content := fmt.Sprintf("commit %d", mark), fmt.Sprintf("content %d\n", mark)
		fmt.Fprintf(&s, "commit %s\nmark :%d\ncommitter P <p@example.invalid> now\ndata %d\n%s\n", ref, mark, len(msg), msg)
		if from > 0 {
			fmt.Fprintf(&s, "from :%d\n", from)
		}
		fmt.Fprintf(&s, "M 100644 inline README\ndata %d\n%s\n", len(content), content)
	}
	for i := 1; i <= n; i++ {
		commit("refs/heads/main", i, i-1)
	}
	commit("refs/heads/release", n+1, max(n-1, 1))
	s.WriteString("tag v1\nfrom :1\ntagger P <p@example.invalid> now\ndata 2\nv1\n")
	if _, err := gitrepotest.Run(dir, s.String(), "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	heads := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main", "release", "v1"))
	return dir, heads[0], heads[1], heads[2]
}

// newFork creates a fork of the repository at parent in a new place and
// returns its directory.
func newFork(t *testing.T, parent string) string {
	t.Helper()
	fork := filepath.Join(t.TempDir(), "fork.git")
	if err := (gitrepo.Git{}).CreateFork(context.Background(), parent, fork); err != nil {
		t.Fatal(err)
	}
	return fork
}

// readable reports whether git cat-file -e finds object id in the
// repository at dir.
func readable(dir, id string) bool {
	_, err := gitrepotest.Run(dir, "", "cat-file", "-e", id)
	return err == nil
}

func TestCreateFork_CopiesNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		commits int
		packed  bool
	}{
		{"a parent of one commit", 1, false},
		{"a parent of 300 commits in a pack", 300, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			parent, _, _, _ := forkParent(t, c.commits)
			before, refs := gitrepotest.ObjectCounts(t, parent), gitrepotest.Refs(t, parent, "")
			if packed := before["in-pack"] != "0"; packed != c.packed {
				t.Fatalf("the parent's objects are %v, want packed %v", before, c.packed)
			}
			fork := newFork(t, parent)
			if got := gitrepotest.ObjectCounts(t, fork); got["count"] != "0" || got["in-pack"] != "0" {
				t.Errorf("the fork's objects = %v, want count 0 and in-pack 0", got)
			}
			if got := gitrepotest.ObjectCounts(t, parent); !maps.Equal(got, before) {
				t.Errorf("the parent's objects after the fork = %v, want %v", got, before)
			}
			assertRefs(t, "the parent's refs after the fork", gitrepotest.Refs(t, parent, ""), refs)
			if got := gitrepotest.Git(t, fork, "rev-parse", "--is-bare-repository"); got != "true" {
				t.Errorf("the fork is bare = %s, want true", got)
			}
		})
	}
}

func TestCreateFork_SharesHistory(t *testing.T) {
	t.Parallel()
	parent, mainHead, release, v1 := forkParent(t, 3)
	fork := newFork(t, parent)
	for _, id := range []string{mainHead, release, v1} {
		if !readable(fork, id) {
			t.Errorf("the parent's %s is not readable in the fork", id)
		}
	}
	if got := strings.Fields(gitrepotest.Git(t, fork, "rev-list", mainHead)); len(got) != 3 {
		t.Errorf("the fork lists %d commits of main, want 3", len(got))
	}
	later := gitrepotest.Git(t, parent, "commit-tree", mainHead+"^{tree}", "-p", mainHead, "-m", "later")
	gitrepotest.Git(t, parent, "update-ref", "refs/heads/main", later)
	if !readable(fork, later) {
		t.Errorf("the parent's commit %s made after the fork is not readable in the fork", later)
	}
}

func TestCreateFork_OwnWorkStaysInFork(t *testing.T) {
	t.Parallel()
	parent, _, _, _ := forkParent(t, 3)
	fork := newFork(t, parent)
	refs := gitrepotest.Refs(t, parent, "")
	// A client holding the parent's main commits one changed file on top of
	// it and pushes the commit to the new branch task of the fork.
	id := gitrepotest.PushCommit(t, parent, fork, "task", "changed\n")

	if got := gitrepotest.Git(t, fork, "rev-parse", "refs/heads/task"); got != id {
		t.Errorf("the fork's task = %s, want %s", got, id)
	}
	assertRefs(t, "the parent's refs after the push", gitrepotest.Refs(t, parent, ""), refs)
	if readable(parent, id) {
		t.Errorf("the commit %s pushed to the fork is readable in the parent", id)
	}
	if got := gitrepotest.ObjectCounts(t, fork)["count"]; got != "3" {
		t.Errorf("the fork holds %s objects, want the 3 pushed (commit, tree, blob)", got)
	}
}

func TestCreateFork_RefusesAndLeavesNothing(t *testing.T) {
	t.Parallel()
	parent, _, _, _ := forkParent(t, 1)
	for _, c := range []struct {
		name   string
		parent string
		taken  bool
	}{
		{"a fork's place that is taken", parent, true},
		{"a parent that is not a repository", t.TempDir(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			fork, kept := filepath.Join(root, "fork.git"), filepath.Join(root, "fork.git", "kept")
			var want []string
			if c.taken {
				want = []string{fork, kept}
				if err := os.Mkdir(fork, 0o700); err != nil || os.WriteFile(kept, nil, 0o600) != nil {
					t.Fatal("cannot take the fork's place")
				}
			}
			err := (gitrepo.Git{}).CreateFork(context.Background(), c.parent, fork)
			if err == nil || c.taken != errors.Is(err, fs.ErrExist) {
				t.Fatalf("CreateFork = %v, want an error (matching fs.ErrExist: %v)", err, c.taken)
			}
			// A star matches a leading dot too, so a temporary directory left
			// behind would show.
			top, _ := filepath.Glob(filepath.Join(root, "*"))
			below, _ := filepath.Glob(filepath.Join(root, "*", "*"))
			if got := append(top, below...); !slices.Equal(got, want) {
				t.Errorf("after the refusal the fork's place holds %v, want %v", got, want)
			}
		})
	}
}
