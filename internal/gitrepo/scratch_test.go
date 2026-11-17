package gitrepo_test

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// scratchStream is the history of the scratch tests: main holds m1, which
// writes f.txt, and m2, which changes it to "two"; add, present and
// conflict each hold one commit on m1: a1 adds a.txt, p1 makes the change
// of m2 again and c1 changes f.txt otherwise.
const scratchStream = `commit refs/heads/main
mark :1
author Origin <origin@example.invalid> 1500000000 +0000
committer Origin <origin@example.invalid> 1500000000 +0000
data 3
m1
M 100644 inline f.txt
data 5
base

commit refs/heads/main
author Origin <origin@example.invalid> 1500000100 +0000
committer Origin <origin@example.invalid> 1500000100 +0000
data 3
m2
M 100644 inline f.txt
data 4
two

commit refs/heads/add
author Ann <ann@example.invalid> 1600000000 +0200
committer Origin <origin@example.invalid> 1500000200 +0000
data 9
a1

body
from :1
M 100644 inline a.txt
data 2
a

commit refs/heads/present
author Origin <origin@example.invalid> 1500000300 +0000
committer Origin <origin@example.invalid> 1500000300 +0000
data 3
p1
from :1
M 100644 inline f.txt
data 4
two

commit refs/heads/conflict
author Origin <origin@example.invalid> 1500000400 +0000
committer Origin <origin@example.invalid> 1500000400 +0000
data 3
c1
from :1
M 100644 inline f.txt
data 6
three

`

// newScratchRepo is a bare repository with the history stream, opened.
func newScratchRepo(t *testing.T, stream string) compareFixture {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return compareFixture{repo: repo, dir: dir}
}

// lsRemote is what the repository dir advertises to a fetch: each ref
// name with its id.
func lsRemote(t *testing.T, dir string) map[string]string {
	t.Helper()
	refs := map[string]string{}
	for line := range strings.Lines(gitrepotest.Git(t, dir, "ls-remote", dir)) {
		id, name, _ := strings.Cut(strings.TrimSpace(line), "\t")
		refs[name] = id
	}
	return refs
}

func TestScratch_AddsAndRemovesAWorkingTreeThatNoRefPointsTo(t *testing.T) {
	t.Parallel()
	f := newScratchRepo(t, scratchStream)
	ctx := context.Background()
	refs, advertised := gitrepotest.Refs(t, f.dir, ""), lsRemote(t, f.dir)
	m2 := f.id(t, "main")

	s, err := f.repo.AddScratch(ctx, "job-1", m2)
	if err != nil {
		t.Fatalf("AddScratch(job-1, main) = %v", err)
	}
	if want := filepath.Join(f.dir, "parley-rebuild", "job-1"); s.Dir() != want {
		t.Errorf("the scratch working tree is at %s, want %s", s.Dir(), want)
	}
	if b, err := os.ReadFile(filepath.Join(s.Dir(), "f.txt")); err != nil || string(b) != "two\n" {
		t.Errorf("f.txt of the scratch working tree = %q, %v; want main's \"two\\n\"", b, err)
	}
	if head, err := s.Head(ctx); err != nil || head != m2 {
		t.Errorf("Head = %q, %v; want main %s", head, err, m2)
	}
	picked, err := s.Pick(ctx, f.id(t, "add"), gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0)})
	if err != nil || picked.Outcome != gitrepo.PickApplied {
		t.Fatalf("Pick(add) = %+v, %v; want applied", picked, err)
	}
	if got := gitrepotest.Refs(t, f.dir, ""); !maps.Equal(got, refs) {
		t.Errorf("the refs with a scratch working tree = %v, want them as they were, %v", got, refs)
	}
	if got := lsRemote(t, f.dir); !maps.Equal(got, advertised) {
		t.Errorf("the repository advertises %v with a scratch working tree, want %v", got, advertised)
	}

	if err := s.Remove(ctx); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	if _, err := os.Stat(s.Dir()); !os.IsNotExist(err) {
		t.Errorf("the scratch working tree is still there after Remove: %v", err)
	}
	if got := gitrepotest.Git(t, f.dir, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("git worktree list = %q, want the repository alone", got)
	}
	if got := gitrepotest.Refs(t, f.dir, ""); !maps.Equal(got, refs) {
		t.Errorf("the refs after Remove = %v, want them as they were, %v", got, refs)
	}
}

