package carrypatch_test

import (
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// workspaceRoute is a route of a workspace's remembered resolutions or of
// its patch status; path follows /workspaces/<slug>.
type workspaceRoute struct {
	method, path string
	write        bool
}

func (rt workspaceRoute) String() string { return rt.method + " " + rt.path }

// send sends the route for slug with credential.
func (rt workspaceRoute) send(t *testing.T, h *rebuildHub, credential, slug string) answer {
	t.Helper()
	return h.call(t, credential, rt.method, "/workspaces/"+slug+rt.path, nil, nil)
}

// resolutionRoutes are the list, the patch status and the forget of the
// entry id.
func resolutionRoutes(id string) []workspaceRoute {
	return []workspaceRoute{
		{http.MethodGet, "/rerere", false},
		{http.MethodGet, "/patch-status", false},
		{http.MethodDelete, "/rerere/" + id, true},
	}
}

// entriesState is what a refused request leaves as it was: the stored
// entry rows of a workspace and the files of its copy, none without one.
type entriesState struct {
	rows  []string
	files map[string]string
}

func (h *rebuildHub) entriesState(t *testing.T, slug string) entriesState {
	t.Helper()
	s := entriesState{rows: h.storedEntries(t, slug)}
	if _, err := os.Stat(h.repoPath(slug)); err == nil {
		s.files = filesIn(t, h.repoPath(slug))
	}
	return s
}

// keptEntries fails the test when the entries of slug differ from before.
func (h *rebuildHub) keptEntries(t *testing.T, slug string, before entriesState) {
	t.Helper()
	after := h.entriesState(t, slug)
	if !slices.Equal(after.rows, before.rows) {
		t.Errorf("the entry rows of %s = %v, want them unchanged, %v", slug, after.rows, before.rows)
	}
	if !maps.Equal(after.files, before.files) {
		t.Errorf("the files of the copy of %s = %v, want them unchanged, %v", slug, slices.Sorted(maps.Keys(after.files)), slices.Sorted(maps.Keys(before.files)))
	}
}

// refused sends rt for slug with credential and fails the test unless it
// answers status and errorType without running git.
func (h *rebuildHub) refused(t *testing.T, rt workspaceRoute, credential, slug string, status int, errorType string) answer {
	t.Helper()
	gits := len(h.gitCommands())
	a := rt.send(t, h, credential, slug)
	if a.status != status || a.Error.ErrorType != errorType {
		t.Errorf("%s on %s = %d %s, want %d %s", rt, slug, a.status, a.raw, status, errorType)
	}
	if ran := h.gitCommands()[gits:]; len(ran) != 0 {
		t.Errorf("the refused %s on %s ran git: %q", rt, slug, ran)
	}
	return a
}

// unreadyWorkspace is a carry-patch workspace outside the ready state,
// which holds the entry, and a copy where the state has one; want is the
// error_type a forget there answers.
type unreadyWorkspace struct {
	slug   string
	status workspace.Status
	marker workspace.Marker
	copy   bool
	want   string
}

// routesFixture is a rebuild hub with the workspaces the refusals of the
// resolution and status routes are sent to: demo, a ready carry-patch
// workspace holding entry, the standard workspace plain, task forks of
// both, and the unready workspaces.
type routesFixture struct {
	h       *rebuildHub
	entry   rerereEntry
	unready []unreadyWorkspace
	routes  []workspaceRoute
	demo    entriesState
}

func startRoutesFixture(t *testing.T) *routesFixture {
	t.Helper()
	h := startRebuildHub(t)
	entry := rerereEntry{rerereID("a"), "src/a.go", time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), true}
	h.rerereCopy(t, "demo", entry)
	h.add(t, "plain", workspace.ModeStandard, workspace.StatusReady, workspace.MarkerNone)
	// A task fork is always in mode standard, whatever its parent's mode.
	for _, f := range []struct {
		slug, parent string
		status       workspace.Status
	}{{"plain-fork", "plain", workspace.StatusReady}, {"demo-fork", "demo", workspace.StatusReady}, {"demo-fork-archived", "demo", workspace.StatusArchived}} {
		h.add(t, f.slug, workspace.ModeStandard, f.status, workspace.MarkerNone, func(w *workspace.Workspace) {
			w.Kind, w.ParentSlug, w.GitURL = access.KindFork, new(f.parent), nil
		})
	}
	unready := []unreadyWorkspace{
		{"pending", workspace.StatusPending, workspace.MarkerNone, false, "workspace_busy"},
		{"cloning", workspace.StatusCloning, workspace.MarkerNone, false, "workspace_busy"},
		{"recloning", workspace.StatusReady, workspace.MarkerReclone, true, "workspace_busy"},
		{"archiving", workspace.StatusReady, workspace.MarkerArchive, true, "workspace_busy"},
		{"deleting", workspace.StatusReady, workspace.MarkerDelete, true, "workspace_busy"},
		{"failed", workspace.StatusFailed, workspace.MarkerNone, true, "workspace_not_ready"},
		{"archived", workspace.StatusArchived, workspace.MarkerNone, true, "workspace_archived"},
	}
	for _, s := range unready {
		if s.copy {
			h.rerereCopy(t, s.slug, entry)
			h.setState(t, s.slug, s.status, s.marker)
		} else {
			h.add(t, s.slug, workspace.ModeCarryPatch, s.status, s.marker)
			h.recordAt(t, s.slug, entry)
		}
	}
	return &routesFixture{h: h, entry: entry, unready: unready, routes: resolutionRoutes(entry.id), demo: h.entriesState(t, "demo")}
}

