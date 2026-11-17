package gitrepo_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
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
	gitrepotest.InitBare(t, dir, "")
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

func TestFetchRemote_RefusesANameThatIsNotOneRefComponent(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.InitBare(t, dir, "")
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

func TestFetchOriginBranch_FetchesTheOneBranch(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	mainHead, devHead, _ := o.History()
	var runs [][]string
	g := gitrepo.Git{Env: o.Env(), Trace: func(args []string) { runs = append(runs, args) }}
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.InitBare(t, dir, "")
	// A stale origin head of another branch, which the fetch leaves alone.
	gitrepotest.Git(t, dir, "fetch", "--quiet", "--no-tags", o.Dir(), "refs/heads/dev:refs/remotes/origin/main")

	tip, err := g.FetchOriginBranch(ctx, dir, o.URL(), "dev", gitrepo.Credential{}, time.Minute)
	if err != nil || tip != devHead {
		t.Fatalf("FetchOriginBranch(dev) = %q, %v; want %s", tip, err, devHead)
	}
	want := map[string]string{"refs/remotes/origin/main": devHead, "refs/remotes/origin/dev": devHead}
	assertRefs(t, "the copy's refs", gitrepotest.Refs(t, dir, ""), want)
	for _, args := range runs {
		i := slices.Index(args, "--")
		for j, a := range args {
			if (a == o.URL() || strings.Contains(a, "refs/heads/dev")) && (i < 0 || j < i) {
				t.Errorf("git ran with the URL or the branch before the end of its options: %q", args)
			}
			if a == "dev" {
				t.Errorf("git ran with the branch name not in full ref form: %q", args)
			}
		}
	}

	// Origin moves dev backwards: the fetch is forced.
	gitrepotest.Git(t, o.Dir(), "update-ref", "refs/heads/dev", mainHead)
	o.Publish()
	if tip, err := g.FetchOriginBranch(ctx, dir, o.URL(), "dev", gitrepo.Credential{}, time.Minute); err != nil || tip != mainHead {
		t.Fatalf("FetchOriginBranch(dev) after origin moved it back = %q, %v; want %s", tip, err, mainHead)
	}
	want["refs/remotes/origin/dev"] = mainHead

	// A branch origin does not have, and one whose origin head another is
	// in the way of, change nothing.
	if _, err := g.FetchOriginBranch(ctx, dir, o.URL(), "gone", gitrepo.Credential{}, time.Minute); !errors.Is(err, gitrepo.ErrNoSuchBranch) {
		t.Errorf("FetchOriginBranch(gone) = %v, want ErrNoSuchBranch", err)
	}
	if _, err := g.FetchOriginBranch(ctx, dir, o.URL(), "main/x", gitrepo.Credential{}, time.Minute); !errors.Is(err, gitrepo.ErrRefNameConflict) {
		t.Errorf("FetchOriginBranch(main/x) = %v, want ErrRefNameConflict", err)
	}
	assertRefs(t, "the copy's refs after the refusals", gitrepotest.Refs(t, dir, ""), want)
}