func TestScratch_PickReplaysOneCommitOntoItsHead(t *testing.T) {
	t.Parallel()
	f := newScratchRepo(t, scratchStream)
	ctx := context.Background()
	committer := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0).In(time.FixedZone("", 3600))}
	m2 := f.id(t, "main")
	cases := []struct {
		branch    string
		outcome   gitrepo.PickOutcome
		conflicts []string
	}{
		{"add", gitrepo.PickApplied, nil},
		{"present", gitrepo.PickDropped, nil},
		{"conflict", gitrepo.PickConflicted, []string{"f.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.branch, func(t *testing.T) {
			t.Parallel()
			s, err := f.repo.AddScratch(ctx, "job-"+tc.branch, m2)
			if err != nil {
				t.Fatalf("AddScratch = %v", err)
			}
			t.Cleanup(func() { _ = s.Remove(ctx) })
			from := f.id(t, tc.branch)

			picked, err := s.Pick(ctx, from, committer)
			if err != nil || picked.Outcome != tc.outcome || !reflect.DeepEqual(picked.Conflicts, tc.conflicts) {
				t.Fatalf("Pick(%s) = %+v, %v; want %s with the conflicts %v", tc.branch, picked, err, tc.outcome, tc.conflicts)
			}
			head, err := s.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.outcome != gitrepo.PickApplied {
				if picked.Commit != "" || head != m2 {
					t.Errorf("Pick(%s) made %q and left HEAD at %s, want no commit and HEAD at main %s", tc.branch, picked.Commit, head, m2)
				}
				return
			}
			if head != picked.Commit {
				t.Errorf("HEAD = %s, want the new commit %s", head, picked.Commit)
			}
			if got := show(t, f.dir, picked.Commit, "%P"); got != m2 {
				t.Errorf("the parents of the new commit = %s, want main %s", got, m2)
			}
			if a, b := show(t, f.dir, from, "%an <%ae> %ad%n%B"), show(t, f.dir, picked.Commit, "%an <%ae> %ad%n%B"); a != b {
				t.Errorf("author and message of the new commit = %q, want the original's %q", b, a)
			}
			if got, want := show(t, f.dir, picked.Commit, "%cn <%ce> %cd"), "Hub <hub@example.org> 1760000000 +0100"; got != want {
				t.Errorf("committer of the new commit = %q, want %q", got, want)
			}
			if got := gitrepotest.Git(t, f.dir, "show", picked.Commit+":a.txt"); got != "a" {
				t.Errorf("a.txt of the new commit = %q, want \"a\"", got)
			}
		})
	}
}

