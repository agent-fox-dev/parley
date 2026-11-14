package gitserver_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// forkRemote is the fork's URL with the member's key, as git is given it.
func forkRemote(w *world) string { return w.remote(w.memberKey, "acme/demo--t1.git") }

func TestForkView_AdvertisesEachTrackingRefAtTheParentsValueOfTheRequest(t *testing.T) {
	t.Parallel()
	w := newForkWorld(t, &fakeForkView{})
	parent := gitrepotest.Refs(t, w.repo, "")
	refs := lsRemote(t, forkRemote(w))
	for _, name := range []string{"refs/heads/main", "refs/heads/dev", "refs/tags/v1"} {
		if refs[name] != parent[name] {
			t.Errorf("the fork lists %s at %q, want the parent's %s", name, refs[name], parent[name])
		}
	}
	if refs["refs/heads/work"] != w.work {
		t.Errorf("the fork lists its own work at %q, want %s", refs["refs/heads/work"], w.work)
	}
	next := w.commitOn(t, "main")
	if got := lsRemote(t, forkRemote(w))["refs/heads/main"]; got != next {
		t.Errorf("after main moved in the parent the fork lists main at %s, want %s", got, next)
	}
	gitrepotest.Git(t, w.repo, "update-ref", "-d", "refs/heads/dev")
	gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/late", next)
	refs = lsRemote(t, forkRemote(w))
	for _, name := range []string{"refs/heads/dev", "refs/heads/late"} {
		if _, listed := refs[name]; listed {
			t.Errorf("the fork lists %s, which is no tracking ref the parent has", name)
		}
	}
	if refs["refs/heads/work"] != w.work {
		t.Errorf("the fork's own work moved to %q, want %s", refs["refs/heads/work"], w.work)
	}
	dir := t.TempDir()
	runGit(t, dir, "clone", "--quiet", forkRemote(w), "d")
	if got, _ := clientGit(t, filepath.Join(dir, "d"), nil, "rev-parse", "origin/main"); strings.TrimSpace(got) != next {
		t.Errorf("origin/main of the clone = %s, want %s", got, next)
	}
}

func TestForkView_AdvertisesTheVersionRefsOfTheForksOwnChangesAlone(t *testing.T) {
	t.Parallel()
	view := &fakeForkView{}
	w := newForkWorld(t, view)
	parent := gitrepotest.Refs(t, w.repo, "refs/parley/changes/")
	refs := lsRemote(t, forkRemote(w))
	for _, v := range []string{"7/1", "7/2"} {
		if got := refs["refs/parley/changes/"+v]; got != parent[v] {
			t.Errorf("the fork lists refs/parley/changes/%s at %q, want the parent's %s", v, got, parent[v])
		}
	}
	if _, listed := refs["refs/parley/changes/8/1"]; listed {
		t.Error("the fork lists refs/parley/changes/8/1, a version of another change")
	}
	d := t.TempDir()
	runGit(t, d, "init", "--quiet")
	runGit(t, d, "fetch", forkRemote(w), "refs/parley/changes/7/1")
	view.set(gitserver.ForkRefs{Tracking: []string{"refs/heads/main"}}, nil)
	for name := range lsRemote(t, forkRemote(w)) {
		if strings.HasPrefix(name, "refs/parley/changes/") {
			t.Errorf("the fork lists %s after the view stopped naming its change", name)
		}
	}
}

// The hub's own view names the tracking refs the fork's record holds and no
// change, since the hub has no record of a change's source yet. A fork's
// HEAD names its base branch, as a workspace's names its default branch, so
// that a clone of the fork checks it out and a single-branch (shallow) clone
// takes it.
func TestForkView_TheHubNamesTheTrackingRefsOfTheForksRecord(t *testing.T) {
	t.Parallel()
	w := newForkWorld(t, nil)
	parent := gitrepotest.Refs(t, w.repo, "")
	want := map[string]string{"refs/heads/work": w.work, "HEAD": parent["refs/heads/main"], "symref HEAD": "refs/heads/main"}
	for _, name := range []string{"refs/heads/main", "refs/heads/dev", "refs/tags/v1"} {
		want[name] = parent[name]
	}
	if got := lsRemote(t, forkRemote(w)); !maps.Equal(got, want) {
		t.Errorf("the fork lists %v, want %v", got, want)
	}
}

