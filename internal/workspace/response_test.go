package workspace_test

import (
	"context"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// timestamp is the form of every time in a workspace object.
var timestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

func TestResponse_HasEveryMemberWithNullForNoValue(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	sha, branch, synced := "0123456789abcdef0123456789abcdef01234567", "main", w.clock().Add(-time.Hour)
	auto, purpose := true, "fix the build"
	w.put(t, workspace.Workspace{Slug: "plain"})
	w.put(t, workspace.Workspace{Slug: "synced", Status: workspace.StatusReady, DefaultBranch: &branch,
		HeadSHA: &sha, LastSyncAt: &synced})
	w.put(t, workspace.Workspace{Slug: "task", Kind: access.KindFork, ParentSlug: new("synced"),
		Status: workspace.StatusReady, ForkPoint: &workspace.ForkPoint{Ref: "refs/heads/main", SHA: sha},
		ExpiresAt: new(w.clock().Add(time.Hour)), AutoChange: &auto, Purpose: &purpose})

	nullable := []string{"default_branch", "head_sha", "origin_head_sha", "clone_error", "last_sync_at",
		"sync_error", "upstream_url", "integration_branch", "upstream_head_sha", "parent_slug", "fork_point",
		"expires_at", "auto_change", "purpose", "session_id"}
	for _, c := range []struct {
		slug    string
		null    []string // the members that must be null
		present []string // the members that must have a value
	}{
		{"plain", nullable, nil},
		{"synced", slices.DeleteFunc(slices.Clone(nullable), func(m string) bool {
			return m == "default_branch" || m == "head_sha" || m == "last_sync_at"
		}), []string{"default_branch", "head_sha", "last_sync_at"}},
		{"task", []string{"origin_head_sha", "clone_error", "last_sync_at", "sync_error", "upstream_url",
			"integration_branch", "upstream_head_sha", "session_id"},
			[]string{"parent_slug", "fork_point", "expires_at", "auto_change", "purpose"}},
	} {
		obj := w.get(t, w.uKey, c.slug).decode(t, http.StatusOK)
		if got := slices.Sorted(maps.Keys(obj)); !slices.Equal(got, slices.Sorted(slices.Values(objectMembers))) {
			t.Errorf("%s: members = %v, want exactly %v", c.slug, got, objectMembers)
		}
		for _, m := range c.null {
			if v, ok := obj[m]; !ok || v != nil {
				t.Errorf("%s: %s = %#v (present %v), want null", c.slug, m, v, ok)
			}
		}
		for _, m := range c.present {
			if obj[m] == nil {
				t.Errorf("%s: %s is null, want a value", c.slug, m)
			}
		}
		for _, m := range []string{"created_at", "updated_at", "last_sync_at", "expires_at"} {
			if s, ok := obj[m].(string); obj[m] != nil && (!ok || !timestamp.MatchString(s)) {
				t.Errorf("%s: %s = %#v, want the form 2026-01-02T03:04:05Z", c.slug, m, obj[m])
			}
		}
	}
}

func TestResponse_ShowsTheOrgSlugAndTheCallersLevelWhateverTheScopes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", OrgID: w.acme(t)})
	for _, c := range []struct {
		caller, cred, access string
	}{
		{"the admin token", w.Token, "admin"},
		{"the owner with an API key", w.uKey, "owner"},
		{"the owner with a PAT that carries only workspaces:read", w.pat(t, w.u, "workspaces:read"), "owner"},
		{"a member of the org", w.vKey, "member"},
	} {
		obj := w.get(t, c.cred, "demo").decode(t, http.StatusOK)
		if obj["org_slug"] != "acme" || obj["access"] != c.access {
			t.Errorf("%s: org_slug %#v, access %#v; want acme, %s", c.caller, obj["org_slug"], obj["access"], c.access)
		}
	}
}

func TestResponse_ShowsTheHubURLUnderTheExternalURL(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", OrgID: w.acme(t)})
	rec, _ := w.workspace(t, "demo")
	for _, c := range []struct {
		externalURL string
		want        string // "" for null
	}{
		{"", ""},
		{"https://hub.example", "https://hub.example/git/acme/demo.git"},
		{"https://hub.example/", "https://hub.example/git/acme/demo.git"},
		{"https://hub.example/parley/", "https://hub.example/parley/git/acme/demo.git"},
	} {
		var svc workspace.Service
		svc.Bind(workspace.Deps{Store: w.Store, ExternalURL: c.externalURL})
		obj, err := svc.RenderWorkspace(context.Background(), rec, access.LevelOwner)
		switch {
		case err != nil:
			t.Errorf("render under %q: %v", c.externalURL, err)
		case c.want == "" && obj.HubURL != nil, c.want != "" && (obj.HubURL == nil || *obj.HubURL != c.want):
			t.Errorf("hub_url under %q = %v, want %q", c.externalURL, obj.HubURL, c.want)
		}
	}
}