func TestScratch_MergeAlwaysMakesAMergeCommit(t *testing.T) {
	t.Parallel()
	f := newScratchRepo(t, scratchStream)
	ctx := context.Background()
	hub := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0).In(time.FixedZone("", 3600))}
	const sig = "Hub <hub@example.org> 1760000000 +0100"
	cases := []struct {
		name string
		// at is where HEAD starts; merges are the branches merged in turn.
		at        string
		merges    []string
		outcome   gitrepo.PickOutcome
		conflicts []string
		// files are files of the last merge commit's tree and their
		// content.
		files map[string]string
	}{
		{"a fast-forward is possible", "main~1", []string{"add"}, gitrepo.PickApplied, nil,
			map[string]string{"f.txt": "base", "a.txt": "a"}},
		{"a real merge, then another", "main", []string{"add", "present"}, gitrepo.PickApplied, nil,
			map[string]string{"f.txt": "two", "a.txt": "a"}},
		{"the change does not merge", "main", []string{"conflict"}, gitrepo.PickConflicted, []string{"f.txt"}, nil},
		{"HEAD already reaches the commit", "main", []string{"main~1"}, gitrepo.PickDropped, nil, nil},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			head := f.id(t, tc.at)
			s, err := f.repo.AddScratch(ctx, fmt.Sprintf("job-%d", i), head)
			if err != nil {
				t.Fatalf("AddScratch = %v", err)
			}
			t.Cleanup(func() { _ = s.Remove(ctx) })
			refs := gitrepotest.Refs(t, f.dir, "")
			var merged gitrepo.Picked
			for _, branch := range tc.merges {
				from := f.id(t, branch)
				msg := "Merge branch '" + branch + "'"
				merged, err = s.Merge(ctx, from, msg, hub, hub)
				if err != nil {
					t.Fatalf("Merge(%s) = %v", branch, err)
				}
				got, err := s.Head(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if merged.Outcome != gitrepo.PickApplied {
					break
				}
				if got != merged.Commit {
					t.Errorf("HEAD after Merge(%s) = %s, want the merge commit %s", branch, got, merged.Commit)
				}
				if want := head + " " + from; show(t, f.dir, merged.Commit, "%P") != want {
					t.Errorf("the parents of the merge of %s = %s, want HEAD before, then the commit: %s", branch, show(t, f.dir, merged.Commit, "%P"), want)
				}
				if got := show(t, f.dir, merged.Commit, "%B"); got != msg {
					t.Errorf("the message of the merge of %s = %q, want %q", branch, got, msg)
				}
				if got := show(t, f.dir, merged.Commit, "%an <%ae> %ad|%cn <%ce> %cd"); got != sig+"|"+sig {
					t.Errorf("author|committer of the merge of %s = %q, want %q for both", branch, got, sig)
				}
				head = merged.Commit
			}
			if merged.Outcome != tc.outcome || !reflect.DeepEqual(merged.Conflicts, tc.conflicts) {
				t.Fatalf("the last Merge = %+v, want %s with the conflicts %v", merged, tc.outcome, tc.conflicts)
			}
			if got := gitrepotest.Refs(t, f.dir, ""); !maps.Equal(got, refs) {
				t.Errorf("the refs after the merges = %v, want them as they were, %v", got, refs)
			}
			if tc.outcome != gitrepo.PickApplied {
				if got, err := s.Head(ctx); err != nil || merged.Commit != "" || got != head {
					t.Errorf("Merge made %q and left HEAD at %s (%v), want no commit and HEAD at %s", merged.Commit, got, err, head)
				}
				return
			}
			for name, content := range tc.files {
				if got := gitrepotest.Git(t, f.dir, "show", merged.Commit+":"+name); got != content {
					t.Errorf("%s of the merge commit = %q, want %q", name, got, content)
				}
			}
		})
	}
}

func TestScratch_RunsNoHookAndNoAttributeOfTheContent(t *testing.T) {
	t.Parallel()
	// Every commit's .gitattributes asks for the union merge driver and the
	// filter mark, whose smudge the repository's configuration defines.
	const stream = `commit refs/heads/main
mark :1
author Origin <origin@example.invalid> 1500000000 +0000
committer Origin <origin@example.invalid> 1500000000 +0000
data 3
m1
M 100644 inline .gitattributes
data 26
* merge=union filter=mark
M 100644 inline f.txt
data 5
base

commit refs/heads/main
author Origin <origin@example.invalid> 1500000100 +0000
committer Origin <origin@example.invalid> 1500000100 +0000
data 3
m2
M 100644 inline f.txt
data 5
ours

commit refs/heads/theirs
author Origin <origin@example.invalid> 1500000200 +0000
committer Origin <origin@example.invalid> 1500000200 +0000
data 3
t1
from :1
M 100644 inline f.txt
data 7
theirs

`
	f := newScratchRepo(t, stream)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "ran")
	touch := "#!/bin/sh\necho \"$0\" >> " + marker + "\n"
	if err := os.MkdirAll(filepath.Join(f.dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"post-checkout", "reference-transaction", "pre-commit", "post-commit", "pre-merge-commit", "prepare-commit-msg", "commit-msg", "post-merge"} {
		if err := os.WriteFile(filepath.Join(f.dir, "hooks", hook), []byte(touch), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitrepotest.Git(t, f.dir, "config", "filter.mark.smudge", "echo smudge >> "+marker+"; cat")

	hub := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0)}
	ops := []struct {
		name string
		run  func(s *gitrepo.Scratch, commit string) (gitrepo.Picked, error)
	}{
		{"Pick", func(s *gitrepo.Scratch, commit string) (gitrepo.Picked, error) { return s.Pick(ctx, commit, hub) }},
		{"Merge", func(s *gitrepo.Scratch, commit string) (gitrepo.Picked, error) {
			return s.Merge(ctx, commit, "Merge branch 'theirs'", hub, hub)
		}},
	}
	for _, op := range ops {
		s, err := f.repo.AddScratch(ctx, "job-"+op.name, f.id(t, "main"))
		if err != nil {
			t.Fatalf("AddScratch = %v", err)
		}
		t.Cleanup(func() { _ = s.Remove(ctx) })
		got, err := op.run(s, f.id(t, "theirs"))
		if err != nil || got.Outcome != gitrepo.PickConflicted || !reflect.DeepEqual(got.Conflicts, []string{"f.txt"}) {
			t.Errorf("%s(theirs) = %+v, %v; want a conflict in f.txt, not the union merge the content asks for", op.name, got, err)
		}
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("a hook or the content's filter ran: %q", b)
	}
}

