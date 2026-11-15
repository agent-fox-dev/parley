package gitrepo_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// The committer times of the history fixture: c3's is the oldest, though it
// comes after c2.
const tC3, tC1, tC2, tC4 = 1700000000, 1767225600, 1767323045, 1767484800

// historyFixture is main with c1, which adds a.txt, the binary bin.dat and
// old.md; c2, which renames old.md to new.md and changes a.txt; c3, which
// changes nothing; and c4, written by Ann with a signature header and a
// message whose body holds a line that looks like a header. id maps c1 to
// c4 and the blob paths at c1 and c2 (as c1:a.txt) to their ids.
type historyFixture struct {
	repo *gitrepo.Repo
	id   map[string]string
}

func newHistoryFixture(t *testing.T) historyFixture {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	who := func(at int) string {
		return "author A <a@example.com> " + strconv.Itoa(at) + " +0000\ncommitter A <a@example.com> " + strconv.Itoa(at) + " +0000\n"
	}
	stream := "commit refs/heads/main\n" + who(tC1) + data("c1") +
		"M 100644 inline a.txt\n" + data("1\n") + "M 100644 inline bin.dat\n" + data("\x00\x01\n") + "M 100644 inline old.md\n" + data("one\n") +
		"\ncommit refs/heads/main\n" + who(tC2) + data("c2") +
		"D old.md\nM 100644 inline new.md\n" + data("one\n") + "M 100644 inline a.txt\n" + data("1x\n2\n") +
		"\ncommit refs/heads/main\n" + who(tC3) + data("c3") + "\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	c3 := strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "main"))
	c4, err := gitrepotest.Run(dir, "tree "+strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "main^{tree}"))+"\nparent "+c3+"\n"+
		"author Ann <ann@example.com> "+strconv.Itoa(tC4)+" +0100\ncommitter Ann <ann@example.com> "+strconv.Itoa(tC4)+" +0100\n"+
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n abc\n -----END PGP SIGNATURE-----\n\nSubject line\n\ntree deadbeef\nbody\n",
		"hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/main", strings.TrimSpace(c4))
	f := historyFixture{id: map[string]string{}}
	names := []string{"c1", "c2", "c3", "c4", "c1:a.txt", "c1:bin.dat", "c1:old.md", "c2:a.txt"}
	revs := []string{"main~3", "main~2", "main~1", "main", "main~3:a.txt", "main~3:bin.dat", "main~3:old.md", "main~2:a.txt"}
	for i, id := range strings.Fields(gitrepotest.Git(t, dir, append([]string{"rev-parse"}, revs...)...)) {
		f.id[names[i]] = id
	}
	if f.repo, err = (gitrepo.Git{}).Open(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f historyFixture) ids(names ...string) []string {
	ids := []string{}
	for _, n := range names {
		ids = append(ids, f.id[n])
	}
	return ids
}

func TestHistory_KeepsAndPagesTheCommitsAQueryAsksFor(t *testing.T) {
	t.Parallel()
	f := newHistoryFixture(t)
	t2 := time.Unix(tC2, 0)
	for _, c := range []struct {
		name string
		q    gitrepo.HistoryQuery
		want []string
	}{
		{"every commit, newest first", gitrepo.HistoryQuery{}, []string{"c4", "c3", "c2", "c1"}},
		{"a page", gitrepo.HistoryQuery{Skip: 1, Count: 2}, []string{"c3", "c2"}},
		{"since, past an older commit", gitrepo.HistoryQuery{Since: &t2}, []string{"c4", "c2"}},
		{"since within a second", gitrepo.HistoryQuery{Since: new(t2.Add(time.Millisecond))}, []string{"c4"}},
		{"until", gitrepo.HistoryQuery{Until: &t2}, []string{"c3", "c2", "c1"}},
		{"since before 1970", gitrepo.HistoryQuery{Since: new(time.Unix(-100, 0))}, []string{"c4", "c3", "c2", "c1"}},
		{"until before 1970", gitrepo.HistoryQuery{Until: new(time.Unix(-100, 0))}, nil},
		{"a path", gitrepo.HistoryQuery{Path: "new.md"}, []string{"c2"}},
		{"a renamed file followed, paged", gitrepo.HistoryQuery{Path: "new.md", Follow: true, Skip: 1, Count: 1}, []string{"c1"}},
		{"a path that reads as pathspec magic", gitrepo.HistoryQuery{Path: ":(glob)*.md"}, nil},
		{"an author, without regard to case", gitrepo.HistoryQuery{Author: "ANN@"}, []string{"c4"}},
		{"an author that reads as a pattern", gitrepo.HistoryQuery{Author: "a.*"}, nil},
		{"an author spanning the name and the e-mail", gitrepo.HistoryQuery{Author: "n <an"}, nil},
		{"an author holding only the e-mail's bracket", gitrepo.HistoryQuery{Author: "<"}, nil},
		{"an author's name alone", gitrepo.HistoryQuery{Author: "ANN"}, []string{"c4"}},
		{"an author, paged after the filter", gitrepo.HistoryQuery{Author: "a@example", Skip: 1, Count: 1}, []string{"c2"}},
		{"an author on a renamed file followed", gitrepo.HistoryQuery{Path: "new.md", Follow: true, Author: "A@EX"}, []string{"c2", "c1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := f.repo.History(context.Background(), f.id["c4"], c.q)
			ids := []string{}
			for _, cm := range got {
				ids = append(ids, cm.ID)
			}
			if err != nil || !slices.Equal(ids, f.ids(c.want...)) {
				t.Errorf("History(%+v) = %v, %v; want %v", c.q, ids, err, f.ids(c.want...))
			}
		})
	}
}

