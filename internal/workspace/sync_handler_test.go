package workspace_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// syncBranches decodes the branches of a, which must be 200, and checks that
// the rest of a is the workspace object.
func syncBranches(t *testing.T, a answer) (map[string]any, []workspace.BranchResult) {
	t.Helper()
	obj := a.decode(t, http.StatusOK)
	var body struct {
		Branches []workspace.BranchResult `json:"branches"`
	}
	if err := json.Unmarshal(a.body, &body); err != nil || body.Branches == nil {
		t.Fatalf("the sync answer %s has no branches list (%v)", a.body, err)
	}
	delete(obj, "branches")
	if len(obj) != len(objectMembers) {
		t.Errorf("the sync answer has %d members beside branches, want the %d of a workspace object", len(obj), len(objectMembers))
	}
	return obj, body.Branches
}

func TestSyncRoute_AnswersWorkspaceWithBranches(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	heads := refsUnder(t, s.origin.Dir(), "refs/heads/")
	var want []workspace.BranchResult
	for _, name := range slices.Sorted(maps.Keys(heads)) {
		want = append(want, workspace.BranchResult{Name: name, Result: workspace.BranchUpToDate, LocalSHA: heads[name], OriginSHA: heads[name]})
	}
	for _, c := range []struct {
		caller, cred string
		level        access.Level
	}{
		{"the owner", s.uKey, access.LevelOwner},
		{"the admin token", s.Token, access.LevelAdmin},
	} {
		obj, branches := syncBranches(t, s.postSync(t, c.cred, syncSlug, ""))
		if obj["slug"] != syncSlug || obj["access"] != string(c.level) {
			t.Errorf("%s: answer slug %v access %v, want %s %s", c.caller, obj["slug"], obj["access"], syncSlug, c.level)
		}
		if !reflect.DeepEqual(branches, want) {
			t.Errorf("%s: branches = %+v, want %+v", c.caller, branches, want)
		}
		// The object is the record as the sync left it, the one a read
		// answers now.
		if read := s.get(t, c.cred, syncSlug).decode(t, http.StatusOK); !reflect.DeepEqual(obj, read) {
			t.Errorf("%s: the sync answer's object %v differs from a read after it %v", c.caller, obj, read)
		}
	}
}

func TestSyncRoute_ResetsDivergedBranchesOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	base := ref(t, s.copy, "refs/heads/dev")
	grow(t, s.origin.Dir(), "dev", base, "origin")
	grow(t, s.copy, "dev", base, "hub")
	for _, c := range []struct {
		query string
		want  workspace.BranchOutcome
	}{
		{"", workspace.BranchDiverged},
		{"reset_to_origin=false", workspace.BranchDiverged},
		{"reset_to_origin=true", workspace.BranchReset},
	} {
		_, branches := syncBranches(t, s.postSync(t, s.uKey, syncSlug, c.query))
		got := outcomes(workspace.SyncResult{Branches: branches})
		if got["dev"] != c.want || got["main"] != workspace.BranchUpToDate {
			t.Errorf("query %q: results %v, want dev %s and main up_to_date", c.query, got, c.want)
		}
	}
}

func TestSyncRoute_RefusesWhenSyncDisabled(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	setSync := func(enabled bool) {
		t.Helper()
		if a := s.patch(t, s.uKey, syncSlug, map[string]any{"settings": map[string]any{"sync_enabled": enabled}}); a.status != http.StatusOK {
			t.Fatalf("PATCH sync_enabled %v = %d %s, want 200", enabled, a.status, a.body)
		}
	}
	setSync(false)
	if a := s.postSync(t, s.uKey, syncSlug, ""); a.status != http.StatusBadRequest || a.errorType(t) != "sync_disabled" {
		t.Errorf("sync with sync_enabled false = %d %s, want 400 sync_disabled", a.status, a.body)
	}
	if n := len(s.fetches.recorded()); n != 0 {
		t.Errorf("a refused sync fetched origin %d times", n)
	}
	setSync(true)
	if a := s.postSync(t, s.uKey, syncSlug, ""); a.status != http.StatusOK {
		t.Errorf("sync with sync_enabled true again = %d %s, want 200", a.status, a.body)
	}
}

