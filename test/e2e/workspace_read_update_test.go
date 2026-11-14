package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// must runs parley against the hub with the developer's key and fails the
// test unless it exits 0.
func (h *workspaceHub) must(t *testing.T, args ...string) ran {
	t.Helper()
	r := h.run(t, args...)
	if r.code != 0 {
		t.Fatalf("parley %v exited %d; stderr = %q", args, r.code, r.stderr)
	}
	return r
}

// updateFields are the metadata.fields of each parley.workspace.update event
// of the workspace slug, oldest first.
func (h *workspaceHub) updateFields(t *testing.T, slug string) [][]string {
	t.Helper()
	var all [][]string
	for _, m := range h.events(t, "parley.workspace.update", slug) {
		var meta struct {
			Fields []string `json:"fields"`
		}
		if err := json.Unmarshal([]byte(m), &meta); err != nil {
			t.Fatalf("event metadata %s: %v", m, err)
		}
		all = append(all, meta.Fields)
	}
	return all
}

// decoded is body, which must be one JSON object.
func decoded(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body is not one JSON object (%v): %s", err, body)
	}
	return v
}

func TestWorkspaceReadUpdate_ADeveloperChangesAWorkspaceFromTheCLIAndReadsItBack(t *testing.T) {
	origin := newOrigin(t)
	h := startWorkspaceHub(t, origin, "demo")

	// The create waits by polling GET /workspaces/demo of the real hub
	// until its clone workers have made the workspace ready.
	created := h.must(t, "workspace", "create", "--slug", "demo", "--git-url", origin.URL(),
		"--wait", "--poll-interval", "1s", "--timeout", "30s").json(t)
	if created["status"] != "ready" {
		t.Fatalf("workspace create --wait printed status %v, want ready", created["status"])
	}

	updated := h.must(t, "workspace", "update", "demo", "--description", "payments service", "--setting", "land.strategy=squash")
	got := h.must(t, "workspace", "get", "demo")
	if updated.stdout != got.stdout {
		t.Errorf("update printed %s, get printed %s; want the same workspace", updated.stdout, got.stdout)
	}
	ws := got.json(t)
	if ws["description"] != "payments service" {
		t.Errorf("description = %v, want payments service", ws["description"])
	}
	var defaults workspace.SettingsRegistry
	if err := workspace.RegisterSettingKeys(&defaults); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{}
	raw, _ := json.Marshal(defaults.EffectiveSettings(nil, new("main")))
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	want["land.strategy"] = "squash"
	if settings := ws["settings"]; !reflect.DeepEqual(settings, want) {
		t.Errorf("settings = %v, want the defaults with land.strategy squash: %v", settings, want)
	}

	list := h.must(t, "workspace", "list")
	if slugs, access := itemValues(t, list, "slug"), itemValues(t, list, "access"); !reflect.DeepEqual(slugs, []any{"demo"}) || !reflect.DeepEqual(access, []any{"owner"}) {
		t.Errorf("list items have slugs %v and access %v, want [demo] and [owner]", slugs, access)
	}
	if fields := h.updateFields(t, "demo"); !reflect.DeepEqual(fields, [][]string{{"description", "settings"}}) {
		t.Errorf("update events of demo have fields %v, want one with [description settings]", fields)
	}
}

func TestWorkspaceReadUpdate_ADeveloperMovesAWorkspaceIntoAnOrganizationAndAMemberFindsIt(t *testing.T) {
	origin := newOrigin(t)
	h := startWorkspaceHub(t, origin, "demo")
	dev, err := identity.UserBySelector(context.Background(), h.Store.Main(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	member := h.CreateUser(t, "mem")
	memberKey := h.IssueKey(t, member)
	acme := h.CreateOrg(t, "acme")
	h.AddMember(t, acme, dev.ID)
	h.AddMember(t, acme, member)
	if status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key,
		map[string]any{"slug": "demo", "git_url": origin.URL()}); status != http.StatusCreated {
		t.Fatalf("create demo = %d %s, want 201", status, body)
	}
	h.waitStatus(t, "demo", workspace.StatusReady)
	demoOf := func(items any) map[string]any {
		list, _ := items.([]any)
		for _, it := range list {
			if item, _ := it.(map[string]any); item["slug"] == "demo" {
				return item
			}
		}
		return nil
	}

	_, body := h.Call(t, http.MethodGet, "/api/v1/workspaces", memberKey, nil)
	if item := demoOf(decoded(t, body)["items"]); item != nil {
		t.Fatalf("the member lists demo before the move: %s", body)
	}

	if status, body := h.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", h.key, map[string]any{"org_id": "acme"}); status != http.StatusOK {
		t.Fatalf("move demo to acme = %d %s, want 200", status, body)
	}

	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces", memberKey, nil)
	item := demoOf(decoded(t, body)["items"])
	if status != http.StatusOK || item == nil {
		t.Fatalf("the member's list = %d %s, want 200 with demo", status, body)
	}
	if hubURL, _ := item["hub_url"].(string); item["access"] != "member" || item["org_slug"] != "acme" || !strings.HasSuffix(hubURL, "/git/acme/demo.git") {
		t.Errorf("the member sees demo with access %v, org_slug %v, hub_url %v; want member, acme and a hub_url ending in /git/acme/demo.git",
			item["access"], item["org_slug"], item["hub_url"])
	}

	status, body = h.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", memberKey, map[string]any{"description": "mine"})
	if refusal, _ := decoded(t, body)["error"].(map[string]any); status != http.StatusForbidden || refusal["error_type"] != "forbidden" {
		t.Errorf("the member's update = %d %s, want 403 forbidden", status, body)
	}
	_, body = h.Call(t, http.MethodGet, "/api/v1/workspaces/demo", h.key, nil)
	if description := decoded(t, body)["description"]; description != "" {
		t.Errorf("description after the member's refused update = %v, want empty", description)
	}
	if fields := h.updateFields(t, "demo"); !reflect.DeepEqual(fields, [][]string{{"org_id"}}) {
		t.Errorf("update events of demo have fields %v, want one with [org_id]", fields)
	}
}
