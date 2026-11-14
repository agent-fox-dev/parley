package gitrepo_test

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
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

func TestPush_ForcesOnlyUnderTheLease(t *testing.T) {
	t.Parallel()
	o := gitrepotest.SmartOrigin(t)
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	tree := gitrepotest.Git(t, dir, "mktree")
	a := gitrepotest.Git(t, dir, "commit-tree", tree, "-m", "a")
	b := gitrepotest.Git(t, dir, "commit-tree", tree, "-p", a, "-m", "b on a")
	x := gitrepotest.Git(t, dir, "commit-tree", tree, "-p", a, "-m", "x on a")
	gitrepotest.Git(t, dir, "push", "--quiet", o.Dir(),
		b+":refs/heads/replaced", a+":refs/heads/ahead", b+":refs/heads/moved", a+":refs/heads/unrecorded")

	var traced [][]string
	g := gitrepo.Git{Env: o.Env(), Trace: func(args []string) { traced = append(traced, args) }}
	for _, c := range []struct {
		name, branch, lease, commit string
		want                        gitrepo.PushResult // "": the push is refused as origin moved
		originAfter                 string             // "": origin lacks the branch
	}{
		{"origin at the lease, which the commit does not descend from", "replaced", b, x, gitrepo.PushForced, x},
		{"a fast-forward from the lease", "ahead", a, b, gitrepo.PushPushed, b},
		{"a branch the lease and origin both lack", "created", "", a, gitrepo.PushPushed, a},
		{"origin moved from the lease", "moved", a, x, "", b},
		{"origin has a branch the lease says it lacks", "unrecorded", "", x, "", a},
		{"origin lacks the branch the lease names", "deleted", a, x, "", ""},
	} {
		traced = nil
		got, err := g.Push(context.Background(), gitrepo.PushRequest{
			Dir: dir, URL: o.URL(), Commit: c.commit, Branch: c.branch, Force: true, Lease: c.lease,
		})
		switch {
		case c.want == "" && !errors.Is(err, gitrepo.ErrOriginMoved):
			t.Errorf("%s: Push = %q, %v; want an error matching ErrOriginMoved", c.name, got, err)
		case c.want != "" && (err != nil || got != c.want):
			t.Errorf("%s: Push = %q, %v; want %s", c.name, got, err, c.want)
		}
		if head := o.Head(c.branch); head != c.originAfter {
			t.Errorf("%s: origin's branch is at %q, want %q", c.name, head, c.originAfter)
		}
		args := slices.Concat(traced...)
		if lease := "--force-with-lease=refs/heads/" + c.branch + ":" + c.lease; !slices.Contains(args, lease) {
			t.Errorf("%s: git ran with %q, without the lease %s", c.name, args, lease)
		}
		for _, arg := range args {
			if arg == "--force" || arg == "-f" || strings.HasPrefix(arg, "+") {
				t.Errorf("%s: git ran with %q, which holds the plain force %q", c.name, args, arg)
			}
		}
	}
}
