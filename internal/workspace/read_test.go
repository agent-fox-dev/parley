package workspace_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestRead_AnswersEveryStatusAndKindToEveryReader(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	acme := w.acme(t)
	statuses := []workspace.Status{workspace.StatusPending, workspace.StatusCloning, workspace.StatusReady,
		workspace.StatusFailed, workspace.StatusArchived}
	for _, s := range statuses {
		w.put(t, workspace.Workspace{Slug: string(s), Status: s, OrgID: acme})
	}
	w.put(t, workspace.Workspace{Slug: "fork", Kind: access.KindFork, ParentSlug: new("ready"),
		Status: workspace.StatusReady, OrgID: acme})
	records := map[string]workspace.Status{"fork": workspace.StatusReady}
	for _, s := range statuses {
		records[string(s)] = s
	}
	for slug, status := range records {
		for _, c := range []struct{ caller, cred, access string }{
			{"the owner", w.uKey, "owner"}, {"a member", w.vKey, "member"}, {"the admin token", w.Token, "admin"},
		} {
			obj := w.get(t, c.cred, slug).decode(t, http.StatusOK)
			if obj["slug"] != slug || obj["status"] != string(status) || obj["access"] != c.access {
				t.Errorf("%s as %s: slug %v, status %v, access %v; want %s, %s, %s",
					slug, c.caller, obj["slug"], obj["status"], obj["access"], slug, status, c.access)
			}
		}
	}
}

func TestRead_AnswersTheSame404ForAnUnreadableAndAnUnknownSlug(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, status := range []workspace.Status{workspace.StatusReady, workspace.StatusArchived} {
		w.deleteRecord(t, "private-one")
		w.put(t, workspace.Workspace{Slug: "private-one", Status: status, OwnerID: w.v, OrgID: w.vOrg.ID})
		unreadable, unknown := w.get(t, w.uKey, "private-one"), w.get(t, w.uKey, "no-such-slug")
		errorType, message := unreadable.refusal(t)
		if unreadable.status != http.StatusNotFound || errorType != "not_found" || message != "workspace not found" {
			t.Errorf("%s: unreadable = %d %s, want 404 not_found \"workspace not found\"", status, unreadable.status, unreadable.body)
		}
		if unknown.status != unreadable.status || !bytes.Equal(unknown.body, unreadable.body) {
			t.Errorf("%s: unknown = %d %s, want the unreadable's %d %s",
				status, unknown.status, unknown.body, unreadable.status, unreadable.body)
		}
	}
}

func TestRead_RefusesAPATWithoutAWorkspacesScopeBeforeTheLookup(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo"})
	other, writer := w.pat(t, w.u, "changes:read"), w.pat(t, w.u, "workspaces:write")
	existing, unknown := w.get(t, other, "demo"), w.get(t, other, "no-such-slug")
	if existing.status != http.StatusForbidden || existing.errorType(t) != "insufficient_scope" {
		t.Errorf("an existing slug = %d %s, want 403 insufficient_scope", existing.status, existing.body)
	}
	if unknown.status != existing.status || !bytes.Equal(unknown.body, existing.body) {
		t.Errorf("an unknown slug = %d %s, want the existing's %d %s", unknown.status, unknown.body, existing.status, existing.body)
	}
	if a := w.get(t, writer, "demo"); a.status != http.StatusOK {
		t.Errorf("a PAT with workspaces:write = %d %s, want 200", a.status, a.body)
	}
}

func TestRead_ShowsAForkTheEffectiveSettingsOfItsParent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, DefaultBranch: new("main")})
	w.put(t, workspace.Workspace{Slug: "demo--t1", Kind: access.KindFork, ParentSlug: new("demo"),
		Status: workspace.StatusReady, Settings: map[string]json.RawMessage{"audit_fetches": json.RawMessage(`true`)}})
	settings := func(slug string) map[string]any {
		t.Helper()
		s, _ := w.get(t, w.uKey, slug).decode(t, http.StatusOK)["settings"].(map[string]any)
		return s
	}
	if fork, parent := settings("demo--t1"), settings("demo"); len(parent) == 0 || !reflect.DeepEqual(fork, parent) {
		t.Errorf("the fork's settings = %v, want its parent's %v", fork, parent)
	}
	// The parent's settings change after the fork was made.
	if _, err := w.Store.Main().Exec(`UPDATE workspaces SET settings = ? WHERE slug = 'demo'`,
		`{"land.strategy":"squash"}`); err != nil {
		t.Fatal(err)
	}
	fork := settings("demo--t1")
	if fork["land.strategy"] != "squash" || !reflect.DeepEqual(fork["protected_branches"], []any{"main"}) {
		t.Errorf("after the parent changed, the fork's land.strategy = %v and protected_branches = %v, want squash and [main]",
			fork["land.strategy"], fork["protected_branches"])
	}
}
