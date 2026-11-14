package gitrepo_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// copyOf is a bare copy at a new place holding the branches and the tags of
// origin o, as a clone leaves them.
func copyOf(t *testing.T, o *gitrepotest.Origin) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	gitrepotest.Git(t, dir, "fetch", "--quiet", o.Dir(), "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*")
	return dir
}

// on points branch of the bare copy dir at a new commit on top of from and
// returns its id.
func on(t *testing.T, dir, branch, from string) string {
	t.Helper()
	id := gitrepotest.Git(t, dir, "commit-tree", from+"^{tree}", "-p", from, "-m", "hub on "+branch)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/"+branch, id)
	return id
}

func TestPushBranches_OffersEachBranchAsAFastForward(t *testing.T) {
	t.Parallel()
	o := gitrepotest.SmartOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", "s3cret"))
	mainHead, devHead, _ := o.History()
	o.Branch("stale", "dev")
	dir := copyOf(t, o)
	ahead := on(t, dir, "main", on(t, dir, "main", mainHead))
	feat := on(t, dir, "feat", mainHead)
	// Not a descendant of origin's stale, which is dev.
	stale := on(t, dir, "stale", mainHead)

	g := gitrepo.Git{Env: o.Env()}
	got, err := g.PushBranches(context.Background(), gitrepo.PushBranchesRequest{
		Dir: dir, URL: o.URL(), Credential: gitrepo.Credential{Username: "x-token-auth", Password: "s3cret"},
		Heads: map[string]string{"main": ahead, "feat": feat, "dev": devHead, "stale": stale},
	})
	if err != nil {
		t.Fatalf("PushBranches: %v", err)
	}
	results := map[string]gitrepo.BranchPush{}
	for _, p := range got {
		results[p.Branch] = p
	}
	for branch, want := range map[string]gitrepo.PushResult{"main": gitrepo.PushPushed, "feat": gitrepo.PushPushed, "dev": gitrepo.PushUpToDate} {
		if p := results[branch]; p.Result != want || p.Err != nil {
			t.Errorf("branch %s: %+v, want %s", branch, p, want)
		}
	}
	if p := results["stale"]; p.Result != "" || !errors.Is(p.Err, gitrepo.ErrNonFastForward) || p.Commit != stale {
		t.Errorf("branch stale: %+v, want refused as not a fast-forward of %s", p, stale)
	}
	if len(got) != 4 {
		t.Errorf("%d results, want one for each of the 4 branches", len(got))
	}
}

func TestPushBranches_Failures(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		prepare func(o *gitrepotest.Origin) context.Context
		// refused is the class of the branch's refusal, nil when the push
		// reports no branch and fails with err.
		refused error
		err     error
	}{
		{"origin's hook refuses the update", func(o *gitrepotest.Origin) context.Context {
			o.Reject("closed for pushes")
			return context.Background()
		}, gitrepo.ErrOriginRejected, nil},
		{"origin refuses the credential", func(o *gitrepotest.Origin) context.Context {
			o.Accept("x-token-auth", "another")
			return context.Background()
		}, nil, gitrepo.ErrUnauthenticated},
		{"the context ends before origin answers", func(o *gitrepotest.Origin) context.Context {
			o.Block()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			t.Cleanup(cancel)
			return ctx
		}, nil, context.DeadlineExceeded},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := gitrepotest.SmartOrigin(t)
			mainHead, _, _ := o.History()
			dir := copyOf(t, o)
			ahead := on(t, dir, "main", mainHead)
			ctx := c.prepare(o)
			start := time.Now()
			got, err := gitrepo.Git{Env: o.Env()}.PushBranches(ctx, gitrepo.PushBranchesRequest{
				Dir: dir, URL: o.URL(), Credential: gitrepo.Credential{Username: "x-token-auth", Password: "s3cret"},
				Heads: map[string]string{"main": ahead},
			})
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("PushBranches returned after %v", took)
			}
			if c.refused != nil {
				var pe *gitrepo.PushError
				if err != nil || len(got) != 1 || !errors.Is(got[0].Err, c.refused) || !errors.As(got[0].Err, &pe) ||
					!strings.Contains(pe.Reason, "closed for pushes") {
					t.Fatalf("PushBranches = %+v, %v; want main refused as %v with origin's reason", got, err, c.refused)
				}
			} else if len(got) != 0 || !errors.Is(err, c.err) {
				t.Fatalf("PushBranches = %+v, %v; want no result and an error matching %v", got, err, c.err)
			}
			if head := o.Head("main"); head != mainHead {
				t.Errorf("origin's main is %s, want it unchanged at %s", head, mainHead)
			}
		})
	}
}
