package gitrepo_test

import (
	"context"
	"maps"
	"path/filepath"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestPush_SendsOnlyTheOneBranch(t *testing.T) {
	t.Parallel()
	o := gitrepotest.SmartOrigin(t)
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	tree := gitrepotest.Git(t, dir, "mktree")
	a := gitrepotest.Git(t, dir, "commit-tree", tree, "-m", "on a")
	b := gitrepotest.Git(t, dir, "commit-tree", tree, "-p", a, "-m", "on b")
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/a", a)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/b", b)
	gitrepotest.Git(t, dir, "update-ref", "refs/tags/v1", b)

	g := gitrepo.Git{Env: o.Env()}
	got, err := g.Push(context.Background(), gitrepo.PushRequest{Dir: dir, URL: o.URL(), Commit: a, Branch: "a"})
	if err != nil || got != gitrepo.PushPushed {
		t.Fatalf("Push = %q, %v; want pushed", got, err)
	}
	if refs, want := gitrepotest.Refs(t, o.Dir(), ""), map[string]string{"refs/heads/a": a}; !maps.Equal(refs, want) {
		t.Errorf("origin's refs after the push = %v, want %v", refs, want)
	}
}