func TestReadCommits_ReadsEachCommitAsWritten(t *testing.T) {
	t.Parallel()
	f := newHistoryFixture(t)
	ctx := context.Background()
	ann := gitrepo.Signature{Name: "Ann", Email: "ann@example.com", Time: time.Unix(tC4, 0)}
	a := gitrepo.Signature{Name: "A", Email: "a@example.com", Time: time.Unix(tC1, 0)}
	got, err := f.repo.ReadCommits(ctx, []string{f.id["c4"], f.id["c1"]})
	if err != nil || len(got) != 2 {
		t.Fatalf("ReadCommits = %v, %v; want two commits", got, err)
	}
	tree := func(c string) string { return gitrepotest.Git(t, f.repo.Dir(), "rev-parse", f.id[c]+"^{tree}") }
	for i, want := range []gitrepo.Commit{
		{ID: f.id["c4"], Parents: []string{f.id["c3"]}, Tree: strings.TrimSpace(tree("c4")), Author: ann, Committer: ann,
			Message: "Subject line\n\ntree deadbeef\nbody\n"},
		{ID: f.id["c1"], Tree: strings.TrimSpace(tree("c1")), Author: a, Committer: a, Message: "c1"},
	} {
		g := got[i]
		if g.ID != want.ID || !slices.Equal(g.Parents, want.Parents) || g.Tree != want.Tree || g.Message != want.Message ||
			!sameSignature(g.Author, want.Author) || !sameSignature(g.Committer, want.Committer) {
			t.Errorf("commit %d = %+v, want %+v", i, g, want)
		}
	}
	for _, c := range []struct {
		id   string
		want error
	}{{f.id["c1:a.txt"], gitrepo.ErrNotCommit}, {strings.Repeat("0", 40), gitrepo.ErrNotFound}, {"main", gitrepo.ErrNotFound}} {
		if _, err := f.repo.ReadCommits(ctx, []string{c.id}); !errors.Is(err, c.want) {
			t.Errorf("ReadCommits(%q) error = %v, want %v", c.id, err, c.want)
		}
	}
}

func sameSignature(a, b gitrepo.Signature) bool {
	return a.Name == b.Name && a.Email == b.Email && a.Time.Equal(b.Time)
}

func TestCommitStats_CountsEachCommitAgainstItsFirstParent(t *testing.T) {
	t.Parallel()
	f := newHistoryFixture(t)
	ctx := context.Background()
	commits, err := f.repo.ReadCommits(ctx, f.ids("c1", "c2", "c3", "c4"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.repo.CommitStats(ctx, commits)
	want := []gitrepo.DiffStats{{FilesChanged: 3, Insertions: 2}, {FilesChanged: 2, Insertions: 2, Deletions: 1}, {}, {}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("CommitStats = %v, %v; want %v", got, err, want)
	}
	for i, c := range commits {
		from := ""
		if len(c.Parents) > 0 {
			from = c.Parents[0]
		}
		changes, err := f.repo.DiffChanges(ctx, from, c.ID)
		if s := gitrepo.Stats(changes); err != nil || s != want[i] {
			t.Errorf("Stats(DiffChanges(%s)) = %v, %v; want %v", c.ID, s, err, want[i])
		}
	}
}

func TestCommitsBetween_ListsAndCountsTheCommitsOnlyOneSideHas(t *testing.T) {
	t.Parallel()
	f := newCompareFixture(t)
	ctx := context.Background()
	long := strings.Fields(gitrepotest.Git(t, f.dir, "rev-list", "--reverse", "main~1..long"))
	for _, c := range []struct {
		base, head    string
		limit         int
		ahead, behind int
		want          []string
	}{
		{"main", "feature", 0, 2, 1, []string{f.id(t, "feature~1"), f.id(t, "feature")}},
		{"main", "feature", 1, 2, 1, []string{f.id(t, "feature~1")}},
		{"main", "main~1", 0, 0, 1, nil},
		{"main", "long", 200, gitrepotest.LongCommits, 1, long[:200]},
		{"feature", "main", 0, 1, 2, []string{f.id(t, "main")}},
	} {
		ahead, behind, err := f.repo.AheadBehind(ctx, f.id(t, c.base), f.id(t, c.head))
		if err != nil || ahead != c.ahead || behind != c.behind {
			t.Errorf("AheadBehind(%s, %s) = %d, %d, %v; want %d, %d", c.base, c.head, ahead, behind, err, c.ahead, c.behind)
		}
		got, err := f.repo.CommitsBetween(ctx, f.id(t, c.base), f.id(t, c.head), c.limit)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("CommitsBetween(%s, %s, %d) = %v, %v; want %v", c.base, c.head, c.limit, got, err, c.want)
		}
	}
}

func TestCommitsBetween_ListsEveryCommitOfTheHeadForNoBase(t *testing.T) {
	t.Parallel()
	f := newCompareFixture(t)
	want := strings.Fields(gitrepotest.Git(t, f.dir, "rev-list", "--reverse", "--topo-order", "feature"))
	got, err := f.repo.CommitsBetween(context.Background(), "", f.id(t, "feature"), 0)
	if err != nil || len(got) < 2 || !slices.Equal(got, want) {
		t.Errorf("CommitsBetween(\"\", feature, 0) = %v, %v; want %v", got, err, want)
	}
}
