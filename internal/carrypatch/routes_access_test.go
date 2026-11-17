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

// The rows share one hub and its workspaces, set up first, and run one
// after the other, so that the git commands the hub runs during a row are
// that row's alone.
func TestResolutionAndStatusRoutes_RefuseCallersAndWorkspacesTheyDoNotServe(t *testing.T) {
	t.Parallel()
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
	// Carry-patch workspaces outside the ready state, each with the entry,
	// and a copy where the state has one.
	unready := []struct {
		slug   string
		status workspace.Status
		marker workspace.Marker
		copy   bool
		want   string // the forget's error_type
	}{
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
	routes := resolutionRoutes(entry.id)
	demo := h.entriesState(t, "demo")
	listed := func(t *testing.T) {
		t.Helper()
		if a := routes[0].send(t, h, h.key, "demo"); a.status != http.StatusOK || !slices.Equal(a.ids(), []string{entry.id}) {
			t.Errorf("GET rerere of demo as the owner = %d %s, want the entry %s", a.status, a.raw, entry.id)
		}
		h.keptEntries(t, "demo", demo)
	}

	t.Run("a token without the scope of the route is refused, whether the workspace exists or not", func(t *testing.T) {
		patchesRead := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"patches:read"}})
		rebuildsRead := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"rebuilds:read"}})
		workspacesRead := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"workspaces:read"}})
		for _, c := range []struct {
			name, credential string
			routes           []workspaceRoute
		}{
			{"patches:read only", patchesRead, routes},
			{"rebuilds:read only", rebuildsRead, routes},
			{"workspaces:read only", workspacesRead, routes[2:]},
		} {
			for _, rt := range c.routes {
				for _, slug := range []string{"demo", "nosuch"} {
					t.Run(c.name+"/"+rt.String()+"/"+slug, func(t *testing.T) {
						h.refused(t, rt, c.credential, slug, http.StatusForbidden, "insufficient_scope")
					})
				}
			}
		}
		listed(t)
	})

	t.Run("a caller without access learns nothing and a reader may not forget", func(t *testing.T) {
		member := h.CreateUser(t, "member")
		h.AddMember(t, h.org, member)
		memberKey := h.IssueKey(t, member)
		outsiderKey := h.IssueKey(t, h.CreateUser(t, "outsider"))
		for _, rt := range routes {
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
		listed(t)
	})

	t.Run("a workspace not in carry-patch mode is refused", func(t *testing.T) {
		for _, slug := range []string{"plain", "plain-fork", "demo-fork"} {
			for _, rt := range resolutionRoutes("abc") {
				t.Run(slug+"/"+rt.String(), func(t *testing.T) {
					h.refused(t, rt, h.key, slug, http.StatusBadRequest, "workspace_mode_mismatch")
				})
			}
		}
		listed(t)
	})

	t.Run("a forget outside a ready workspace is refused and the reads are served", func(t *testing.T) {
		for _, s := range unready {
			t.Run(s.slug, func(t *testing.T) {
				before := h.entriesState(t, s.slug)
				h.refused(t, routes[2], h.key, s.slug, http.StatusConflict, s.want)
				h.keptEntries(t, s.slug, before)
				if _, err := os.Stat(filepath.Join(h.repoPath(s.slug), "rr-cache", entry.id)); s.copy && err != nil {
					t.Errorf("the entry's directory in the copy of %s: %v, want it kept", s.slug, err)
				}
				for _, rt := range routes[:2] {
					if a := rt.send(t, h, h.key, s.slug); a.status != http.StatusOK {
						t.Errorf("%s on %s = %d %s, want 200: a read serves in every state", rt, s.slug, a.status, a.raw)
					}
				}
			})
		}
	})

	t.Run("a forget checks the request, then the mode, then the state", func(t *testing.T) {
		for _, c := range []struct {
			name, slug, pathspec string
			status               int
			want                 string
		}{
			{"an option-like pathspec on a standard workspace", "plain", "-x", http.StatusBadRequest, "invalid_request"},
			{"a valid pathspec on an archived task fork", "demo-fork-archived", entry.id, http.StatusBadRequest, "workspace_mode_mismatch"},
			{"an option-like pathspec on an archived carry-patch workspace", "archived", "-x", http.StatusBadRequest, "invalid_request"},
		} {
			t.Run(c.name, func(t *testing.T) {
				h.refused(t, workspaceRoute{http.MethodDelete, "/rerere/" + c.pathspec, true}, h.key, c.slug, c.status, c.want)
			})
		}
	})
}
