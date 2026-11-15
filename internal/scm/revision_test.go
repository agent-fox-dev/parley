package scm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/server"
)

// wantRefNotFound asserts that err is the API error ref_not_found, 404.
func wantRefNotFound(t *testing.T, rev string, got scm.Resolved, err error) {
	t.Helper()
	var apiErr *server.Error
	if !errors.As(err, &apiErr) || apiErr.Status() != 404 || apiErr.Type() != "ref_not_found" {
		t.Errorf("Resolve(%q) = %+v, %v, want 404 ref_not_found", rev, got, err)
	}
}

func TestResolve_NamesTheCommitOfEveryAcceptedForm(t *testing.T) {
	t.Parallel()
	w := newRevisionRepo(t)
	r := scm.NewResolver(w.repo, scm.RepoRefs(w.repo), "main")
	for _, c := range []struct {
		rev, want string
		full      bool
	}{
		{"main", w.id["c3"], false},
		{"feature/x", w.id["x"], false},
		{"v1", w.id["c1"], false},
		{"v2", w.id["c3"], false},
		{"refs/heads/main", w.id["c3"], false},
		{"refs/tags/v2", w.id["c3"], false},
		{"refs/parley/changes/12/3", w.id["x"], false},
		// The branch refs/heads/refs/tags/v1 is not what refs/tags/v1 names.
		{"refs/tags/v1", w.id["c1"], false},
		{w.id["c2"], w.id["c2"], true},
		{w.id["c2"][:7], w.id["c2"], false},
		{w.id["gone"], w.id["gone"], true},
	} {
		got, err := r.Resolve(context.Background(), c.rev)
		if err != nil || got != (scm.Resolved{SHA: c.want, Full: c.full}) || len(got.SHA) != 40 {
			t.Errorf("Resolve(%q) = %+v, %v, want %s (full %v)", c.rev, got, err, c.want, c.full)
		}
	}
}

func TestResolve_ReadsTheWorkspaceRefView(t *testing.T) {
	t.Parallel()
	w := newRevisionRepo(t)
	// A fork's view: main tracks its parent's head, here c1 whatever the
	// repository holds, and a tag is recorded as its tag object.
	view := refList{
		{Name: "refs/heads/main", SHA: w.id["c1"], Commit: w.id["c1"], Tracking: true},
		{Name: "refs/tags/v2", SHA: w.id["v2tag"]},
	}
	r := scm.NewResolver(w.repo, view, "main")
	for _, c := range []struct{ rev, want string }{
		{"main", w.id["c1"]},
		{"v2", w.id["c3"]},
		{"", w.id["c1"]},
	} {
		got, err := r.ResolveOptional(context.Background(), c.rev)
		if err != nil || got.SHA != c.want {
			t.Errorf("Resolve(%q) = %+v, %v, want %s", c.rev, got, err, c.want)
		}
	}
	got, err := r.Resolve(context.Background(), "feature/x")
	wantRefNotFound(t, "feature/x", got, err)
}

func TestResolve_RefusesARevisionThatNamesNoOneCommit(t *testing.T) {
	t.Parallel()
	w := newRevisionRepo(t)
	r := scm.NewResolver(w.repo, scm.RepoRefs(w.repo), "main")
	for _, rev := range []string{
		"nope", "dup", w.prefix, w.id["blob"], "main~2", "HEAD", "@{1}", "--all", "-x", "refs/heads/nope", "",
	} {
		got, err := r.Resolve(context.Background(), rev)
		wantRefNotFound(t, rev, got, err)
		if hex := rev == w.prefix || rev == w.id["blob"]; hex || rev == "" {
			continue
		}
		for _, args := range w.git.reset() {
			for _, a := range args {
				if !strings.HasPrefix(a, "--git-dir=") && strings.Contains(a, rev) {
					t.Errorf("Resolve(%q) ran git with the argument %q", rev, a)
				}
			}
		}
	}
}

func TestResolveOptional_AnAbsentRevisionIsTheDefaultBranchHead(t *testing.T) {
	t.Parallel()
	w := newRevisionRepo(t)
	for _, c := range []struct{ defaultBranch, want string }{
		{"trunk", w.id["c1"]},
		{"main", w.id["c3"]},
	} {
		got, err := scm.NewResolver(w.repo, scm.RepoRefs(w.repo), c.defaultBranch).ResolveOptional(context.Background(), "")
		if err != nil || got != (scm.Resolved{SHA: c.want}) {
			t.Errorf("default branch %s: ResolveOptional(\"\") = %+v, %v, want %s", c.defaultBranch, got, err, c.want)
		}
	}
	got, err := scm.NewResolver(w.repo, scm.RepoRefs(w.repo), "nope").ResolveOptional(context.Background(), "")
	wantRefNotFound(t, "", got, err)
}
