package gitrepo_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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
	gitrepotest.InitBare(t, dir, "main")
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

// walkStream is main with c1 to c5, side made on c2 with s1 and s2, merged,
// the merge of side into c4, topic on c5 with t1 and t2, whose committer
// time is older than its parent's, and orphan, a root of its own. Every
// message shares a long body, so that a repack stores commits as deltas.
func walkStream() string {
	body := strings.Repeat("A body every commit of the fixture shares, so that git deltifies them.\n", 40)
	var b strings.Builder
	at := 1700000000
	// The commits' zones take turns, so that a zone read from the wrong
	// commit shows.
	zones := []string{"+0100", "-0530", "+1245"}
	commit := func(ref, mark, msg string, from ...string) {
		at += 60
		zone := zones[at/60%len(zones)]
		fmt.Fprintf(&b, "commit refs/heads/%s\nmark :%s\ncommitter T <t@example.invalid> %d %s\ndata %d\n%s\n", ref, mark, at, zone, len(msg+body), msg+body)
		for i, f := range from {
			if i == 0 {
				fmt.Fprintf(&b, "from :%s\n", f)
			} else {
				fmt.Fprintf(&b, "merge :%s\n", f)
			}
		}
		fmt.Fprintf(&b, "M 100644 inline %s.txt\ndata %d\n%s\n\n", mark, len(mark), mark)
	}
	commit("main", "1", "c1\n")
	commit("main", "2", "c2\n", "1")
	commit("main", "3", "c3\n", "2")
	commit("main", "4", "c4\n", "3")
	commit("main", "5", "c5\n", "4")
	commit("side", "11", "s1\n", "2")
	commit("side", "12", "s2\n", "11")
	commit("merged", "13", "merge side\n", "4", "12")
	commit("topic", "21", "t1\n", "5")
	at -= 3600
	commit("topic", "22", "t2\n", "21")
	commit("orphan", "31", "orphan\n")
	return b.String()
}

// walkQuery is a question the walks answer, asked of the repository and of
// git itself, each answer written as one comparable string.
type walkQuery struct {
	name string
	ours func(ctx context.Context, repo *gitrepo.Repo, id map[string]string) string
	git  func(t *testing.T, dir string, id map[string]string) string
}

// walkQueries ask IsAncestor, MergeBase, CommitsBetween, AuthorZone and
// ReadCommits about walkStream's commits.
func walkQueries() []walkQuery {
	var qs []walkQuery
	for _, q := range [][2]string{{"c1", "c5"}, {"c5", "c2"}, {"s1", "m"}, {"s2", "c5"}, {"c3", "c3"}, {"orphan", "t2"}} {
		qs = append(qs, walkQuery{"IsAncestor(" + q[0] + ", " + q[1] + ")",
			func(ctx context.Context, repo *gitrepo.Repo, id map[string]string) string {
				return fmt.Sprint(repo.IsAncestor(ctx, id[q[0]], id[q[1]]))
			},
			func(t *testing.T, dir string, id map[string]string) string {
				_, err := gitrepotest.Run(dir, "", "merge-base", "--is-ancestor", id[q[0]], id[q[1]])
				return fmt.Sprint(err == nil, nil)
			}})
	}
	for _, q := range [][2]string{{"c5", "s2"}, {"t2", "m"}, {"s1", "s2"}, {"c3", "c3"}, {"c5", "orphan"}} {
		qs = append(qs, walkQuery{"MergeBase(" + q[0] + ", " + q[1] + ")",
			func(ctx context.Context, repo *gitrepo.Repo, id map[string]string) string {
				return fmt.Sprint(repo.MergeBase(ctx, id[q[0]], id[q[1]]))
			},
			func(t *testing.T, dir string, id map[string]string) string {
				base, _ := gitrepotest.Run(dir, "", "merge-base", id[q[0]], id[q[1]])
				return fmt.Sprint(base, nil)
			}})
	}
	for _, q := range [][2]string{{"c2", "c5"}, {"", "t2"}, {"s2", "t2"}, {"t2", "c3"}, {"m", "t2"}, {"orphan", "s2"}} {
		qs = append(qs, walkQuery{"CommitsBetween(" + q[0] + ", " + q[1] + ")",
			func(ctx context.Context, repo *gitrepo.Repo, id map[string]string) string {
				return fmt.Sprint(repo.CommitsBetween(ctx, id[q[0]], id[q[1]], 0))
			},
			func(t *testing.T, dir string, id map[string]string) string {
				revs := id[q[1]]
				if q[0] != "" {
					revs = id[q[0]] + ".." + revs
				}
				var ids []string
				ids = append(ids, strings.Fields(gitrepotest.Git(t, dir, "rev-list", "--reverse", "--topo-order", revs))...)
				return fmt.Sprint(ids, nil)
			}})
	}
	for _, name := range []string{"c1", "c2", "t2", "m", "orphan"} {
		qs = append(qs, walkQuery{"AuthorZone(" + name + ")",
			func(ctx context.Context, repo *gitrepo.Repo, id map[string]string) string {
				zone, err := repo.AuthorZone(ctx, id[name])
				if err != nil {
					return fmt.Sprint("", err)
				}
				return fmt.Sprint(time.Unix(0, 0).In(zone).Format("-0700"), nil)
			},
			func(t *testing.T, dir string, id map[string]string) string {
				f := strings.Fields(gitrepotest.Git(t, dir, "log", "-1", "--date=raw", "--format=%ad", id[name]))
				return fmt.Sprint(f[1], nil)
			}})
	}
	read := []string{"t2", "c1", "m"}
	qs = append(qs, walkQuery{"ReadCommits(t2, c1, m)",
		func(ctx context.Context, repo *gitrepo.Repo, id map[string]string) string {
			commits, err := repo.ReadCommits(ctx, []string{id[read[0]], id[read[1]], id[read[2]]})
			var b strings.Builder
			for _, c := range commits {
				fmt.Fprintf(&b, "%s %s %v %s %s %d %q\n", c.ID, c.Tree, c.Parents, c.Committer.Name, c.Committer.Email,
					c.Committer.Time.Unix(), strings.TrimSpace(c.Message))
			}
			return fmt.Sprint(b.String(), err)
		},
		func(t *testing.T, dir string, id map[string]string) string {
			var b strings.Builder
			for _, name := range read {
				f := strings.Split(gitrepotest.Git(t, dir, "show", "-s", "--format=%H%n%T%n%P%n%cn%n%ce%n%ct", id[name]), "\n")
				_, msg, _ := strings.Cut(gitrepotest.Git(t, dir, "cat-file", "commit", id[name]), "\n\n")
				fmt.Fprintf(&b, "%s %s %v %s %s %s %q\n", f[0], f[1], strings.Fields(f[2]), f[3], f[4], f[5], strings.TrimSpace(msg))
			}
			return fmt.Sprint(b.String(), nil)
		}})
	return qs
}

