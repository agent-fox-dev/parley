package gitrepo_test

import (
	"context"
	"maps"
	"path/filepath"
	"strings"
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
	local := allRefs(t, dir)

	got, err := g.FetchOrigin(ctx, dir, o.URL(), gitrepo.Credential{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	heads := map[string]string{"main": newMain, "dev": devHead, "release/1.x": release, "feat/a": feat}
	assertRefs(t, "Fetched.Branches", got.Branches, heads)
	assertRefs(t, "Fetched.Tags", got.Tags, map[string]string{"v1": v1, "v2": devHead})
	// The branches and tags of the copy are as they were, and no ref but
	// the origin heads was added.
	assertRefs(t, "the copy's refs after the fetch", allRefs(t, dir), withOriginHeads(local, heads))

	gitrepotest.Git(t, o.Dir(), "update-ref", "-d", "refs/heads/feat/a")
	newer := o.Commit("main", "fifth")
	if _, err := g.FetchOrigin(ctx, dir, o.URL(), gitrepo.Credential{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	heads = map[string]string{"main": newer, "dev": devHead, "release/1.x": release}
	assertRefs(t, "the copy's refs after origin deleted feat/a", allRefs(t, dir), withOriginHeads(local, heads))
}

// allRefs maps every ref of the repository at dir to the object id it
// points at.
func allRefs(t *testing.T, dir string) map[string]string {
	t.Helper()
	refs := map[string]string{}
	for line := range strings.Lines(gitrepotest.Git(t, dir, "for-each-ref", "--format=%(refname) %(objectname)")) {
		name, id, _ := strings.Cut(strings.TrimSpace(line), " ")
		refs[name] = id
	}
	return refs
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
