package gitrepo_test

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestFetchOrigin_RecordsOriginHeads(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	mainHead, devHead, v1 := o.History()
	g := gitrepo.Git{Env: o.Env()}
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "copy.git")
	if err := g.Clone(ctx, o.URL(), gitrepo.Credential{}, dir); err != nil {
		t.Fatal(err)
	}
	// Origin moves on after the clone: a commit on main, two new branches
	// and a new tag.
	newMain := o.Commit("main", "fourth")
	release := o.Branch("release/1.x", "dev")
	feat := o.Branch("feat/a", mainHead)
	o.Tag("v2", "dev")
	local := gitrepotest.Refs(t, dir, "")

	got, err := g.FetchOrigin(ctx, dir, o.URL(), gitrepo.Credential{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	heads := map[string]string{"main": newMain, "dev": devHead, "release/1.x": release, "feat/a": feat}
	assertRefs(t, "Fetched.Branches", got.Branches, heads)
	assertRefs(t, "Fetched.Tags", got.Tags, map[string]string{"v1": v1, "v2": devHead})
	// The branches and tags of the copy are as they were, and no ref but
	// the origin heads was added.
	assertRefs(t, "the copy's refs after the fetch", gitrepotest.Refs(t, dir, ""), withOriginHeads(local, heads))

	gitrepotest.Git(t, o.Dir(), "update-ref", "-d", "refs/heads/feat/a")
	newer := o.Commit("main", "fifth")
	if _, err := g.FetchOrigin(ctx, dir, o.URL(), gitrepo.Credential{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	heads = map[string]string{"main": newer, "dev": devHead, "release/1.x": release}
	assertRefs(t, "the copy's refs after origin deleted feat/a", gitrepotest.Refs(t, dir, ""), withOriginHeads(local, heads))

	// Origin renames dev to dev/next: the origin head of dev is in the way
	// of dev/next's until it is removed.
	gitrepotest.Git(t, o.Dir(), "update-ref", "-d", "refs/heads/dev")
	next := o.Branch("dev/next", devHead)
	if _, err := g.FetchOrigin(ctx, dir, o.URL(), gitrepo.Credential{}, time.Minute); err != nil {
		t.Fatalf("fetch after origin renamed dev to dev/next: %v", err)
	}
	heads = map[string]string{"main": newer, "dev/next": next, "release/1.x": release}
	assertRefs(t, "the copy's refs after origin renamed dev", gitrepotest.Refs(t, dir, ""), withOriginHeads(local, heads))
}

func TestFetchOrigin_StopsAtDeadline(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	o.History()
	g := gitrepo.Git{Env: o.Env()}
	dir := filepath.Join(t.TempDir(), "copy.git")
	if err := g.Clone(context.Background(), o.URL(), gitrepo.Credential{}, dir); err != nil {
		t.Fatal(err)
	}
	before := gitrepotest.Refs(t, dir, "")
	ln := newSilentListener(t)
	start := time.Now()
	_, err := g.FetchOrigin(context.Background(), dir, ln.url, gitrepo.Credential{}, 200*time.Millisecond)
	if !errors.Is(err, gitrepo.ErrDeadline) {
		t.Errorf("err = %v, want ErrDeadline", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("returned after %v, want within 2s", d)
	}
	// The connection is held by git's transport helper: it closes only when
	// no process of the fetch is left.
	if !ln.quiet(500*time.Millisecond, 2*time.Second) {
		t.Errorf("a connection to origin is still open 2s after the return: a git process is left")
	}
	assertRefs(t, "the copy's refs after the deadline", gitrepotest.Refs(t, dir, ""), before)
}

// withOriginHeads is refs with the origin head of every branch of heads
// added.
func withOriginHeads(refs, heads map[string]string) map[string]string {
	out := maps.Clone(refs)
	for b, id := range heads {
		out["refs/remotes/origin/"+b] = id
	}
	return out
}
