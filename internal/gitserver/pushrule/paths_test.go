package pushrule_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

// pathOutcomes says of each decision of v "" when it is allowed, the path
// it names when it is refused for a path outside the token's paths, and its
// message and error type otherwise.
func pathOutcomes(v pushrule.Verdict) []string {
	var out []string
	for _, d := range v.Decisions {
		switch {
		case d.Allowed:
			out = append(out, "")
		case d.ErrorType == "path_not_allowed" && d.Message == "path "+d.Path+" is outside this token's allowed paths":
			out = append(out, d.Path)
		default:
			out = append(out, fmt.Sprintf("refused: %s (%s)", d.Message, d.ErrorType))
		}
	}
	return out
}

func TestEvaluate_HoldsAPathScopedTokenToItsPathsInEveryNewCommit(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	e := engine(pushrule.EngineDeps{})
	h := []string{r.head}
	c := func(parents []string, ops ...string) string { return commit(t, r.dir, parents, ops...) }
	keep := func(ref, id string) string { gitrepotest.Git(t, r.dir, "update-ref", ref, id); return id }
	push := func(us ...pushrule.Update) pushrule.Request { return pushrule.Request{Updates: us} }
	feat := func(after string) pushrule.Request { return push(move("refs/heads/feat", r.head, after)) }
	for _, tc := range []struct {
		name string
		req  func() pushrule.Request
		want []string
	}{
		{"a commit inside the paths", func() pushrule.Request { return feat(c(h, "M docs/a.md")) }, []string{""}},
		{"a commit also outside them", func() pushrule.Request { return feat(c(h, "M docs/b.md", "M src/main.go")) }, []string{"src/main.go"}},
		{"the older commit first", func() pushrule.Request { return feat(c([]string{c(h, "M zz/late.txt")}, "M aa/first.txt")) }, []string{"zz/late.txt"}},
		{"paths in byte order", func() pushrule.Request { return feat(c(h, "M b.txt", "M a.txt")) }, []string{"a.txt"}},
		{"a rename out of the paths", func() pushrule.Request {
			return feat(c([]string{keep("refs/heads/r1", c(h, "M docs/r1.md"))}, "R docs/r1.md src/r1.md"))
		}, []string{"src/r1.md"}},
		{"a rename into the paths", func() pushrule.Request {
			return feat(c([]string{keep("refs/heads/r2", c(h, "M src/r2.md"))}, "R src/r2.md docs/r2.md"))
		}, []string{"src/r2.md"}},
		{"a root commit on a new branch", func() pushrule.Request {
			return push(create("refs/heads/fresh", c(nil, "M docs/a.md", "M other/b.md")))
		}, []string{"other/b.md"}},
		{"a merge of a commit already in the repository", func() pushrule.Request {
			return feat(c([]string{r.head, keep("refs/heads/s", c(h, "M src/s.go"))}, "M docs/m.md"))
		}, []string{""}},
		{"a rebase onto commits already in the repository", func() pushrule.Request {
			return push(move("refs/heads/feat", r.side, c([]string{keep("refs/heads/next", c(h, "M src/n.go"))}, "M docs/r.md")))
		}, []string{""}},
		{"a fork's push over commits only its parent's refs reach", func() pushrule.Request {
			up := keep("refs/heads/up", c(h, "M src/up.go"))
			dir := filepath.Join(t.TempDir(), "fork.git")
			if err := (gitrepo.Git{}).CreateFork(ctx, r.dir, dir); err != nil {
				t.Fatal(err)
			}
			fork, err := gitrepo.Git{}.Open(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			return pushrule.Request{Workspace: demoFk, Repo: fork, ParentRepo: r.dir,
				Updates: []pushrule.Update{create("refs/heads/task", commit(t, dir, []string{up}, "M docs/f.md"))}}
		}, []string{""}},
		{"an annotated tag of a new commit", func() pushrule.Request {
			tag := unreferenced(t, r.dir, "refs/tags/tmp", "tag tmp\nfrom "+c(h, "M src/t.go")+"\ntagger P <p@example.invalid> now\ndata 0\n")
			return push(create("refs/tags/v1", tag))
		}, []string{"src/t.go"}},
		{"a notes commit", func() pushrule.Request { return push(create("refs/notes/commits", c(nil, "M "+r.head))) }, []string{r.head}},
		{"a delete", func() pushrule.Request { return push(remove("refs/heads/feat", r.head)) }, []string{""}},
		{"a fast-forward to a commit another branch reaches", func() pushrule.Request {
			return feat(keep("refs/heads/other", c(h, "M src/o.go")))
		}, []string{""}},
		{"the second head carrying the commit", func() pushrule.Request {
			return push(create("refs/heads/ok", c(h, "M docs/ok.md")), create("refs/heads/bad", c(h, "M src/bad.go")))
		}, []string{"", "src/bad.go"}},
		{"both heads reaching the commit", func() pushrule.Request {
			bad := c(h, "M src/bad2.go")
			return push(create("refs/heads/a", c([]string{bad}, "M docs/x.md")), create("refs/heads/b", bad))
		}, []string{"src/bad2.go", ""}},
	} {
		req := tc.req()
		if req.Repo == nil {
			req.Workspace, req.Repo = demo, r.view
		}
		req.Caller = docsPAT
		if got := pathOutcomes(e.Evaluate(ctx, req)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEvaluate_HoldsAPathScopedTokenToTheFilesOfTheVersionAnUpdateMakesWithoutNewCommits(t *testing.T) {
	r := newRepo(t)
	mixed := []string{"src/x.go", "docs/a.md", "lib/y.go"}
	for _, tc := range []struct {
		files []string
		found bool
		opens bool
		want  string
	}{
		{mixed, true, false, "lib/y.go"},
		{mixed, true, true, "lib/y.go"},
		{[]string{"docs/a.md"}, true, false, ""},
		{nil, false, false, ""},
	} {
		lookup := &versionFiles{files: tc.files, found: tc.found}
		u := move("refs/heads/feat", r.base, r.head)
		u.OpensChange = tc.opens
		v := engine(pushrule.EngineDeps{VersionFiles: lookup}).Evaluate(context.Background(),
			pushrule.Request{Workspace: demo, Caller: docsPAT, Updates: []pushrule.Update{u}, Repo: r.view})
		if got := pathOutcomes(v); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("version files %q (found %t): %q, want %q", tc.files, tc.found, got, tc.want)
		}
		if want := []string{fmt.Sprintf("demo feat %s %t", r.head, tc.opens)}; !slices.Equal(lookup.asked, want) {
			t.Errorf("version files %q: lookup asked %q, want %q", tc.files, lookup.asked, want)
		}
	}
}

func TestEvaluate_AsksNoCommitQuestionOfACallerWithoutPaths(t *testing.T) {
	r := newRepo(t)
	e := engine(pushrule.EngineDeps{})
	u := move("refs/heads/feat", r.head, commit(t, r.dir, []string{r.head}, "M src/main.go"))
	for _, tc := range []struct {
		caller pushrule.Caller
		want   string
		asked  bool
	}{
		{ownerKey, "", false},
		{adminToken, "", false},
		{ownerPAT, "", false},
		{docsPAT, "src/main.go", true},
	} {
		v := &view{View: r.view}
		got := pathOutcomes(e.Evaluate(context.Background(), pushrule.Request{Workspace: demo, Caller: tc.caller, Updates: []pushrule.Update{u}, Repo: v}))
		if !slices.Equal(got, []string{tc.want}) || (v.asked > 0) != tc.asked {
			t.Errorf("by %s: %q after %d commit questions, want %q (asked %t)", tc.caller.CredentialID, got, v.asked, tc.want, tc.asked)
		}
	}
}
