package gitrepo_test

import (
	"context"
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
	for _, hook := range []string{"post-checkout", "reference-transaction", "pre-commit", "post-commit", "post-merge"} {
		if err := os.WriteFile(filepath.Join(f.dir, "hooks", hook), []byte(touch), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitrepotest.Git(t, f.dir, "config", "filter.mark.smudge", "echo smudge >> "+marker+"; cat")

	s, err := f.repo.AddScratch(ctx, "job-1", f.id(t, "main"))
	if err != nil {
		t.Fatalf("AddScratch = %v", err)
	}
	t.Cleanup(func() { _ = s.Remove(ctx) })
	picked, err := s.Pick(ctx, f.id(t, "theirs"), gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0)})
	if err != nil || picked.Outcome != gitrepo.PickConflicted || !reflect.DeepEqual(picked.Conflicts, []string{"f.txt"}) {
		t.Errorf("Pick(theirs) = %+v, %v; want a conflict in f.txt, not the union merge the content asks for", picked, err)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("a hook or the content's filter ran: %q", b)
	}
}