func TestScratch_ResetUndoesAConflictedReplayOrMerge(t *testing.T) {
	t.Parallel()
	f := newScratchRepo(t, scratchStream)
	ctx := context.Background()
	hub := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0)}
	cases := []struct {
		name string
		run  func(s *gitrepo.Scratch, commit string) (gitrepo.Picked, error)
	}{
		{"a replay", func(s *gitrepo.Scratch, commit string) (gitrepo.Picked, error) { return s.Pick(ctx, commit, hub) }},
		{"a merge", func(s *gitrepo.Scratch, commit string) (gitrepo.Picked, error) {
			return s.Merge(ctx, commit, "Merge branch 'conflict'", hub, hub)
		}},
	}
	m2 := f.id(t, "main")
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name := fmt.Sprintf("job-%d", i)
			s, err := f.repo.AddScratch(ctx, name, m2)
			if err != nil {
				t.Fatalf("AddScratch = %v", err)
			}
			t.Cleanup(func() { _ = s.Remove(ctx) })
			if picked, err := tc.run(s, f.id(t, "conflict")); err != nil || picked.Outcome != gitrepo.PickConflicted {
				t.Fatalf("%s of conflict = %+v, %v; want conflicted", tc.name, picked, err)
			}
			// Something git does not track is left in the tree as well.
			if err := os.WriteFile(filepath.Join(s.Dir(), "stray.txt"), []byte("stray\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := s.Reset(ctx, m2); err != nil {
				t.Fatalf("Reset(main) = %v", err)
			}
			if head, err := s.Head(ctx); err != nil || head != m2 {
				t.Errorf("Head after Reset = %q, %v; want main %s", head, err, m2)
			}
			admin := filepath.Join(f.dir, "worktrees", name)
			for _, state := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD"} {
				if _, err := os.Stat(filepath.Join(admin, state)); !os.IsNotExist(err) {
					t.Errorf("%s of the scratch working tree is still there after Reset: %v", state, err)
				}
			}
			entries, err := os.ReadDir(s.Dir())
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if want := []string{".git", "f.txt"}; !reflect.DeepEqual(names, want) {
				t.Errorf("the scratch working tree holds %q after Reset, want %q", names, want)
			}
			if b, err := os.ReadFile(filepath.Join(s.Dir(), "f.txt")); err != nil || string(b) != "two\n" {
				t.Errorf("f.txt after Reset = %q, %v; want main's \"two\\n\"", b, err)
			}
			// The tree takes the next replay as if nothing had happened.
			picked, err := s.Pick(ctx, f.id(t, "add"), hub)
			if err != nil || picked.Outcome != gitrepo.PickApplied {
				t.Fatalf("Pick(add) after Reset = %+v, %v; want applied", picked, err)
			}
			if got := show(t, f.dir, picked.Commit, "%P"); got != m2 {
				t.Errorf("the parent of the replay after Reset = %s, want main %s", got, m2)
			}
		})
	}
}

func TestScratch_DiscardRemovesEveryLeftover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("leftovers of earlier work", func(t *testing.T) {
		t.Parallel()
		f := newScratchRepo(t, scratchStream)
		hub := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0)}
		m2 := f.id(t, "main")
		if _, err := f.repo.AddScratch(ctx, "job-1", m2); err != nil {
			t.Fatal(err)
		}
		half, err := f.repo.AddScratch(ctx, "job-2", m2)
		if err != nil {
			t.Fatal(err)
		}
		if picked, err := half.Pick(ctx, f.id(t, "conflict"), hub); err != nil || picked.Outcome != gitrepo.PickConflicted {
			t.Fatalf("Pick(conflict) = %+v, %v; want conflicted", picked, err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, gitrepo.ScratchParent, "stray"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		refs := gitrepotest.Refs(t, f.dir, "")

		if err := f.repo.DiscardScratches(ctx); err != nil {
			t.Fatalf("DiscardScratches = %v", err)
		}
		if entries, err := os.ReadDir(filepath.Join(f.dir, gitrepo.ScratchParent)); err != nil && !os.IsNotExist(err) || len(entries) != 0 {
			t.Errorf("%s holds %v (%v) after DiscardScratches, want no entry", gitrepo.ScratchParent, entries, err)
		}
		if got := gitrepotest.Git(t, f.dir, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
			t.Errorf("git worktree list = %q, want the repository alone", got)
		}
		if got := gitrepotest.Refs(t, f.dir, ""); !maps.Equal(got, refs) {
			t.Errorf("the refs after DiscardScratches = %v, want them as they were, %v", got, refs)
		}
		// The names are free again.
		s, err := f.repo.AddScratch(ctx, "job-2", m2)
		if err != nil {
			t.Fatalf("AddScratch(job-2) after DiscardScratches = %v", err)
		}
		_ = s.Remove(ctx)
	})
	t.Run("nothing to discard", func(t *testing.T) {
		t.Parallel()
		f := newScratchRepo(t, scratchStream)
		if err := f.repo.DiscardScratches(ctx); err != nil {
			t.Fatalf("DiscardScratches of a repository without scratch working trees = %v", err)
		}
	})
}

