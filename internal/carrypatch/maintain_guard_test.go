package carrypatch_test

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// maintenanceRoute is a route that changes a patch list, as the guard tests
// send it.
type maintenanceRoute struct {
	name, method string
	// suffix is the path after /workspaces/<slug>/patches, with :id for the
	// patch the route names.
	suffix string
	// body is a body the route accepts for a workspace whose patches are
	// ids, in position order; nil sends none.
	body func(ids []string) any
}

// maintenanceRoutes are the routes the guard tests cover: each route that
// changes a patch list is added here when the component gets it.
var maintenanceRoutes = []maintenanceRoute{
	{name: "update", method: http.MethodPatch, suffix: "/:id", body: func([]string) any { return map[string]any{"description": "guarded"} }},
	{name: "delete", method: http.MethodDelete, suffix: "/:id", body: func([]string) any { return ignoredBody }},
	{name: "restore", method: http.MethodPost, suffix: "/:id/restore", body: func([]string) any { return ignoredBody }},
}

// ignoredBody is a body, not even JSON, that a route which takes none
// ignores.
const ignoredBody = "{not json"

// namesPatch reports whether the route names a patch.
func (rt maintenanceRoute) namesPatch() bool { return strings.Contains(rt.suffix, ":id") }

// call sends the route for the patch id of slug with credential and a valid
// body for the patches ids.
func (rt maintenanceRoute) call(t *testing.T, h *patchHub, credential, slug, id string, ids []string) answer {
	t.Helper()
	var body any
	if rt.body != nil {
		body = rt.body(ids)
	}
	return h.send(t, credential, rt.method, "/workspaces/"+slug+"/patches"+strings.ReplaceAll(rt.suffix, ":id", id), body)
}

// idsOf are the ids of the patches A, B and C of carryList.
func idsOf(ps map[string]carrypatch.Patch) []string {
	return []string{ps["A"].ID, ps["B"].ID, ps["C"].ID}
}

// untouched fails the test when the patches of slug differ from before or
// slug has a parley.patch.* event, or when a secret was read, which every
// fetch from origin does first.
func (h *patchHub) untouched(t *testing.T, slug string, before []carrypatch.Patch) {
	t.Helper()
	if after := h.patches(t, slug); !reflect.DeepEqual(after, before) {
		t.Errorf("the patches of %s changed:\n got %+v\nwant %+v", slug, after, before)
	}
	if e := h.patchEvents(t, slug, ""); len(e) != 0 {
		t.Errorf("%s has %d parley.patch.* events, want none: %+v", slug, len(e), e)
	}
	if n := h.secretReads.Load(); n != 0 {
		t.Errorf("the component read %d secrets, want none: origin was not to be contacted", n)
	}
}

func TestMaintenanceRoutes_RefuseCallersWithoutWriteAccess(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	before := h.patches(t, "carry")
	member := h.CreateUser(t, "member")
	h.AddMember(t, h.org, member)
	memberKey := h.IssueKey(t, member)
	outsiderKey := h.IssueKey(t, h.CreateUser(t, "outsider"))
	readPAT := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"patches:read"}})
	callers := []struct {
		name, credential, slug string
		status                 int
		errorType              string
	}{
		{"a PAT with patches:read only", readPAT, "carry", http.StatusForbidden, "insufficient_scope"},
		{"a PAT with patches:read only on an unknown slug", readPAT, "nowhere", http.StatusForbidden, "insufficient_scope"},
		{"a member with read access only", memberKey, "carry", http.StatusForbidden, "forbidden"},
		{"a user outside the organization", outsiderKey, "carry", http.StatusNotFound, "not_found"},
		{"the owner on an unknown slug", h.key, "nowhere", http.StatusNotFound, "not_found"},
	}
	for _, rt := range maintenanceRoutes {
		for _, c := range callers {
			t.Run(rt.name+"/"+c.name, func(t *testing.T) {
				a := rt.call(t, h, c.credential, c.slug, ps["A"].ID, idsOf(ps))
				if a.status != c.status || a.Error.ErrorType != c.errorType {
					t.Errorf("%s = %d %s, want %d %s", rt.name, a.status, a.raw, c.status, c.errorType)
				}
				h.untouched(t, "carry", before)
			})
		}
	}
}

