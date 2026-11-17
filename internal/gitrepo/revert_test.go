package gitrepo_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// revertAs is the signature the reverts of the tests are made by.
var revertAs = gitrepo.Signature{Name: "Lander", Email: "lander@example.invalid", Time: time.Unix(1760000000, 0).UTC()}

// newRevertFixture is a repository whose main is base (a.txt "l1\nl2\nl3\n"),
// one (a.txt's second line "one") and addb (adding b.txt); on top of addb,
// back sets a.txt back to what base has, and rewrite sets its second line
// to "two".
func newRevertFixture(t *testing.T) compareFixture {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.InitBare(t, dir, "main")
	const committer = "committer T <t@example.invalid> 1700000000 +0000\n"
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	commit := func(ref, mark, from, msg string, files ...string) string {
		s := "commit refs/heads/" + ref + "\nmark :" + mark + "\n" + committer + data(msg)
		if from != "" {
			s += "from :" + from + "\n"
		}
		for i := 0; i < len(files); i += 2 {
			s += "M 100644 inline " + files[i] + "\n" + data(files[i+1])
		}
		return s
	}
	stream := commit("main", "1", "", "base", "a.txt", "l1\nl2\nl3\n") +
		commit("main", "2", "", "Set one\n\nWhy one.\n", "a.txt", "l1\none\nl3\n") +
		commit("main", "3", "", "Add b", "b.txt", "b\n") +
		commit("back", "4", "3", "back", "a.txt", "l1\nl2\nl3\n") +
		commit("rewrite", "5", "3", "rewrite", "a.txt", "l1\ntwo\nl3\n")
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return compareFixture{repo: repo, dir: dir}
}

func TestRevert_UndoesEachCommitNewestFirstWithGitsRevertMessage(t *testing.T) {
	t.Parallel()
	f := newRevertFixture(t)
	refs := gitrepotest.Git(t, f.dir, "for-each-ref")
	one, addb := f.id(t, "main~1"), f.id(t, "main")

	got, report, err := f.repo.Revert(context.Background(), addb, []string{addb, one}, revertAs)

	if err != nil || report != nil || len(got) != 40 {
		t.Fatalf("Revert(main, [addb, one]) = %q, %+v, %v; want a new head", got, report, err)
	}
	if parent := f.id(t, got+"~2"); parent != addb {
		t.Errorf("the head's second ancestor is %s, want main %s", parent, addb)
	}
	for _, c := range []struct{ rev, message string }{
		{got + "~1", "Revert \"Add b\"\n\nThis reverts commit " + addb + ".\n"},
		{got, "Revert \"Set one\"\n\nThis reverts commit " + one + ".\n"},
	} {
		if m := gitrepotest.Git(t, f.dir, "log", "-1", "--format=%B", c.rev); strings.TrimRight(m, "\n") != strings.TrimRight(c.message, "\n") {
			t.Errorf("the message of %s is %q, want %q", c.rev, m, c.message)
		}
		signatures := gitrepotest.Git(t, f.dir, "log", "-1", "--date=raw", "--format=%an <%ae> %ad|%cn <%ce> %cd", c.rev)
		if want := "Lander <lander@example.invalid> 1760000000 +0000|Lander <lander@example.invalid> 1760000000 +0000"; strings.TrimSpace(signatures) != want {
			t.Errorf("the signatures of %s are %q, want %q", c.rev, signatures, want)
		}
	}
	if tree, want := f.id(t, got+"^{tree}"), f.id(t, "main~2^{tree}"); tree != want {
		t.Errorf("the head's tree is %s, want base's %s", tree, want)
	}
	if after := gitrepotest.Git(t, f.dir, "for-each-ref"); after != refs {
		t.Errorf("the refs changed from\n%s\nto\n%s", refs, after)
	}
}

func TestRevert_LeavesOutAnUndoThatChangesNothing(t *testing.T) {
	t.Parallel()
	f := newRevertFixture(t)
	one, addb, back := f.id(t, "main~1"), f.id(t, "main"), f.id(t, "back")
	for _, c := range []struct {
		name    string
		commits []string
		// undone are the commits undone, newest first, each by one commit.
		undone []string
	}{
		{"every undo changes nothing", []string{one}, nil},
		{"one undo changes nothing", []string{addb, one}, []string{addb}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, report, err := f.repo.Revert(context.Background(), back, c.commits, revertAs)
			if err != nil || report != nil {
				t.Fatalf("Revert(back, %v) = %q, %+v, %v; want no conflict", c.commits, got, report, err)
			}
			if c.undone == nil {
				if got != back {
					t.Errorf("Revert(back, %v) = %s, want back %s", c.commits, got, back)
				}
				return
			}
			log := strings.Fields(gitrepotest.Git(t, f.dir, "rev-list", back+".."+got))
			if len(log) != len(c.undone) {
				t.Fatalf("the revert made %d commits, want %d", len(log), len(c.undone))
			}
			if m := gitrepotest.Git(t, f.dir, "log", "-1", "--format=%b", got); !strings.Contains(m, c.undone[0]) {
				t.Errorf("the commit's body %q does not name %s", m, c.undone[0])
			}
		})
	}
}

func TestRevert_ReportsTheFirstUndoThatDoesNotApply(t *testing.T) {
	t.Parallel()
	f := newRevertFixture(t)
	refs := gitrepotest.Git(t, f.dir, "for-each-ref")
	one, addb, rewrite := f.id(t, "main~1"), f.id(t, "main"), f.id(t, "rewrite")

	got, report, err := f.repo.Revert(context.Background(), rewrite, []string{addb, one}, revertAs)

	if err != nil || got != "" || report == nil {
		t.Fatalf("Revert(rewrite, [addb, one]) = %q, %+v, %v; want a conflict report", got, report, err)
	}
	if report.TargetSHA != rewrite || report.CommitSHA != one || report.HeadSHA != addb {
		t.Errorf("the report names target %s, commit %s, head %s; want %s, %s, %s",
			report.TargetSHA, report.CommitSHA, report.HeadSHA, rewrite, one, addb)
	}
	if len(report.Files) != 1 || report.Files[0].Path != "a.txt" || report.Files[0].Kind != gitrepo.ConflictContent {
		t.Errorf("the report's files are %+v, want a.txt of kind content", report.Files)
	}
	if after := gitrepotest.Git(t, f.dir, "for-each-ref"); after != refs {
		t.Errorf("the refs changed from\n%s\nto\n%s", refs, after)
	}
}