func TestCommitWalks_AnswerAsGitDoesWithoutRunningGit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "repo.git")
	gitrepotest.InitBare(t, dir, "main")
	if _, err := gitrepotest.Run(dir, walkStream(), "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	id := map[string]string{}
	names := []string{"c1", "c2", "c3", "c4", "c5", "s1", "s2", "m", "t1", "t2", "orphan"}
	revs := []string{"main~4", "main~3", "main~2", "main~1", "main", "side~1", "side", "merged", "topic~1", "topic", "orphan"}
	for i, v := range strings.Fields(gitrepotest.Git(t, dir, append([]string{"rev-parse"}, revs...)...)) {
		id[names[i]] = v
	}
	// The fork reads every object through its alternates.
	fork := filepath.Join(root, "fork.git")
	gitrepotest.InitBare(t, fork, "main")
	if err := os.WriteFile(filepath.Join(fork, "objects", "info", "alternates"), []byte(filepath.Join(dir, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []struct {
		name, dir string
		prepare   func() bool
	}{
		// fast-import writes a small import as loose objects.
		{"loose objects", dir, func() bool {
			return strings.Contains(gitrepotest.Git(t, dir, "count-objects", "-v"), "\nin-pack: 0\n")
		}},
		{"a pack holding commits as deltas", dir, func() bool {
			gitrepotest.Git(t, dir, "repack", "-a", "-d", "-f", "-q", "--window=50", "--depth=50")
			gitrepotest.Git(t, dir, "prune-packed")
			packs, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.idx"))
			return len(packs) == 1 && slices.ContainsFunc(strings.Split(gitrepotest.Git(t, dir, "verify-pack", "-v", packs[0]), "\n"),
				func(l string) bool { f := strings.Fields(l); return len(f) == 7 && f[1] == "commit" })
		}},
		{"a fork reading through its alternates", fork, func() bool { return true }},
	} {
		if !stage.prepare() {
			t.Fatalf("%s: the fixture does not hold its objects so", stage.name)
		}
		var calls atomic.Int32
		repo, err := gitrepo.Git{Trace: func([]string) { calls.Add(1) }}.Open(context.Background(), stage.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range walkQueries() {
			want := q.git(t, stage.dir, id)
			calls.Store(0)
			if got := q.ours(context.Background(), repo, id); got != want {
				t.Errorf("%s: %s = %s; git answers %s", stage.name, q.name, got, want)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%s: %s ran %d git processes, want none", stage.name, q.name, n)
			}
		}
	}
}

func TestCommitWalks_LeaveToGitWhatOnlyGitSettles(t *testing.T) {
	t.Parallel()
	who := func(at int) string { return fmt.Sprintf("committer T <t@example.invalid> %d +0000\n", at) }
	// x and y each merge the other's first commit: their merge bases are
	// x1 and y1, and git picks one of them.
	crisscross := "commit refs/heads/x\nmark :1\n" + who(1700000000) + "data 4\nbase\n\n" +
		"commit refs/heads/x\nmark :2\n" + who(1700000060) + "data 2\nx1\nfrom :1\n\n" +
		"commit refs/heads/y\nmark :3\n" + who(1700000120) + "data 2\ny1\nfrom :1\n\n" +
		"commit refs/heads/x\nmark :4\n" + who(1700000180) + "data 2\nx2\nfrom :2\nmerge :3\n\n" +
		"commit refs/heads/y\nmark :5\n" + who(1700000240) + "data 2\ny2\nfrom :3\nmerge :2\n\n"
	for _, c := range []struct {
		name    string
		prepare func(t *testing.T, dir string)
		ours    func(t *testing.T, repo *gitrepo.Repo, dir string) string
		git     func(t *testing.T, dir string) string
	}{
		{"two merge bases", func(t *testing.T, dir string) {
			if _, err := gitrepotest.Run(dir, crisscross, "fast-import", "--quiet"); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, repo *gitrepo.Repo, dir string) string {
			return fmt.Sprint(repo.MergeBase(context.Background(), gitrepotest.Git(t, dir, "rev-parse", "x"), gitrepotest.Git(t, dir, "rev-parse", "y")))
		}, func(t *testing.T, dir string) string {
			return fmt.Sprint(gitrepotest.Git(t, dir, "merge-base", "x", "y"), nil)
		}},
		{"a range with merge commits", func(t *testing.T, dir string) {}, func(t *testing.T, repo *gitrepo.Repo, dir string) string {
			return fmt.Sprint(repo.CommitsBetween(context.Background(), gitrepotest.Git(t, dir, "rev-parse", "main~4"), gitrepotest.Git(t, dir, "rev-parse", "merged"), 0))
		}, func(t *testing.T, dir string) string {
			return fmt.Sprint(strings.Fields(gitrepotest.Git(t, dir, "rev-list", "--reverse", "--topo-order", "main~4..merged")), nil)
		}},
		{"a replace ref", func(t *testing.T, dir string) {
			gitrepotest.Git(t, dir, "replace", gitrepotest.Git(t, dir, "rev-parse", "main~1"), gitrepotest.Git(t, dir, "rev-parse", "orphan"))
		}, func(t *testing.T, repo *gitrepo.Repo, dir string) string {
			ids := strings.Fields(gitrepotest.Git(t, dir, "--no-replace-objects", "rev-parse", "main~4", "main"))
			return fmt.Sprint(repo.IsAncestor(context.Background(), ids[0], ids[1]))
		}, func(t *testing.T, dir string) string {
			ids := strings.Fields(gitrepotest.Git(t, dir, "--no-replace-objects", "rev-parse", "main~4", "main"))
			_, err := gitrepotest.Run(dir, "", "merge-base", "--is-ancestor", ids[0], ids[1])
			return fmt.Sprint(err == nil, nil)
		}},
		// git reads parents only from the lines right after the tree, so a
		// commit whose parent line follows its author line is a root to it.
		{"a parent line where git reads none", func(t *testing.T, dir string) {
			ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main^{tree}", "main"))
			obj := "tree " + ids[0] + "\nauthor T <t@example.invalid> 1700009000 +0000\nparent " + ids[1] +
				"\ncommitter T <t@example.invalid> 1700009000 +0000\n\nodd\n"
			odd, err := gitrepotest.Run(dir, obj, "hash-object", "-t", "commit", "--literally", "-w", "--stdin")
			if err != nil {
				t.Fatal(err)
			}
			gitrepotest.Git(t, dir, "update-ref", "refs/heads/odd", odd)
		}, func(t *testing.T, repo *gitrepo.Repo, dir string) string {
			ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main", "odd"))
			yes, err := repo.IsAncestor(context.Background(), ids[0], ids[1])
			base, berr := repo.MergeBase(context.Background(), ids[0], ids[1])
			between, lerr := repo.CommitsBetween(context.Background(), "", ids[1], 0)
			return fmt.Sprint(yes, err, base, berr, between, lerr)
		}, func(t *testing.T, dir string) string {
			ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main", "odd"))
			_, err := gitrepotest.Run(dir, "", "merge-base", "--is-ancestor", ids[0], ids[1])
			base, _ := gitrepotest.Run(dir, "", "merge-base", ids[0], ids[1])
			return fmt.Sprint(err == nil, nil, base, nil, strings.Fields(gitrepotest.Git(t, dir, "rev-list", "--reverse", "--topo-order", ids[1])), nil)
		}},
		{"a shallow repository", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "shallow"), []byte(gitrepotest.Git(t, dir, "rev-parse", "main~2")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, repo *gitrepo.Repo, dir string) string {
			return fmt.Sprint(repo.CommitsBetween(context.Background(), "", gitrepotest.Git(t, dir, "rev-parse", "main"), 0))
		}, func(t *testing.T, dir string) string {
			return fmt.Sprint(strings.Fields(gitrepotest.Git(t, dir, "rev-list", "--reverse", "--topo-order", "main")), nil)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "repo.git")
			gitrepotest.InitBare(t, dir, "main")
			if _, err := gitrepotest.Run(dir, walkStream(), "fast-import", "--quiet"); err != nil {
				t.Fatal(err)
			}
			c.prepare(t, dir)
			repo, err := gitrepo.Git{}.Open(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := c.ours(t, repo, dir), c.git(t, dir); got != want {
				t.Errorf("got %s; git answers %s", got, want)
			}
		})
	}
}