func TestForkView_ServesAForkAsItsRepositoryHoldsItWithoutAView(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.ForkView = nil })
	}})
	w.addFork(t)
	gitrepotest.Git(t, w.workspaces.RepoPath("demo--t1"), "update-ref", "refs/heads/work", w.head)
	got := lsRemote(t, forkRemote(w))
	if want := map[string]string{"refs/heads/work": w.head}; !maps.Equal(got, want) {
		t.Errorf("the fork lists %v, want %v", got, want)
	}
}

// Every request on the fork asks the view before git runs; when the view
// or the copy fails, none is served.
func TestForkView_RefusesTheForkWhenItsRefsCannotBeBroughtUpToDate(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		fail func(t *testing.T, w *world, view *fakeForkView)
	}{
		{"the view fails", func(_ *testing.T, _ *world, view *fakeForkView) {
			view.set(gitserver.ForkRefs{}, errors.New("view down"))
		}},
		{"the parent's repository is missing", func(t *testing.T, w *world, _ *fakeForkView) {
			if err := os.RemoveAll(w.repo); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			view := &fakeForkView{}
			w := newForkWorld(t, view)
			c.fail(t, w, view)
			for _, g := range gitRequests {
				before := view.questions()
				a := w.call(t, g.method, g.to("acme/demo--t1.git"), basic("x", w.memberKey))
				if a.status != http.StatusInternalServerError || a.body != "internal error\n" {
					t.Errorf("%s = %d %q, want 500 \"internal error\\n\"", g.name, a.status, a.body)
				}
				if view.questions() != before+1 {
					t.Errorf("%s did not ask the fork view", g.name)
				}
			}
			if n := w.runs.n.Load(); n != 0 {
				t.Errorf("git upload-pack ran %d times, want never", n)
			}
		})
	}
}

// stuckCopier holds every copy until release is closed, whatever its
// context says, and tells entered of the first.
type stuckCopier struct{ entered, release chan struct{} }

func (s stuckCopier) CopyRefs(context.Context, string, string, gitrepo.RefCopy) (gitrepo.Copied, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return gitrepo.Copied{}, errors.New("stuck")
}

// A request waits for the copy of another into the same fork no longer than
// its own timeout, which bounds the copy and git together.
func TestForkView_GivesUpWaitingForAStuckCopyAtTheTimeout(t *testing.T) {
	t.Parallel()
	stuck := stuckCopier{entered: make(chan struct{}, 1), release: make(chan struct{})}
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Copier, d.Timeout = stuck, 300*time.Millisecond })
	}})
	t.Cleanup(func() { close(stuck.release) })
	w.addFork(t)
	status := make(chan int, 2)
	get := func() {
		req, _ := http.NewRequest(http.MethodGet, w.URL("/git/acme/demo--t1.git/info/refs?service=git-upload-pack"), nil)
		req.Header.Set("Authorization", basic("x", w.memberKey))
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
			status <- res.StatusCode
		}
	}
	go get()
	<-stuck.entered
	go get()
	select {
	case s := <-status:
		if s != http.StatusInternalServerError {
			t.Errorf("the request behind the stuck copy = %d, want 500", s)
		}
	case <-time.After(3 * time.Second):
		t.Error("the request behind the stuck copy got no answer within 3s")
	}
}

// Requests that come at once each bring the fork's refs up to date without
// getting in each other's way, round after round of a parent that moves.
func TestForkView_BringsTheRefsUpToDateForRequestsThatComeAtOnce(t *testing.T) {
	t.Parallel()
	w := newForkWorld(t, &fakeForkView{})
	for round := range 5 {
		next := w.commitOn(t, "main")
		t.Run(fmt.Sprint("round ", round), func(t *testing.T) {
			for i := range 8 {
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					t.Parallel()
					a := w.call(t, http.MethodGet, "/git/acme/demo--t1.git/info/refs?service=git-upload-pack", basic("x", w.memberKey))
					if a.status != http.StatusOK || !strings.Contains(a.body, next+" refs/heads/main") {
						t.Errorf("ref discovery = %d %q, want 200 listing main at %s", a.status, a.body, next)
					}
				})
			}
		})
	}
}
