package carrypatch_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// noScratchLeft fails the test when the repository dir holds a scratch
// working tree, or a record of one.
func noScratchLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, gitrepo.ScratchParent))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %q, want no entry", gitrepo.ScratchParent, names)
	}
	if got := gitrepotest.Git(t, dir, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("git worktree list = %q, want the repository alone", got)
	}
}

func TestRebuild_ScratchTreeIsAlwaysRemoved(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts carrypatch.RebuildOptions
		// second stores the second patch; prepare readies the attempt.
		second       func(t *testing.T, f *conflictFixture)
		prepare      func(t *testing.T, f *conflictFixture)
		kind, reason string
	}{
		{"completed", continueRebase, func(t *testing.T, f *conflictFixture) { f.addPatch(t, "p2") }, nil, attemptCompleted, ""},
		{"fail_fast conflict", carrypatch.RebuildOptions{FailMode: new("fail_fast")},
			func(t *testing.T, f *conflictFixture) { f.conflictingPatch(t, "p2", "a.txt") }, nil, attemptPermanent, "conflict"},
		{"git_error in the second patch", continueRebase, func(t *testing.T, f *conflictFixture) { f.addPatch(t, "p2") },
			func(t *testing.T, f *conflictFixture) {
				p2 := refIn(f.copy, "refs/heads/p2")
				f.hooks.beforePatch(func(i int) {
					if i == 1 {
						if err := os.Remove(filepath.Join(f.copy, "objects", p2[:2], p2[2:])); err != nil {
							t.Errorf("remove the commit of p2: %v", err)
						}
					}
				})
			}, attemptRetryable, "git_error"},
		{"context cancelled before the second patch", continueRebase, func(t *testing.T, f *conflictFixture) { f.addPatch(t, "p2") },
			func(t *testing.T, f *conflictFixture) {
				f.hooks.beforePatch(func(i int) {
					if i == 1 {
						f.runner.cancelAttempt()
					}
				})
			}, attemptRetryable, "shutdown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startConflictFixture(t, nil)
			f.addPatch(t, "p1")
			tc.second(t, f)
			f.moveOn(t)
			// The scratch working tree exists while the attempt builds.
			during := -1
			if tc.prepare != nil {
				tc.prepare(t, f)
			} else {
				f.hooks.beforePatch(func(i int) {
					if i == 1 {
						entries, _ := os.ReadDir(filepath.Join(f.copy, gitrepo.ScratchParent))
						during = len(entries)
					}
				})
			}

			out, _ := f.attemptWith(t, tc.opts, nil)
			if out.Kind != tc.kind || out.Reason != tc.reason {
				t.Errorf("the attempt = %+v, want %s with reason %q", out, tc.kind, tc.reason)
			}
			if tc.prepare == nil && during != 1 {
				t.Errorf("%s held %d entries before the second patch, want the attempt's scratch working tree", gitrepo.ScratchParent, during)
			}
			noScratchLeft(t, f.copy)
		})
	}
}

func TestRebuild_DiscardsLeftoverScratchTree(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	f.addPatch(t, "p1")
	ctx := context.Background()
	repo, err := (gitrepo.Git{}).Open(ctx, f.copy)
	if err != nil {
		t.Fatal(err)
	}
	// An earlier job stopped half-way through the replay of a commit.
	ours := commitFiles(t, f.copy, f.base, "ours", map[string]string{"x.txt": "ours\n"})
	theirs := commitFiles(t, f.copy, f.base, "theirs", map[string]string{"x.txt": "theirs\n"})
	left, err := repo.AddScratch(ctx, "an-earlier-job", ours)
	if err != nil {
		t.Fatal(err)
	}
	hub := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0)}
	if picked, err := left.Pick(ctx, theirs, hub); err != nil || picked.Outcome != gitrepo.PickConflicted {
		t.Fatalf("Pick(theirs) in the leftover = %+v, %v; want conflicted", picked, err)
	}

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	if r := f.patchResults(t, id); len(r) != 1 || r[0]["status"] != "success" {
		t.Errorf("patch_results = %v, want p1 applied", r)
	}
	if _, err := os.Stat(left.Dir()); !os.IsNotExist(err) {
		t.Errorf("the leftover scratch working tree is still there: %v", err)
	}
	noScratchLeft(t, f.copy)
}