// listed fails the test unless the owner's list of demo still holds the
// entry and the entries of demo are as they were.
func (f *routesFixture) listed(t *testing.T) {
	t.Helper()
	if a := f.routes[0].send(t, f.h, f.h.key, "demo"); a.status != http.StatusOK || !slices.Equal(a.ids(), []string{f.entry.id}) {
		t.Errorf("GET rerere of demo as the owner = %d %s, want the entry %s", a.status, a.raw, f.entry.id)
	}
	f.h.keptEntries(t, "demo", f.demo)
}

// The rows share one hub and its workspaces, set up first, and run one
// after the other, so that the git commands the hub runs during a row are
// that row's alone.
func TestResolutionAndStatusRoutes_RefuseCallersAndWorkspacesTheyDoNotServe(t *testing.T) {
	t.Parallel()
	f := startRoutesFixture(t)

	t.Run("a token without the scope of the route is refused, whether the workspace exists or not", func(t *testing.T) {
		routesRefuseATokenWithoutTheScope(t, f)
	})

	t.Run("a caller without access learns nothing and a reader may not forget", func(t *testing.T) {
		routesRefuseACallerWithoutAccess(t, f)
	})

	t.Run("a workspace not in carry-patch mode is refused", func(t *testing.T) {
		routesRefuseAWorkspaceNotInCarryPatchMode(t, f)
	})

	t.Run("a forget outside a ready workspace is refused and the list is served", func(t *testing.T) {
		routesRefuseAForgetOutsideAReadyWorkspace(t, f)
	})

	t.Run("a forget checks the request, then the mode, then the state", func(t *testing.T) {
		routesForgetChecksInOrder(t, f)
	})
}

func routesRefuseATokenWithoutTheScope(t *testing.T, f *routesFixture) {
	h := f.h
	patchesRead := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"patches:read"}})
	rebuildsRead := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"rebuilds:read"}})
	workspacesRead := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"workspaces:read"}})
	for _, c := range []struct {
		name, credential string
		routes           []workspaceRoute
	}{
		{"patches:read only", patchesRead, f.routes},
		{"rebuilds:read only", rebuildsRead, f.routes},
		{"workspaces:read only", workspacesRead, f.routes[2:]},
	} {
		for _, rt := range c.routes {
			for _, slug := range []string{"demo", "nosuch"} {
				t.Run(c.name+"/"+rt.String()+"/"+slug, func(t *testing.T) {
					h.refused(t, rt, c.credential, slug, http.StatusForbidden, "insufficient_scope")
				})
			}
		}
	}
	f.listed(t)
}

