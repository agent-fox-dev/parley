package gitrepo_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestRebaseFromAs_MapsEachCommitToTheCommitItBecame(t *testing.T) {
	t.Parallel()
	f := newRebaseFixture(t)
	committer := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0).In(time.FixedZone("", 3600))}
	const kept, dropped, replayed = "kept", "dropped", "replayed"
	for _, c := range []struct {
		name, base, head, onto string
		// became says, for each commit of base..head, oldest first, what
		// the rebase made of it.
		became []string
	}{
		{"commits not on onto are replayed", "main~1", "authored", "main", []string{replayed, replayed}},
		{"a commit whose change onto holds is dropped", "main~1", "same", "main", []string{dropped, replayed}},
		{"nothing left is the onto commit", "main~1", "noop", "main", []string{dropped}},
		{"commits already on onto are kept", "main~1", "feature", "main~1", []string{kept, kept}},
		{"no commits after the base is the onto commit", "main", "main~1", "main", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			onto := f.id(t, c.onto)
			got, commits, report, err := f.repo.RebaseFromAs(context.Background(), f.id(t, c.base), f.id(t, c.head), onto, committer)
			if err != nil || report != nil || len(commits) != len(c.became) {
				t.Fatalf("RebaseFromAs = %q, %v, %+v, %v; want %d commits", got, commits, report, err, len(c.became))
			}
			// Each commit kept or replayed is on top of the one before it,
			// the first on onto, and the last is the new head.
			last := onto
			for i, rc := range commits {
				if from := f.id(t, c.head+"~"+strconv.Itoa(len(commits)-1-i)); rc.From != from {
					t.Errorf("commit %d is %s, want %s", i, rc.From, from)
				}
				switch c.became[i] {
				case dropped:
					if rc.To != "" {
						t.Errorf("commit %d became %s, want dropped", i, rc.To)
					}
					continue
				case kept:
					if rc.To != rc.From {
						t.Errorf("commit %d became %s, want itself", i, rc.To)
					}
				case replayed:
					if rc.To == "" || rc.To == rc.From {
						t.Errorf("commit %d became %q, want a new commit", i, rc.To)
						continue
					}
					if a, b := show(t, f.dir, rc.From, "%an <%ae> %ad%n%B"), show(t, f.dir, rc.To, "%an <%ae> %ad%n%B"); a != b {
						t.Errorf("commit %d: author and message %q, want %q", i, b, a)
					}
					if got, want := show(t, f.dir, rc.To, "%cn <%ce> %cd"), "Hub <hub@example.org> 1760000000 +0100"; got != want {
						t.Errorf("commit %d: committer %q, want %q", i, got, want)
					}
				}
				if parent := f.id(t, rc.To+"^"); parent != last {
					t.Errorf("commit %d became %s on %s, want it on %s", i, rc.To, parent, last)
				}
				last = rc.To
			}
			if got != last {
				t.Errorf("RebaseFromAs = %s, want the new head %s", got, last)
			}
		})
	}
}

func TestRebaseFromAs_ReportsEachConflictedFileWithItsKindAndSides(t *testing.T) {
	t.Parallel()
	f := newCompareFixture(t)
	for _, c := range []struct {
		onto, head, path, kind string
		// absent are the sides that do not hold the file: base, ours or
		// theirs.
		absent string
	}{
		{"main", "both-edit", "f.txt", gitrepo.ConflictContent, ""},
		{"main", "both-add", "f.txt", gitrepo.ConflictAddAdd, "base"},
		{"main", "head-deletes", "f.txt", gitrepo.ConflictModifyDelete, "theirs"},
		{"trimmed", "base-deletes", "f.txt", gitrepo.ConflictDeleteModify, "ours"},
		{"main", "both-rename", "r.txt", gitrepo.ConflictRename, ""},
		{"main", "both-type", "t.txt", gitrepo.ConflictMode, ""},
		{"main", "both-binary", "bin", gitrepo.ConflictBinary, ""},
		{"main", "both-submodule", "sub", gitrepo.ConflictSubmodule, ""},
	} {
		t.Run(c.head, func(t *testing.T) {
			onto, head := f.id(t, c.onto), f.id(t, c.head)
			base := strings.TrimSpace(gitrepotest.Git(t, f.dir, "merge-base", onto, head))
			got, commits, report, err := f.repo.RebaseFromAs(context.Background(), base, head, onto, gitrepo.HubSignature(rebaseAt))
			if got != "" || commits != nil || err != nil || report == nil {
				t.Fatalf("RebaseFromAs(%s, %s) = %q, %v, %+v, %v; want a conflict alone", c.head, c.onto, got, commits, report, err)
			}
			if report.TargetSHA != onto || report.HeadSHA != head || report.BaseSHA != base || report.CommitSHA != head {
				t.Errorf("the report names target %s, head %s, base %s, commit %s; want %s, %s, %s, %s",
					report.TargetSHA, report.HeadSHA, report.BaseSHA, report.CommitSHA, onto, head, base, head)
			}
			if len(report.Files) != 1 || report.Files[0].Path != c.path || report.Files[0].Kind != c.kind {
				t.Fatalf("the report's files are %+v; want %s conflicting as %s", report.Files, c.path, c.kind)
			}
			file := report.Files[0]
			for _, s := range []struct {
				name, rev string
				side      *gitrepo.ConflictSide
			}{{"base", head + "~1", file.Base}, {"ours", onto, file.Ours}, {"theirs", head, file.Theirs}} {
				switch {
				case s.name == c.absent && s.side != nil:
					t.Errorf("%s is %+v, want null", s.name, s.side)
				case s.name == c.absent:
				case s.side == nil:
					t.Errorf("%s is null, want the file on %s", s.name, s.rev)
				case !reflect.DeepEqual(s.side, f.side(t, s.rev, s.side.Path)):
					t.Errorf("%s is %+v, want %s's %s: %+v", s.name, s.side, s.rev, s.side.Path, f.side(t, s.rev, s.side.Path))
				}
			}
		})
	}
}

func TestRebaseFromAs_FailsWithNeitherAHeadNorAReport(t *testing.T) {
	t.Parallel()
	f := newRebaseFixture(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name string
		ctx  context.Context
		head string
		want error
	}{
		{"an ended context", cancelled, f.id(t, "authored"), context.Canceled},
		{"a head that is no commit", context.Background(), strings.Repeat("0", 40), nil},
		{"a merge commit to replay", context.Background(), f.id(t, "merged"), gitrepo.ErrMergeCommit},
	} {
		got, commits, report, err := f.repo.RebaseFromAs(c.ctx, f.id(t, "main~1"), c.head, f.id(t, "main"), gitrepo.HubSignature(rebaseAt))
		if got != "" || commits != nil || report != nil || err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: RebaseFromAs = %q, %v, %+v, %v; want only the error %v", c.name, got, commits, report, err, c.want)
		}
	}
}

// show is the log format of the commit rev of the repository at dir, with
// raw dates.
func show(t *testing.T, dir, rev, format string) string {
	t.Helper()
	return gitrepotest.Git(t, dir, "log", "-1", "--date=raw", "--format="+format, rev)
}