func TestSyncRoute_RefusesBeforeFetching(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	acme := s.acme(t)
	disabled := map[string]json.RawMessage{"sync_enabled": json.RawMessage(`false`)}
	records := []workspace.Workspace{
		{Slug: "team", OrgID: acme, Status: workspace.StatusReady},
		{Slug: "team-old", OrgID: acme, Status: workspace.StatusArchived},
		{Slug: "demo--t1", Kind: access.KindFork, ParentSlug: new(syncSlug), Status: workspace.StatusReady},
		{Slug: "demo--t2", Kind: access.KindFork, ParentSlug: new(syncSlug), Status: workspace.StatusArchived},
		{Slug: "pending"},
		{Slug: "cloning", Status: workspace.StatusCloning},
		{Slug: "recloning", Status: workspace.StatusReady, Marker: workspace.MarkerReclone},
		{Slug: "archiving", Status: workspace.StatusReady, Marker: workspace.MarkerArchive},
		{Slug: "deleting", Status: workspace.StatusFailed, Marker: workspace.MarkerDelete},
		{Slug: "failed", Status: workspace.StatusFailed},
		{Slug: "archived", Status: workspace.StatusArchived},
		{Slug: "archived-off", Status: workspace.StatusArchived, Settings: disabled},
		{Slug: "failed-off", Status: workspace.StatusFailed, Settings: disabled},
		{Slug: "cloning-off", Status: workspace.StatusCloning, Settings: disabled},
	}
	slugs := []string{syncSlug}
	for _, rec := range records {
		s.put(t, rec)
		slugs = append(slugs, rec.Slug)
	}
	snapshot := func() (map[string]workspace.Workspace, int, map[string]string) {
		recs := map[string]workspace.Workspace{}
		for _, slug := range slugs {
			recs[slug], _ = s.workspace(t, slug)
		}
		return recs, s.eventCount(t), refsUnder(t, s.copy, "refs/")
	}
	recsBefore, eventsBefore, refsBefore := snapshot()
	readPAT := s.pat(t, s.u, "workspaces:read")
	bodies := map[string][]byte{}
	for _, c := range []struct {
		name, cred, slug, query string
		status                  int
		errorType               string
	}{
		{"a PAT with only workspaces:read", readPAT, syncSlug, "", http.StatusForbidden, "insufficient_scope"},
		{"an org member", s.vKey, "team", "", http.StatusForbidden, "forbidden"},
		{"a user outside the org", s.vKey, syncSlug, "", http.StatusNotFound, "not_found"},
		{"an unknown slug", s.vKey, "nope", "", http.StatusNotFound, "not_found"},
		{"reset_to_origin=yes", s.uKey, syncSlug, "reset_to_origin=yes", http.StatusBadRequest, "invalid_request"},
		{"reset_to_origin=1", s.uKey, syncSlug, "reset_to_origin=1", http.StatusBadRequest, "invalid_request"},
		{"reset_to_origin=TRUE", s.uKey, syncSlug, "reset_to_origin=TRUE", http.StatusBadRequest, "invalid_request"},
		{"reset_to_origin empty", s.uKey, syncSlug, "reset_to_origin=", http.StatusBadRequest, "invalid_request"},
		{"reset_to_origin twice", s.uKey, syncSlug, "reset_to_origin=true&reset_to_origin=true", http.StatusBadRequest, "invalid_request"},
		{"a task fork", s.uKey, "demo--t1", "", http.StatusBadRequest, "fork_has_no_origin"},
		{"pending", s.uKey, "pending", "", http.StatusConflict, "workspace_busy"},
		{"cloning", s.uKey, "cloning", "", http.StatusConflict, "workspace_busy"},
		{"ready with a reclone running", s.uKey, "recloning", "", http.StatusConflict, "workspace_busy"},
		{"ready with an archive running", s.uKey, "archiving", "", http.StatusConflict, "workspace_busy"},
		{"failed with a delete running", s.uKey, "deleting", "", http.StatusConflict, "workspace_busy"},
		{"failed", s.uKey, "failed", "", http.StatusConflict, "workspace_not_ready"},
		{"archived", s.uKey, "archived", "", http.StatusConflict, "workspace_archived"},
		// When several checks fail, the first in the order access, query,
		// kind, status, sync_enabled answers.
		{"an archived task fork", s.uKey, "demo--t2", "", http.StatusBadRequest, "fork_has_no_origin"},
		{"archived and sync_enabled false", s.uKey, "archived-off", "", http.StatusConflict, "workspace_archived"},
		{"failed and sync_enabled false", s.uKey, "failed-off", "", http.StatusConflict, "workspace_not_ready"},
		{"cloning and sync_enabled false", s.uKey, "cloning-off", "", http.StatusConflict, "workspace_busy"},
		{"archived with reset_to_origin=yes", s.uKey, "archived", "reset_to_origin=yes", http.StatusBadRequest, "invalid_request"},
		{"an org member on an archived workspace", s.vKey, "team-old", "", http.StatusForbidden, "forbidden"},
	} {
		a := s.postSync(t, c.cred, c.slug, c.query)
		if a.status != c.status || a.errorType(t) != c.errorType {
			t.Errorf("%s: answer %d %s, want %d %s", c.name, a.status, a.body, c.status, c.errorType)
		}
		bodies[c.name] = a.body
	}
	if out, unknown := bodies["a user outside the org"], bodies["an unknown slug"]; string(out) != string(unknown) {
		t.Errorf("the outsider's answer %s differs from the unknown slug's %s", out, unknown)
	}
	if n := len(s.fetches.recorded()); n != 0 {
		t.Errorf("refused syncs fetched origin %d times", n)
	}
	recsAfter, eventsAfter, refsAfter := snapshot()
	if !reflect.DeepEqual(recsAfter, recsBefore) || eventsAfter != eventsBefore || !maps.Equal(refsAfter, refsBefore) {
		t.Errorf("refused syncs changed the hub: records %v -> %v, events %d -> %d, refs %v -> %v",
			recsBefore, recsAfter, eventsBefore, eventsAfter, refsBefore, refsAfter)
	}
}