func routesRefuseACallerWithoutAccess(t *testing.T, f *routesFixture) {
	h := f.h
	member := h.CreateUser(t, "member")
	h.AddMember(t, h.org, member)
	memberKey := h.IssueKey(t, member)
	outsiderKey := h.IssueKey(t, h.CreateUser(t, "outsider"))
	for _, rt := range f.routes {
		t.Run("a user outside the organization/"+rt.String(), func(t *testing.T) {
			outsider := h.refused(t, rt, outsiderKey, "demo", http.StatusNotFound, "not_found")
			unknown := h.refused(t, rt, h.key, "nosuch", http.StatusNotFound, "not_found")
			if string(outsider.raw) != string(unknown.raw) {
				t.Errorf("%s answered the outsider %s and the owner on nosuch %s, want the same", rt, outsider.raw, unknown.raw)
			}
		})
		t.Run("a member with read access/"+rt.String(), func(t *testing.T) {
			if rt.write {
				h.refused(t, rt, memberKey, "demo", http.StatusForbidden, "forbidden")
				return
			}
			if a := rt.send(t, h, memberKey, "demo"); a.status != http.StatusOK {
				t.Errorf("%s on demo as a member with read access = %d %s, want 200", rt, a.status, a.raw)
			}
		})
	}
	f.listed(t)
}

func routesRefuseAWorkspaceNotInCarryPatchMode(t *testing.T, f *routesFixture) {
	h := f.h
	for _, slug := range []string{"plain", "plain-fork", "demo-fork"} {
		for _, rt := range resolutionRoutes("abc") {
			t.Run(slug+"/"+rt.String(), func(t *testing.T) {
				h.refused(t, rt, h.key, slug, http.StatusBadRequest, "workspace_mode_mismatch")
			})
		}
	}
	f.listed(t)
}

func routesRefuseAForgetOutsideAReadyWorkspace(t *testing.T, f *routesFixture) {
	h := f.h
	for _, s := range f.unready {
		t.Run(s.slug, func(t *testing.T) {
			before := h.entriesState(t, s.slug)
			h.refused(t, f.routes[2], h.key, s.slug, http.StatusConflict, s.want)
			h.keptEntries(t, s.slug, before)
			if _, err := os.Stat(filepath.Join(h.repoPath(s.slug), "rr-cache", f.entry.id)); s.copy && err != nil {
				t.Errorf("the entry's directory in the copy of %s: %v, want it kept", s.slug, err)
			}
			if a := f.routes[0].send(t, h, h.key, s.slug); a.status != http.StatusOK {
				t.Errorf("%s on %s = %d %s, want 200: the list serves in every state", f.routes[0], s.slug, a.status, a.raw)
			}
		})
	}
}

func routesForgetChecksInOrder(t *testing.T, f *routesFixture) {
	h := f.h
	for _, c := range []struct {
		name, slug, pathspec string
		status               int
		want                 string
	}{
		{"an option-like pathspec on a standard workspace", "plain", "-x", http.StatusBadRequest, "invalid_request"},
		{"a valid pathspec on an archived task fork", "demo-fork-archived", f.entry.id, http.StatusBadRequest, "workspace_mode_mismatch"},
		{"an option-like pathspec on an archived carry-patch workspace", "archived", "-x", http.StatusBadRequest, "invalid_request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.refused(t, workspaceRoute{http.MethodDelete, "/rerere/" + c.pathspec, true}, h.key, c.slug, c.status, c.want)
		})
	}
}
