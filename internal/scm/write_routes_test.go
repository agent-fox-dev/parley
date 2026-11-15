package scm_test

import (
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// TestWriteRoutes_GateBeforeTheirOwnChecks checks that every write route
// answers the write gate's refusal to a body its own checks would refuse
// too, and moves nothing.
func TestWriteRoutes_GateBeforeTheirOwnChecks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	old := w.workspaces.RepoPath("old")
	refs, oldRefs := gitrepotest.Git(t, w.repo, "for-each-ref"), gitrepotest.Git(t, old, "for-each-ref")
	routes := []struct{ method, path string }{
		{http.MethodPost, "/commits"},
		{http.MethodPost, "/refs"},
		{http.MethodPatch, "/refs/main"},
		{http.MethodDelete, "/refs/main"},
		{http.MethodPost, "/rebase"},
	}
	callers := []struct {
		name, slug, credential string
		status                 int
		errorType              string
	}{
		{"a PAT with git:read alone", "demo", w.readPAT, http.StatusForbidden, "insufficient_scope"},
		{"an org member, who may only read", "demo", w.memberKey, http.StatusForbidden, "forbidden"},
		{"the owner of an archived workspace", "old", w.ownerKey, http.StatusConflict, "workspace_archived"},
	}
	for _, route := range routes {
		for _, c := range callers {
			r := w.sendJSON(t, route.method, scmPath(c.slug, route.path), c.credential, map[string]any{})
			if r.status != c.status || r.errorType() != c.errorType {
				t.Errorf("%s %s by %s: %d %s, want %d %s", route.method, route.path, c.name, r.status, r.body, c.status, c.errorType)
			}
		}
	}
	if after := gitrepotest.Git(t, w.repo, "for-each-ref"); after != refs {
		t.Errorf("demo's refs changed from\n%s\nto\n%s", refs, after)
	}
	if after := gitrepotest.Git(t, old, "for-each-ref"); after != oldRefs {
		t.Errorf("old's refs changed from\n%s\nto\n%s", oldRefs, after)
	}
}
