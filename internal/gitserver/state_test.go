package gitserver_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestState_RefusesAWorkspaceThatIsNotReady(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	states := []struct {
		name, status string
		marker       workspace.Marker
		want         string
	}{
		{"pending", access.StatusPending, "", "pending"},
		{"cloning", access.StatusCloning, "", "cloning"},
		{"failed", access.StatusFailed, "", "failed"},
		{"archived", access.StatusArchived, "", "archived"},
		{"maintenance marker", access.StatusReady, workspace.MarkerReclone, "busy"},
	}
	for _, s := range states {
		w.setStatus(t, "demo", s.status)
		w.setMarker(t, "demo", s.marker)
		for _, g := range gitRequests {
			t.Run(s.name+"/"+g.name, func(t *testing.T) {
				want := "repository not available: workspace is " + s.want + "\n"
				if a := w.call(t, g.method, g.to("acme/demo.git"), basic("x", w.ownerKey)); a.status != http.StatusConflict || a.body != want {
					t.Errorf("answer = %d %q, want 409 %q", a.status, a.body, want)
				}
			})
		}
	}
	if n := w.service.served(); n != 0 {
		t.Errorf("%d refused requests reached the service, want none", n)
	}
}

// A want of "" is a request that reaches the service.
func TestState_JudgesAForkByItsOwnStatusThenByItsParent(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	w.addFork(t)
	const (
		busy     = "repository not available: parent workspace is busy\n"
		readOnly = "repository is read-only: parent workspace is archived\n"
	)
	conditions := []struct {
		name, fork, parent string
		marker             workspace.Marker
		fetch, push        string
	}{
		{"parent cloning", access.StatusReady, access.StatusCloning, "", busy, busy},
		{"parent pending", access.StatusReady, access.StatusPending, "", busy, busy},
		{"parent failed", access.StatusReady, access.StatusFailed, "", busy, busy},
		{"maintenance marker on a ready parent", access.StatusReady, access.StatusReady, workspace.MarkerReclone, busy, busy},
		{"maintenance marker on an archived parent", access.StatusReady, access.StatusArchived, workspace.MarkerDelete, busy, busy},
		{"parent archived", access.StatusReady, access.StatusArchived, "", "", readOnly},
		{"fork failed with its parent cloning", access.StatusFailed, access.StatusCloning, "",
			"repository not available: workspace is failed\n", "repository not available: workspace is failed\n"},
		{"parent ready", access.StatusReady, access.StatusReady, "", "", ""},
	}
	for _, c := range conditions {
		w.setStatus(t, "demo--t1", c.fork)
		w.setStatus(t, "demo", c.parent)
		w.setMarker(t, "demo", c.marker)
		for _, g := range gitRequests {
			t.Run(c.name+"/"+g.name, func(t *testing.T) {
				want := c.fetch
				if g.push {
					want = c.push
				}
				before := w.service.served()
				a := w.call(t, g.method, g.to("acme/demo--t1.git"), basic("x", w.memberKey))
				served := w.service.served() > before
				switch {
				case want == "" && (!served || a.status != http.StatusOK):
					t.Errorf("answer = %d %q, want the request served", a.status, a.body)
				case want != "" && (served || a.status != http.StatusConflict || a.body != want):
					t.Errorf("answer = %d %q (served %v), want 409 %q", a.status, a.body, served, want)
				}
			})
		}
	}
}

func TestState_ServesFetchesFromAForkOfAnArchivedParent(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	w.addFork(t)
	w.setStatus(t, "demo", access.StatusArchived)
	dir := t.TempDir()
	runGit(t, dir, "clone", w.remote(w.memberKey, "acme/demo--t1.git"), "d")
	if got, _ := clientGit(t, filepath.Join(dir, "d"), nil, "rev-parse", "origin/main"); strings.TrimSpace(got) != w.head {
		t.Errorf("origin/main of the clone = %s, want %s", got, w.head)
	}
}
