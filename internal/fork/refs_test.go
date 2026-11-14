package fork_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/server"
)

func TestRefs_TrackingFollowsParent(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.seedFork(t, "demo--f")
	want := []fork.Branch{
		{Name: "main", SHA: w.head(t, w.parent, "main"), Tracking: true},
		{Name: "release", SHA: w.head(t, w.parent, "release"), Tracking: true},
	}
	if got := w.refs(t, "demo--f").Branches; !slices.Equal(got, want) {
		t.Fatalf("branches = %+v, want %+v", got, want)
	}
	n := w.commit(t, parentSlug, "main", "refs/heads/main^0")
	if b, _ := w.refs(t, "demo--f").Branch("main"); b != (fork.Branch{Name: "main", SHA: n, Tracking: true}) {
		t.Errorf("main after the parent moved = %+v, want tracking at %s", b, n)
	}
	if out := w.git(t, "demo--f", "for-each-ref", "refs/heads"); out != "" {
		t.Errorf("the fork's repository holds branches %q, want none", out)
	}
}

func TestRefs_DeletedAndLaterBranches(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.seedFork(t, "demo--f")
	w.git(t, parentSlug, "branch", "-D", "release")
	if b, ok := w.refs(t, "demo--f").Branch("release"); ok {
		t.Errorf("release after the parent deleted it = %+v, want it absent", b)
	}
	w.git(t, parentSlug, "branch", "release", "main")
	if b, _ := w.refs(t, "demo--f").Branch("release"); b != (fork.Branch{Name: "release", SHA: w.head(t, w.parent, "main"), Tracking: true}) {
		t.Errorf("release after the parent created it again = %+v, want tracking at the parent's main", b)
	}
	w.git(t, parentSlug, "branch", "later", "main")
	if b, ok := w.refs(t, "demo--f").Branch("later"); ok {
		t.Errorf("a branch the parent gained after the fork shows as %+v, want it absent", b)
	}
}

func TestRefs_Tags(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.seedFork(t, "demo--f")
	want := []fork.Tag{{Name: "v1", SHA: w.git(t, parentSlug, "rev-parse", "v1")}}
	if got := w.refs(t, "demo--f").Tags; !slices.Equal(got, want) {
		t.Fatalf("tags = %+v, want %+v", got, want)
	}
	w.git(t, parentSlug, "tag", "v2", "main")
	if tag, ok := w.refs(t, "demo--f").Tag("v2"); ok {
		t.Errorf("a tag the parent gained after the fork shows as %+v, want it absent", tag)
	}
	w.git(t, parentSlug, "tag", "-d", "v1")
	if got := w.refs(t, "demo--f").Tags; len(got) != 0 {
		t.Errorf("tags after the parent deleted v1 = %+v, want none", got)
	}
}

func TestRefs_OwnBranches(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.seedFork(t, "demo--f")
	id := w.commit(t, "demo--f", "task", w.head(t, w.parent, "main"))
	if b, _ := w.refs(t, "demo--f").Branch("task"); b != (fork.Branch{Name: "task", SHA: id}) {
		t.Fatalf("task = %+v, want an own branch at %s", b, id)
	}
	if w.head(t, w.parent, "task") != "" {
		t.Fatal("the fork's own branch task is in the parent")
	}
	w.git(t, parentSlug, "branch", "task", "main")
	if got := named(w.refs(t, "demo--f").Branches, "task"); !slices.Equal(got, []fork.Branch{{Name: "task", SHA: id}}) {
		t.Errorf("task after the parent created a branch of the name = %+v, want one own branch at %s", got, id)
	}
	// The copy of the parent's refs git serving makes in the fork's
	// repository is never an own branch.
	w.git(t, "demo--f", "update-ref", "refs/heads/release", w.head(t, w.parent, "release"))
	w.git(t, "demo--f", "update-ref", "refs/parley/changes/7/1", id)
	v := w.refs(t, "demo--f")
	if got := named(v.Branches, "release"); !slices.Equal(got, []fork.Branch{{Name: "release", SHA: w.head(t, w.parent, "release"), Tracking: true}}) {
		t.Errorf("release with a copy in the fork = %+v, want it once, tracking", got)
	}
	var own []string
	for _, b := range v.Branches {
		if !b.Tracking {
			own = append(own, b.Name)
		}
	}
	if !slices.Equal(own, []string{"task"}) {
		t.Errorf("own branches = %v, want [task]", own)
	}
}

func TestCheckBranchWrite(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.seedFork(t, "demo--f")
	// The rows run in order: each one's setup changes the parent for the
	// rows after it.
	for _, tc := range []struct {
		name, slug, branch string
		setup              []string // git arguments run in the parent first
		refused            bool
	}{
		{name: "tracking branch", slug: "demo--f", branch: "main", refused: true},
		{name: "another tracking branch", slug: "demo--f", branch: "release", refused: true},
		{name: "tracking branch the parent deleted", slug: "demo--f", branch: "release", setup: []string{"branch", "-D", "release"}, refused: true},
		{name: "own branch", slug: "demo--f", branch: "task"},
		{name: "branch the parent gained after the fork", slug: "demo--f", branch: "later", setup: []string{"branch", "later", "main"}},
		{name: "branch of a repository workspace", slug: parentSlug, branch: "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				w.git(t, parentSlug, tc.setup...)
			}
			err := w.forks.CheckBranchWrite(context.Background(), tc.slug, tc.branch)
			if !tc.refused {
				if err != nil {
					t.Errorf("CheckBranchWrite(%s, %s) = %v, want nil", tc.slug, tc.branch, err)
				}
				return
			}
			if e, ok := err.(*server.Error); !ok || e.Status() != http.StatusForbidden || e.Type() != "tracking_branch" {
				t.Errorf("CheckBranchWrite(%s, %s) = %v, want 403 tracking_branch", tc.slug, tc.branch, err)
			}
		})
	}
}

func TestHeadSHA_FollowsBaseBranch(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	p := w.seedFork(t, "demo--f").ForkPoint.SHA
	n := w.commit(t, parentSlug, "main", "refs/heads/main^0")
	if h, err := w.forks.HeadSHA(context.Background(), "demo--f"); err != nil || h == nil || *h != n {
		t.Fatalf("HeadSHA = %v, %v; want %s", h, err, n)
	}
	obj := w.get(t, w.devKey, "/workspaces/demo--f").object(t, http.StatusOK)
	if obj["head_sha"] != n || obj["fork_point"].(map[string]any)["sha"] != p {
		t.Errorf("head_sha, fork_point = %v, %v; want %s and sha %s", obj["head_sha"], obj["fork_point"], n, p)
	}
	w.git(t, parentSlug, "branch", "-m", "main", "aside")
	obj = w.get(t, w.devKey, "/workspaces/demo--f").object(t, http.StatusOK)
	if obj["head_sha"] != nil || obj["fork_point"].(map[string]any)["sha"] != p {
		t.Errorf("after main was deleted: head_sha, fork_point = %v, %v; want null and sha %s", obj["head_sha"], obj["fork_point"], p)
	}
}
