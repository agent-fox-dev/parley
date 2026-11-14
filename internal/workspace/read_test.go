package workspace_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
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
	if a := w.list(t, other, ""); a.status != http.StatusForbidden || a.errorType(t) != "insufficient_scope" {
		t.Errorf("the list = %d %s, want 403 insufficient_scope", a.status, a.body)
	}
	if slugs, _, _ := w.list(t, writer, "").page(t); !slices.Equal(slugs, []string{"demo"}) {
		t.Errorf("the list with workspaces:write = %v, want [demo]", slugs)
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
	if a := w.patch(t, w.uKey, "demo", map[string]any{"settings": map[string]any{"land.strategy": "squash"}}); a.status != http.StatusOK {
		t.Fatalf("PATCH the parent's settings = %d %s, want 200", a.status, a.body)
	}
	fork := settings("demo--t1")
	if fork["land.strategy"] != "squash" || !reflect.DeepEqual(fork["protected_branches"], []any{"main"}) {
		t.Errorf("after the parent changed, the fork's land.strategy = %v and protected_branches = %v, want squash and [main]",
			fork["land.strategy"], fork["protected_branches"])
	}
}

func TestRead_ListsTheCallersReadableSetWithTheirLevel(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	carol, dave := w.CreateUser(t, "carol"), w.CreateUser(t, "dave")
	w.put(t, workspace.Workspace{Slug: "a-own"})
	w.put(t, workspace.Workspace{Slug: "a-own--t1", Kind: access.KindFork, ParentSlug: new("a-own")})
	w.put(t, workspace.Workspace{Slug: "shared", OrgID: w.acme(t)})
	w.put(t, workspace.Workspace{Slug: "other", OwnerID: dave, OrgID: w.personalOrg(t, dave).ID})
	bound := w.IssuePAT(t, identity.PATSpec{UserID: w.u, Permissions: []string{"workspaces:read"}, Workspace: "a-own"})
	for _, c := range []struct {
		caller, cred string
		want         map[string]string
	}{
		{"the owner", w.uKey, map[string]string{"a-own": "owner", "a-own--t1": "owner", "shared": "owner"}},
		{"an org member", w.vKey, map[string]string{"shared": "member"}},
		{"a user with no workspace", w.IssueKey(t, carol), map[string]string{}},
		{"the admin token", w.Token, map[string]string{"a-own": "admin", "a-own--t1": "admin", "shared": "admin", "other": "admin"}},
		{"the owner's PAT bound to a-own", bound, map[string]string{"a-own": "owner"}},
	} {
		slugs, levels, _ := w.list(t, c.cred, "include_forks=true&include_archived=true").page(t)
		got := map[string]string{}
		for i, slug := range slugs {
			got[slug] = levels[i]
		}
		if !maps.Equal(got, c.want) {
			t.Errorf("%s lists %v, want %v", c.caller, got, c.want)
		}
	}
}

func TestRead_ListLeavesOutArchivedWorkspacesAndForksUnlessAsked(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "live", Status: workspace.StatusReady})
	w.put(t, workspace.Workspace{Slug: "old", Status: workspace.StatusArchived})
	w.put(t, workspace.Workspace{Slug: "live--t1", Kind: access.KindFork, ParentSlug: new("live"), Status: workspace.StatusReady})
	for query, want := range map[string][]string{
		"":                      {"live"},
		"include_archived=true": {"live", "old"},
		"include_forks=true":    {"live", "live--t1"},
		"include_archived=true&include_forks=true":   {"live", "live--t1", "old"},
		"include_archived=false&include_forks=false": {"live"},
	} {
		slugs, _, _ := w.list(t, w.uKey, query).page(t)
		if slices.Sort(slugs); !slices.Equal(slugs, want) {
			t.Errorf("?%s lists %v, want %v", query, slugs, want)
		}
	}
}

func TestRead_ListWithAParentAnswersOnlyItsReadableForks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "live"})
	w.put(t, workspace.Workspace{Slug: "other"})
	for _, f := range []struct{ slug, parent string }{{"live--t1", "live"}, {"live--t2", "live"}, {"other--t1", "other"}} {
		w.put(t, workspace.Workspace{Slug: f.slug, Kind: access.KindFork, ParentSlug: new(f.parent)})
	}
	for _, c := range []struct {
		caller, cred, query string
		want                []string
	}{
		{"the owner", w.uKey, "parent=live", []string{"live--t1", "live--t2"}},
		{"the owner", w.uKey, "parent=live&include_forks=false", []string{"live--t1", "live--t2"}},
		{"the owner", w.uKey, "parent=no-such-slug", nil},
		{"a user who cannot read live", w.vKey, "parent=live", nil},
		{"the owner", w.uKey, "parent=live--t1", nil},
	} {
		if slugs, _, _ := w.list(t, c.cred, c.query).page(t); !slices.Equal(slugs, c.want) {
			t.Errorf("%s, ?%s lists %v, want %v", c.caller, c.query, slugs, c.want)
		}
	}
}

func TestRead_ListOrdersNewestFirstThenBySlugAcrossPages(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	second := w.clock().Truncate(time.Second)
	for _, r := range []struct {
		slug string
		age  time.Duration
	}{{"e", 2 * time.Second}, {"ab", time.Second}, {"aa", time.Second}, {"z", 0}, {"ac", time.Second}} {
		w.put(t, workspace.Workspace{Slug: r.slug, CreatedAt: second.Add(-r.age)})
	}
	all, _, next := w.list(t, w.uKey, "limit=200").page(t)
	if want := []string{"z", "aa", "ab", "ac", "e"}; !slices.Equal(all, want) || next != nil {
		t.Fatalf("one page lists %v with next_cursor %v, want %v and null", all, next, want)
	}
	var paged []string
	for query := "limit=2"; ; {
		slugs, _, next := w.list(t, w.uKey, query).page(t)
		paged = append(paged, slugs...)
		if next == nil {
			break
		}
		if *next == "" || len(paged) > len(all) {
			t.Fatalf("after %v: next_cursor %q", paged, *next)
		}
		query = "limit=2&cursor=" + url.QueryEscape(*next)
	}
	if !slices.Equal(paged, all) {
		t.Errorf("pages of 2 list %v, want %v", paged, all)
	}
}

func TestRead_ListRefusesABooleanOtherThanTrueOrFalse(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, query := range []string{"include_archived=yes", "include_archived=1", "include_forks=TRUE", "include_forks="} {
		if a := w.list(t, w.uKey, query); a.status != http.StatusBadRequest || a.errorType(t) != "invalid_request" {
			t.Errorf("?%s = %d %s, want 400 invalid_request", query, a.status, a.body)
		}
	}
}