// learnStream is scratchStream with two merge commits on c1: learned merges
// main and resolves f.txt to "resolved", unrelated merges add, which
// touches another file.
const learnStream = scratchStream + `commit refs/heads/learned
author Dev <dev@example.invalid> 1500000500 +0000
committer Dev <dev@example.invalid> 1500000500 +0000
data 8
learned
from refs/heads/conflict
merge refs/heads/main
M 100644 inline f.txt
data 9
resolved

commit refs/heads/unrelated
author Dev <dev@example.invalid> 1500000600 +0000
committer Dev <dev@example.invalid> 1500000600 +0000
data 10
unrelated
from refs/heads/conflict
merge refs/heads/add
M 100644 inline a.txt
data 2
a

`

// rrCache are the names of the entries of the rr-cache of the repository
// dir, the conflicts its rerere recorded.
func rrCache(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "rr-cache"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// inProgress are the files of the scratch working tree name of the
// repository dir that say an operation is in progress there.
func inProgress(t *testing.T, dir, name string) []string {
	t.Helper()
	var left []string
	for _, state := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "MERGE_MSG", "MERGE_RR", "AUTO_MERGE"} {
		if _, err := os.Stat(filepath.Join(dir, "worktrees", name, state)); err == nil {
			left = append(left, state)
		}
	}
	return left
}

func TestScratch_ReplaysARecordedResolution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0)}
	cases := []struct {
		name string
		run  func(s *gitrepo.Scratch, commit string) (gitrepo.Picked, error)
	}{
		{"a replay", func(s *gitrepo.Scratch, commit string) (gitrepo.Picked, error) { return s.Pick(ctx, commit, hub) }},
		{"a merge", func(s *gitrepo.Scratch, commit string) (gitrepo.Picked, error) {
			return s.Merge(ctx, commit, "Merge branch 'conflict'", hub, hub)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newScratchRepo(t, scratchStream)
			m2, conflict := f.id(t, "main"), f.id(t, "conflict")
			s, err := f.repo.AddScratch(ctx, "job-1", m2)
			if err != nil {
				t.Fatalf("AddScratch = %v", err)
			}
			t.Cleanup(func() { _ = s.Remove(ctx) })

			first, err := tc.run(s, conflict)
			if err != nil || first.Outcome != gitrepo.PickConflicted {
				t.Fatalf("%s of conflict = %+v, %v; want conflicted", tc.name, first, err)
			}
			ids := rrCache(t, f.dir)
			if len(ids) != 1 {
				t.Fatalf("the rr-cache holds %q, want the one conflict", ids)
			}
			if want := []gitrepo.RerereConflict{{ID: ids[0], Path: "f.txt"}}; !reflect.DeepEqual(first.Recorded, want) {
				t.Errorf("the conflicts git recorded = %+v, want %+v", first.Recorded, want)
			}
			if ok, err := f.repo.RerereResolved(ids[0]); err != nil || ok {
				t.Errorf("RerereResolved before a resolution = %v, %v; want false", ok, err)
			}
			// The conflict is resolved once by hand, with rerere on.
			if err := os.WriteFile(filepath.Join(s.Dir(), "f.txt"), []byte("resolved\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := gitrepotest.Run(filepath.Join(f.dir, "worktrees", "job-1"), "", "-C", s.Dir(), "-c", "rerere.enabled=true", "rerere"); err != nil {
				t.Fatal(err)
			}
			if ok, err := f.repo.RerereResolved(ids[0]); err != nil || !ok {
				t.Errorf("RerereResolved after the resolution = %v, %v; want true", ok, err)
			}
			if err := s.Reset(ctx, m2); err != nil {
				t.Fatal(err)
			}

			again, err := tc.run(s, conflict)
			if err != nil || again.Outcome != gitrepo.PickApplied || len(again.Conflicts) != 0 {
				t.Fatalf("%s of conflict with a recorded resolution = %+v, %v; want applied", tc.name, again, err)
			}
			if got := gitrepotest.Git(t, f.dir, "show", again.Commit+":f.txt"); got != "resolved" {
				t.Errorf("f.txt of the new commit = %q, want the recorded resolution", got)
			}
			if head, err := s.Head(ctx); err != nil || head != again.Commit {
				t.Errorf("HEAD = %s, %v; want the new commit %s", head, err, again.Commit)
			}
			if left := inProgress(t, f.dir, "job-1"); len(left) != 0 {
				t.Errorf("the scratch working tree still holds %q, want no operation in progress", left)
			}
			if picked, err := s.Pick(ctx, f.id(t, "add"), hub); err != nil || picked.Outcome != gitrepo.PickApplied {
				t.Errorf("Pick(add) after the replay = %+v, %v; want applied", picked, err)
			}
		})
	}
}

