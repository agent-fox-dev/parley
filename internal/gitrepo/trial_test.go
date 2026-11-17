package gitrepo_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// compareFixture is a repository made from gitrepotest.CompareStream and
// its git directory.
type compareFixture struct {
	repo *gitrepo.Repo
	dir  string
}

func newCompareFixture(t *testing.T) compareFixture {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	if _, err := gitrepotest.Run(dir, gitrepotest.CompareStream(), "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return compareFixture{repo: repo, dir: dir}
}

// id is the full id of the revision rev.
func (f compareFixture) id(t *testing.T, rev string) string {
	t.Helper()
	return strings.TrimSpace(gitrepotest.Git(t, f.dir, "rev-parse", rev))
}

// side is the file path of the revision rev as a side of a conflict, nil
// when rev holds no such file.
func (f compareFixture) side(t *testing.T, rev, path string) *gitrepo.ConflictSide {
	t.Helper()
	out, err := gitrepotest.Run(f.dir, "", "ls-tree", rev, "--", path)
	if err != nil {
		t.Fatal(err)
	}
	if fields := strings.Fields(out); len(fields) == 4 {
		return &gitrepo.ConflictSide{SHA: fields[2], Mode: fields[0], Path: path}
	}
	return nil
}

func TestTrialRebase_ReportsTheFirstCommitThatDoesNotApply(t *testing.T) {
	t.Parallel()
	f := newCompareFixture(t)
	for _, c := range []struct{ onto, head, kind, base string }{
		{"main", "both-edit", gitrepo.ConflictContent, "main~1"},
		{"main", "both-add", gitrepo.ConflictAddAdd, "main~2"},
		{"main", "head-deletes", gitrepo.ConflictModifyDelete, "main~1"},
		{"trimmed", "base-deletes", gitrepo.ConflictDeleteModify, "main~1"},
	} {
		t.Run(c.head, func(t *testing.T) {
			refs := gitrepotest.Git(t, f.dir, "for-each-ref")
			got, err := f.repo.TrialRebase(context.Background(), f.id(t, c.onto), f.id(t, c.head))
			want := &gitrepo.ConflictReport{
				TargetSHA: f.id(t, c.onto), HeadSHA: f.id(t, c.head), BaseSHA: f.id(t, c.base), CommitSHA: f.id(t, c.head),
				Files: []gitrepo.ConflictFile{{Path: "f.txt", Kind: c.kind,
					Base: f.side(t, c.head+"~1", "f.txt"), Ours: f.side(t, c.onto, "f.txt"), Theirs: f.side(t, c.head, "f.txt")}},
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("TrialRebase(%s, %s) = %+v, %v; want %+v", c.onto, c.head, got, err, want)
			}
			if after := gitrepotest.Git(t, f.dir, "for-each-ref"); after != refs {
				t.Errorf("the refs changed from\n%s\nto\n%s", refs, after)
			}
		})
	}
	// The kinds git reports on top of the side each file is on.
	for _, c := range []struct{ head, path, kind string }{
		{"both-binary", "bin", gitrepo.ConflictBinary},
		{"both-rename", "r1.txt", gitrepo.ConflictRename},
		{"both-type", "t.txt", gitrepo.ConflictMode},
		{"both-submodule", "sub", gitrepo.ConflictSubmodule},
	} {
		got, err := f.repo.TrialRebase(context.Background(), f.id(t, "main"), f.id(t, c.head))
		if err != nil || got == nil || len(got.Files) != 1 || got.Files[0].Path != c.path || got.Files[0].Kind != c.kind {
			t.Errorf("TrialRebase(main, %s) = %+v, %v; want %s conflicting as %s", c.head, got, err, c.path, c.kind)
		}
	}
}

func TestTrialRebase_ReplaysEveryCommitThatAppliesAndNoMerge(t *testing.T) {
	t.Parallel()
	f := newCompareFixture(t)
	for _, c := range []struct {
		head string
		err  error
	}{
		{"feature", nil},
		{"main~1", nil},
		{"long", nil},
		{"merged", gitrepo.ErrMergeCommit},
	} {
		refs, objects := gitrepotest.Git(t, f.dir, "for-each-ref"), gitrepotest.ObjectCounts(t, f.dir)
		got, err := f.repo.TrialRebase(context.Background(), f.id(t, "main"), f.id(t, c.head))
		if got != nil || !errors.Is(err, c.err) {
			t.Errorf("TrialRebase(main, %s) = %+v, %v; want no conflict and the error %v", c.head, got, err, c.err)
		}
		if after := gitrepotest.Git(t, f.dir, "for-each-ref"); after != refs {
			t.Errorf("%s: the refs changed from\n%s\nto\n%s", c.head, refs, after)
		}
		if after := gitrepotest.ObjectCounts(t, f.dir); c.err != nil && after["count"] != objects["count"] {
			t.Errorf("%s: the refused rebase wrote objects: %v, then %v", c.head, objects, after)
		}
	}
}

func TestTrialRebase_ReadsNoCommitMessage(t *testing.T) {
	t.Parallel()
	f := newCompareFixture(t)
	var mu sync.Mutex
	var read [][]string
	repo, err := gitrepo.Git{Trace: func(args []string) {
		mu.Lock()
		defer mu.Unlock()
		if slices.Contains(args, "cat-file") && !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--batch-check") }) {
			read = append(read, args)
		}
	}}.Open(context.Background(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, head := range []string{"feature", "merged", "both-edit", "main~1"} {
		if _, err := repo.TrialRebase(context.Background(), f.id(t, "main"), f.id(t, head)); err != nil && !errors.Is(err, gitrepo.ErrMergeCommit) {
			t.Errorf("TrialRebase(main, %s): %v", head, err)
		}
	}
	if len(read) != 0 {
		t.Errorf("the trial rebases read whole objects: %v", read)
	}
}

func TestConflictFiles_DescribesEachGivenPath(t *testing.T) {
	t.Parallel()
	f := newScratchRepo(t, scratchStream)
	ctx := context.Background()
	m1, m2 := f.id(t, "main~1"), f.id(t, "main")
	content := func(path, base, ours, theirs string) gitrepo.ConflictFile {
		cf := gitrepo.ConflictFile{Path: path, Kind: gitrepo.ConflictContent}
		cf.Base, cf.Ours, cf.Theirs = f.side(t, base, path), f.side(t, ours, path), f.side(t, theirs, path)
		return cf
	}
	cases := []struct {
		name   string
		commit string
		paths  []string
		want   []gitrepo.ConflictFile
	}{
		{"a path the commit conflicts on", "conflict", []string{"f.txt"},
			[]gitrepo.ConflictFile{content("f.txt", m1, m2, f.id(t, "conflict"))}},
		{"paths a merge of the commit alone does not report, in order and once each", "add", []string{"f.txt", "a.txt", "f.txt"},
			[]gitrepo.ConflictFile{content("a.txt", m1, m2, f.id(t, "add")), content("f.txt", m1, m2, f.id(t, "add"))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := f.repo.ConflictFiles(ctx, m1, m2, f.id(t, tc.commit), tc.paths)
			if err != nil {
				t.Fatalf("ConflictFiles = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ConflictFiles(main~1, main, %s, %q) =\n%s\nwant\n%s", tc.commit, tc.paths, describeFiles(got), describeFiles(tc.want))
			}
		})
	}
}

// describeFiles shows conflict files for a test's failure.
func describeFiles(files []gitrepo.ConflictFile) string {
	var b strings.Builder
	for _, cf := range files {
		b.WriteString(cf.Path + " " + cf.Kind)
		for _, s := range []*gitrepo.ConflictSide{cf.Base, cf.Ours, cf.Theirs} {
			if s == nil {
				b.WriteString(" -")
				continue
			}
			b.WriteString(" " + s.SHA + ":" + s.Mode + ":" + s.Path)
		}
		b.WriteString("\n")
	}
	return b.String()
}
