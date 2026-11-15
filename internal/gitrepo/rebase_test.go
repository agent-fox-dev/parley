package gitrepo_test

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// rebaseAt is the time the rebases of the tests make their commits at.
var rebaseAt = time.Unix(1750000000, 0).UTC()

// newRebaseFixture is a compare fixture with three branches more, each on
// top of main~1: authored, whose two commits, by authors in zones other
// than UTC, add a.txt and then b.txt; same, whose first commit makes f.txt
// what main made it and whose second adds s.txt; and noop, whose one
// commit makes f.txt what main made it.
func newRebaseFixture(t *testing.T) compareFixture {
	t.Helper()
	f := newCompareFixture(t)
	const committer = "committer T <t@example.invalid> 1700000000 +0000\n"
	from := "from " + f.id(t, "main~1") + "\n"
	stream := "commit refs/heads/authored\nauthor Ann <ann@example.invalid> 1600000000 +0200\n" + committer +
		"data 3\na1\n" + from + "M 100644 inline a.txt\ndata 2\na\n\n" +
		"commit refs/heads/authored\nauthor Bob <bob@example.invalid> 1600000100 -0500\n" + committer +
		"data 9\nb1\n\nbody\nM 100644 inline b.txt\ndata 2\nb\n\n" +
		"commit refs/heads/same\n" + committer + "data 3\ns1\n" + from + "M 100644 inline f.txt\ndata 5\nmain\n\n" +
		"commit refs/heads/same\n" + committer + "data 3\ns2\nM 100644 inline s.txt\ndata 2\ns\n\n" +
		"commit refs/heads/noop\n" + committer + "data 3\nn1\n" + from + "M 100644 inline f.txt\ndata 5\nmain\n\n"
	if _, err := gitrepotest.Run(f.dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRebase_ReplaysEachCommitKeepingItsAuthorAndMessage(t *testing.T) {
	t.Parallel()
	f := newRebaseFixture(t)
	refs := gitrepotest.Git(t, f.dir, "for-each-ref")
	head := f.id(t, "authored")
	got, report, err := f.repo.Rebase(context.Background(), f.id(t, "main"), head, rebaseAt)
	if err != nil || report != nil || len(got) != 40 {
		t.Fatalf("Rebase(main, authored) = %q, %+v, %v; want a new head", got, report, err)
	}
	if base := f.id(t, got+"~2"); base != f.id(t, "main") {
		t.Errorf("the new head's second ancestor is %s, want main %s", base, f.id(t, "main"))
	}
	for i, rev := range []string{"~1", ""} {
		old, replayed := head+rev, got+rev
		show := func(rev, format string) string {
			return gitrepotest.Git(t, f.dir, "log", "-1", "--date=raw", "--format="+format, rev)
		}
		if a, b := show(old, "%an <%ae> %ad%n%B"), show(replayed, "%an <%ae> %ad%n%B"); a != b {
			t.Errorf("commit %d: author and message %q, want %q", i, b, a)
		}
		if c, want := show(replayed, "%cn <%ce> %cd"), "parley <parley@localhost> 1750000000 +0000"; c != want {
			t.Errorf("commit %d: committer %q, want %q", i, c, want)
		}
		if a, b := gitrepotest.Git(t, f.dir, "diff", old+"^", old), gitrepotest.Git(t, f.dir, "diff", replayed+"^", replayed); a != b {
			t.Errorf("commit %d: the replayed commit changes\n%s\nwant\n%s", i, b, a)
		}
	}
	if after := gitrepotest.Git(t, f.dir, "for-each-ref"); after != refs {
		t.Errorf("the refs changed from\n%s\nto\n%s", refs, after)
	}
}

func TestRebase_SkipsWhatChangesNothingOnTheNewBase(t *testing.T) {
	t.Parallel()
	f := newRebaseFixture(t)
	main := f.id(t, "main")
	for _, c := range []struct {
		name, onto, head string
		// want is the new head, or "" for one commit on top of onto
		// adding s.txt.
		want string
	}{
		{"a commit that changes nothing is left out", "main", "same", ""},
		{"nothing left is the onto commit", "main", "noop", main},
		{"a head behind onto is the onto commit", "main", "main~1", main},
		{"a head already on onto is kept", "main~1", "feature", f.id(t, "feature")},
	} {
		got, report, err := f.repo.Rebase(context.Background(), f.id(t, c.onto), f.id(t, c.head), rebaseAt)
		if err != nil || report != nil {
			t.Errorf("%s: Rebase(%s, %s) = %q, %+v, %v", c.name, c.onto, c.head, got, report, err)
			continue
		}
		if c.want != "" {
			if got != c.want {
				t.Errorf("%s: Rebase(%s, %s) = %s, want %s", c.name, c.onto, c.head, got, c.want)
			}
			continue
		}
		if parent := f.id(t, got+"^"); parent != main {
			t.Errorf("%s: the new head's parent is %s, want main %s", c.name, parent, main)
		}
		if files := gitrepotest.Git(t, f.dir, "diff", "--name-only", main, got); files != "s.txt" {
			t.Errorf("%s: the new head changes %q on main, want s.txt alone", c.name, files)
		}
	}
}

func TestRebase_ReportsTheConflictAndLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	f := newRebaseFixture(t)
	ctx := context.Background()
	for _, head := range []string{"both-edit", "head-deletes"} {
		refs, files := gitrepotest.Git(t, f.dir, "for-each-ref"), repoFiles(t, f.dir)
		want, err := f.repo.TrialRebase(ctx, f.id(t, "main"), f.id(t, head))
		if err != nil || want == nil {
			t.Fatalf("TrialRebase(main, %s) = %+v, %v; want a conflict", head, want, err)
		}
		got, report, err := f.repo.Rebase(ctx, f.id(t, "main"), f.id(t, head), rebaseAt)
		if got != "" || err != nil || !reflect.DeepEqual(report, want) {
			t.Errorf("Rebase(main, %s) = %q, %+v, %v; want the trial's report %+v", head, got, report, err, want)
		}
		if after := gitrepotest.Git(t, f.dir, "for-each-ref"); after != refs {
			t.Errorf("%s: the refs changed from\n%s\nto\n%s", head, refs, after)
		}
		if after := repoFiles(t, f.dir); !slices.Equal(after, files) {
			t.Errorf("%s: the repository's files outside objects/ were %q, then %q", head, files, after)
		}
	}

	objects := gitrepotest.ObjectCounts(t, f.dir)
	if got, report, err := f.repo.Rebase(ctx, f.id(t, "main"), f.id(t, "merged"), rebaseAt); !errors.Is(err, gitrepo.ErrMergeCommit) || got != "" || report != nil {
		t.Errorf("Rebase(main, merged) = %q, %+v, %v; want ErrMergeCommit", got, report, err)
	}
	if after := gitrepotest.ObjectCounts(t, f.dir); after["count"] != objects["count"] {
		t.Errorf("the refused rebase wrote objects: %v, then %v", objects, after)
	}
}

// repoFiles are the paths of the files of the git directory dir outside
// objects/, such as an index, a lock or a work tree a rebase could leave.
func repoFiles(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() && rel == "objects" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	return paths
}