func TestScratch_LearnsTheResolutionOfAMergeCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0)}
	cases := []struct {
		merge string
		// learns says the merge resolved the conflict of conflict onto main.
		learns bool
	}{
		{"learned", true},
		{"unrelated", false},
	}
	for _, tc := range cases {
		t.Run(tc.merge, func(t *testing.T) {
			t.Parallel()
			f := newScratchRepo(t, learnStream)
			m2, conflict := f.id(t, "main"), f.id(t, "conflict")
			s, err := f.repo.AddScratch(ctx, "job-1", m2)
			if err != nil {
				t.Fatalf("AddScratch = %v", err)
			}
			t.Cleanup(func() { _ = s.Remove(ctx) })
			first, err := s.Pick(ctx, conflict, hub)
			if err != nil || first.Outcome != gitrepo.PickConflicted || len(first.Recorded) != 1 {
				t.Fatalf("Pick(conflict) = %+v, %v; want conflicted with one recorded conflict", first, err)
			}
			id := first.Recorded[0].ID
			if err := s.Reset(ctx, m2); err != nil {
				t.Fatal(err)
			}
			refs := gitrepotest.Refs(t, f.dir, "")

			learned, err := s.Learn(ctx, f.id(t, tc.merge))
			if err != nil {
				t.Fatalf("Learn(%s) = %v", tc.merge, err)
			}
			var want []gitrepo.RerereConflict
			if tc.learns {
				want = []gitrepo.RerereConflict{{ID: id, Path: "f.txt"}}
			}
			if !reflect.DeepEqual(learned, want) {
				t.Errorf("Learn(%s) learned %+v, want %+v", tc.merge, learned, want)
			}
			if ok, err := f.repo.RerereResolved(id); err != nil || ok != tc.learns {
				t.Errorf("RerereResolved after Learn(%s) = %v, %v; want %v", tc.merge, ok, err, tc.learns)
			}
			if got := gitrepotest.Refs(t, f.dir, ""); !maps.Equal(got, refs) {
				t.Errorf("the refs after Learn = %v, want them as they were, %v", got, refs)
			}
			if left := inProgress(t, f.dir, "job-1"); len(left) != 0 {
				t.Errorf("the scratch working tree still holds %q after Learn, want no operation in progress", left)
			}

			if err := s.Reset(ctx, m2); err != nil {
				t.Fatal(err)
			}
			again, err := s.Pick(ctx, conflict, hub)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.learns {
				if again.Outcome != gitrepo.PickConflicted {
					t.Errorf("Pick(conflict) after Learn(%s) = %+v, want conflicted", tc.merge, again)
				}
				return
			}
			if again.Outcome != gitrepo.PickApplied {
				t.Fatalf("Pick(conflict) after Learn(%s) = %+v, want applied", tc.merge, again)
			}
			if got := gitrepotest.Git(t, f.dir, "show", again.Commit+":f.txt"); got != "resolved" {
				t.Errorf("f.txt of the replay = %q, want the merge commit's \"resolved\"", got)
			}
		})
	}
}
