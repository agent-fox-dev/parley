package gitrepo_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestFetchRemote_MirrorsTheBranchesUnderTheName(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	mainHead, devHead, _ := o.History()
	var runs [][]string
	g := gitrepo.Git{Env: o.Env(), Trace: func(args []string) { runs = append(runs, args) }}
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	// A ref of the copy's own, which the fetch leaves alone.
	gitrepotest.Git(t, dir, "fetch", "--quiet", "--no-tags", o.Dir(), "refs/heads/main:refs/heads/mine")

	got, err := g.FetchRemote(ctx, dir, o.URL(), "upstream", gitrepo.Credential{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	heads := map[string]string{"main": mainHead, "dev": devHead}
	assertRefs(t, "FetchRemote's branches", got, heads)
	// No tag of origin and no ref of the fetch is left, only the branches
	// below refs/remotes/upstream/.
	assertRefs(t, "the copy's refs", gitrepotest.Refs(t, dir, ""), map[string]string{
		"refs/heads/mine": mainHead, "refs/remotes/upstream/main": mainHead, "refs/remotes/upstream/dev": devHead,
	})
	if out, err := gitrepotest.Run(dir, "", "config", "--local", "--get-regexp", `^remote\.`); err == nil {
		t.Errorf("the copy's configuration has a remote section: %s", out)
	}
	for _, args := range runs {
		if i := slices.Index(args, o.URL()); i >= 0 && (i == 0 || !slices.Contains(args[:i], "--")) {
			t.Errorf("git ran with the URL before the end of its options: %q", args)
		}
	}

	// Origin renames dev to dev/next: the ref of dev is in the way of the
	// one of dev/next until it is removed.
	gitrepotest.Git(t, o.Dir(), "update-ref", "-d", "refs/heads/dev")
	next := o.Branch("dev/next", devHead)
	if _, err := g.FetchRemote(ctx, dir, o.URL(), "upstream", gitrepo.Credential{}, time.Minute); err != nil {
		t.Fatalf("fetch after origin renamed dev to dev/next: %v", err)
	}
	before := gitrepotest.Refs(t, dir, "")
	assertRefs(t, "the copy's refs after the rename", before, map[string]string{
		"refs/heads/mine": mainHead, "refs/remotes/upstream/main": mainHead, "refs/remotes/upstream/dev/next": next,
	})

	// A fetch that fails changes no ref.
	o.Commit("main", "later")
	o.Refuse(http.StatusInternalServerError)
	if _, err := g.FetchRemote(ctx, dir, o.URL(), "upstream", gitrepo.Credential{}, time.Minute); err == nil {
		t.Error("FetchRemote from a failing origin succeeded")
	}
	assertRefs(t, "the copy's refs after a failed fetch", gitrepotest.Refs(t, dir, ""), before)
}

func TestFetchRemote_StopsAtDeadline(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	ln := newSilentListener(t)
	start := time.Now()
	_, err := gitrepo.Git{}.FetchRemote(context.Background(), dir, ln.url, "upstream", gitrepo.Credential{}, 200*time.Millisecond)
	if !errors.Is(err, gitrepo.ErrDeadline) {
		t.Errorf("err = %v, want ErrDeadline", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("returned after %v, want within 2s", d)
	}
	if !ln.quiet(0, 2*time.Second) {
		t.Errorf("a connection to origin is still open 2s after the return: a git process is left")
	}
	assertRefs(t, "the copy's refs after the deadline", gitrepotest.Refs(t, dir, ""), map[string]string{})
}

func TestFetchRemote_RefusesANameThatIsNotOneRefComponent(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	for _, name := range []string{"", "up/stream", "up*", "a:b", ".hidden", ".."} {
		ran := false
		g := gitrepo.Git{Trace: func([]string) { ran = true }}
		if _, err := g.FetchRemote(context.Background(), dir, "https://git.invalid/x.git", name, gitrepo.Credential{}, time.Minute); err == nil {
			t.Errorf("FetchRemote with the name %q succeeded, want an error", name)
		}
		if ran {
			t.Errorf("FetchRemote with the name %q ran git", name)
		}
	}
}
