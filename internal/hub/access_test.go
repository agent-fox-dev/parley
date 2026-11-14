package hub_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

func TestAccess_ConfinesABoundTokenAndReportsItsRefusalsOnTheServedHub(t *testing.T) {
	t.Parallel()
	var events *event.Component
	h := hubtest.Start(t, func(c *hub.Composition) { events = c.Events() })
	alice := h.CreateUser(t, "alice")
	acme := h.CreateOrg(t, "acme")
	for _, slug := range []string{"w1", "w5"} {
		h.AddWorkspace(t, access.Record{Slug: slug, OwnerID: alice, OrgID: acme, Kind: access.KindRepository, Status: access.StatusReady})
		if _, err := events.EmitNow(context.Background(), event.Input{Type: "parley.change.open", Workspace: &slug,
			ActorType: "api_key", ActorID: &alice, ResourceType: "change", ResourceID: "1"}); err != nil {
			t.Fatal(err)
		}
	}
	bound := h.IssuePAT(t, identity.PATSpec{UserID: alice, Permissions: []string{"users:read", "audit:read"}, Workspace: "w1"})

	// The second refusal comes within the minute of the first one's event.
	for range 2 {
		status, body := h.Call(t, http.MethodGet, "/api/v1/user", bound, nil)
		var refused struct {
			Error struct {
				ErrorType string `json:"error_type"`
			} `json:"error"`
		}
		if status != http.StatusForbidden || json.Unmarshal(body, &refused) != nil || refused.Error.ErrorType != "token_bound" {
			t.Errorf("GET /user with a bound PAT = %d %s, want 403 token_bound", status, body)
		}
	}

	const refusedType = "parley.token.bound_refused"
	for _, tc := range []struct {
		name, credential string
		want             []string
	}{
		{"a bound PAT", bound, []string{"w1 parley.change.open", "w1 " + refusedType}},
		{"its user's API key", h.IssueKey(t, alice), []string{"w1 parley.change.open", "w5 parley.change.open", "w1 " + refusedType}},
	} {
		status, body := h.Call(t, http.MethodGet, "/api/v1/events/log", tc.credential, nil)
		var page struct {
			Items []struct {
				Workspace string         `json:"workspace"`
				Type      string         `json:"type"`
				ActorType string         `json:"actor_type"`
				Metadata  map[string]any `json:"metadata"`
			} `json:"items"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
			t.Errorf("GET /events/log with %s = %d %s, want 200 with a page", tc.name, status, body)
			continue
		}
		var got []string
		for _, it := range page.Items {
			got = append(got, it.Workspace+" "+it.Type)
			if it.Type == refusedType && (it.ActorType != "pat" || it.Metadata["route"] != "/api/v1/user" || it.Metadata["suppressed"] != 0.0) {
				t.Errorf("GET /events/log with %s: refusal event %+v, want the PAT's GET /api/v1/user with suppressed 0", tc.name, it)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("GET /events/log with %s: events %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAccess_MintsABoundTokenWithItsEventOnTheServedHub(t *testing.T) {
	t.Parallel()
	h := hubtest.Start(t, nil)
	alice := h.CreateUser(t, "alice")
	h.AddWorkspace(t, access.Record{Slug: "w1", OwnerID: alice, OrgID: h.CreateOrg(t, "acme"), Kind: access.KindRepository, Status: access.StatusReady})
	key := h.IssueKey(t, alice)

	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/w1/tokens", key, map[string]any{"name": "agent", "permissions": []string{"git:read"}})
	var tok struct {
		TokenID   string `json:"token_id"`
		Workspace string `json:"workspace"`
		Token     string `json:"token"`
	}
	if status != http.StatusCreated || json.Unmarshal(body, &tok) != nil || tok.Workspace != "w1" || tok.Token == "" {
		t.Fatalf("POST /workspaces/w1/tokens = %d %s, want 201 with a token bound to w1", status, body)
	}
	status, body = h.Call(t, http.MethodGet, "/api/v1/events/log", key, nil)
	var page struct {
		Items []struct {
			Type       string         `json:"type"`
			Workspace  string         `json:"workspace"`
			ActorType  string         `json:"actor_type"`
			ResourceID string         `json:"resource_id"`
			Action     string         `json:"action"`
			Metadata   map[string]any `json:"metadata"`
		} `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 {
		t.Fatalf("GET /events/log = %d %s, want one event", status, body)
	}
	if e := page.Items[0]; e.Type != "parley.token.bind" || e.Workspace != "w1" || e.ActorType != "api_key" ||
		e.ResourceID != tok.TokenID || e.Action != "bind" || e.Metadata["token_id"] != tok.TokenID {
		t.Errorf("event %+v, want the binding of %s to w1 by an API key", e, tok.TokenID)
	}
}