func TestMaintenanceRoutes_RefuseStandardWorkspaces(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	before := h.patches(t, "carry")
	h.add(t, "plain", workspace.ModeStandard, workspace.StatusReady, workspace.MarkerNone)
	storeFork(t, h, "carry-fork", "carry")
	ids := []string{uuid.New().String(), uuid.New().String(), uuid.New().String()}
	for _, rt := range maintenanceRoutes {
		for _, slug := range []string{"plain", "carry-fork"} {
			t.Run(rt.name+"/"+slug, func(t *testing.T) {
				a := rt.call(t, h, h.key, slug, ids[0], ids)
				if a.status != http.StatusBadRequest || a.Error.ErrorType != "workspace_mode_mismatch" {
					t.Errorf("%s on %s = %d %s, want 400 workspace_mode_mismatch", rt.name, slug, a.status, a.raw)
				}
				h.untouched(t, "carry", before)
				if e := h.patchEvents(t, slug, ""); len(e) != 0 {
					t.Errorf("%s has %d parley.patch.* events, want none", slug, len(e))
				}
			})
		}
	}
	t.Run("an invalid body is refused before the mode", func(t *testing.T) {
		a := h.update(t, "plain", ps["A"].ID, map[string]any{"status": "bogus"})
		if a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" {
			t.Errorf("PATCH on plain with status bogus = %d %s, want 400 invalid_request", a.status, a.raw)
		}
	})
}

func TestMaintenanceRoutes_RefuseByWorkspaceStatus(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	states := []struct {
		slug   string
		status workspace.Status
		marker workspace.Marker
		want   string
	}{
		{"pending", workspace.StatusPending, workspace.MarkerNone, "workspace_busy"},
		{"cloning", workspace.StatusCloning, workspace.MarkerNone, "workspace_busy"},
		{"recloning", workspace.StatusReady, workspace.MarkerReclone, "workspace_busy"},
		{"archiving", workspace.StatusReady, workspace.MarkerArchive, "workspace_busy"},
		{"deleting", workspace.StatusReady, workspace.MarkerDelete, "workspace_busy"},
		{"failed", workspace.StatusFailed, workspace.MarkerNone, "workspace_not_ready"},
		{"archived", workspace.StatusArchived, workspace.MarkerNone, "workspace_archived"},
	}
	for _, s := range states {
		ps := h.carryList(t, s.slug, s.status, s.marker)
		before := h.patches(t, s.slug)
		for _, rt := range maintenanceRoutes {
			t.Run(rt.name+"/"+s.slug, func(t *testing.T) {
				a := rt.call(t, h, h.key, s.slug, ps["A"].ID, idsOf(ps))
				if a.status != http.StatusConflict || a.Error.ErrorType != s.want {
					t.Errorf("%s on %s = %d %s, want 409 %s", rt.name, s.slug, a.status, a.raw, s.want)
				}
				h.untouched(t, s.slug, before)
			})
		}
	}
}

func TestMaintenanceRoutes_UnknownPatchIsNotFound(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	ps := h.carryList(t, "carry", workspace.StatusReady, workspace.MarkerNone)
	h.add(t, "other", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	foreign := livePatch("other", "patch/o", 1)
	h.put(t, foreign)
	before, beforeOther := h.patches(t, "carry"), h.patches(t, "other")
	ids := map[string]string{
		"a random UUID":                uuid.New().String(),
		"the id of a patch of another": foreign.ID,
		"a value that is not a UUID":   "not-a-uuid",
	}
	for _, rt := range maintenanceRoutes {
		if !rt.namesPatch() {
			continue
		}
		for name, id := range ids {
			t.Run(rt.name+"/"+name, func(t *testing.T) {
				a := rt.call(t, h, h.key, "carry", id, idsOf(ps))
				if a.status != http.StatusNotFound || a.Error.ErrorType != "not_found" || a.Error.Message != "patch not found" {
					t.Errorf("%s with %s = %d %s, want 404 not_found %q", rt.name, name, a.status, a.raw, "patch not found")
				}
				h.untouched(t, "carry", before)
				if after := h.patches(t, "other"); !reflect.DeepEqual(after, beforeOther) {
					t.Errorf("the patches of other changed: %+v, want %+v", after, beforeOther)
				}
			})
		}
	}
}

// storeFork stores the task fork slug of the workspace parent, owned by
// the owner; a fork is always in mode standard.
func storeFork(t *testing.T, h *patchHub, slug, parent string) {
	t.Helper()
	now := time.Now().UTC()
	w := workspace.Workspace{
		Slug: slug, Kind: access.KindFork, ParentSlug: &parent, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
		OwnerID: h.owner, OrgID: h.org, Status: workspace.StatusReady, DisplayName: slug,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, w) }); err != nil {
		t.Fatalf("store the fork %s: %v", slug, err)
	}
}
