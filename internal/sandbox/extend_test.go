package sandbox_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The extend route's behaviours share one hub: the repository demo and its
// fork demo--t3, which expires three hours after the requests, each row
// with a sandbox of its own.
func TestExtendRoute(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.repository(t, "demo", workspace.StatusReady)
	// The requests come a minute after the sandboxes were made, so that a
	// new expires_at is counted from the request.
	h.clock.Advance(time.Minute)
	now := h.clock.Now()
	h.fork(t, "demo--t3", "demo", now.Add(3*time.Hour), nil)
	path := func(slug, id string) string { return "/workspaces/" + slug + "/sandboxes/" + id }
	// sandboxOf stores a sandbox of slug in state with the profile agent's
	// ttl and hub access, the purpose old and the label keep, and mints its
	// hub credential when it is ready.
	sandboxOf := func(t *testing.T, slug, state string) string {
		id := h.seedRecord(t, slug, state, func(r *sandbox.Record) {
			r.Purpose, r.Labels = "old", map[string]string{"keep": "me"}
			r.HubAccess = &sandbox.HubAccess{Scopes: allHubScopes, ExpiresAt: r.ExpiresAt}
			r.Snapshot = sandbox.Snapshot{TTL: 4 * time.Hour, HubAccess: "fork"}
		})
		if state == sandbox.StateReady {
			h.hubCredential(t, id)
		}
		return id
	}
	tokenExpiry := func(t *testing.T, tokenID string) *time.Time {
		pat, err := identity.PATByID(context.Background(), h.Store.Main(), tokenID)
		if err != nil {
			t.Fatalf("the hub credential's token: %v", err)
		}
		return pat.ExpiresAt
	}
	events := func(t *testing.T, id string) int {
		return h.count(t, `SELECT count(*) FROM hub_events WHERE resource_id = ?`, id)
	}

	t.Run("refuses a body it cannot apply and changes nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body map[string]any
		}{
			{"a ttl under a minute", map[string]any{"ttl": "59s"}},
			{"a ttl over the profile's", map[string]any{"ttl": "241m"}},
			{"a ttl of two units", map[string]any{"ttl": "4h1m"}},
			{"a ttl that is no duration", map[string]any{"ttl": "soon"}},
			{"a ttl as a number", map[string]any{"ttl": 3600}},
			{"a purpose over 200 characters", map[string]any{"purpose": strings.Repeat("x", 201)}},
			{"17 labels", map[string]any{"labels": entries(17)}},
			{"a reserved label key", map[string]any{"labels": map[string]string{"parley.x": "a"}}},
			{"a label key that begins with a dash", map[string]any{"labels": map[string]string{"-bad": "a"}}},
			{"a label value over 256 bytes", map[string]any{"labels": map[string]string{"k": strings.Repeat("v", 257)}}},
			{"a valid purpose beside a bad ttl", map[string]any{"purpose": "fine", "ttl": "soon"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id := sandboxOf(t, "demo--t3", sandbox.StateReady)
				before := storedRecord(t, h.Hub, id)
				expiry := tokenExpiry(t, before.HubTokenID)

				a := h.call(t, http.MethodPatch, path("demo--t3", id), h.key, tc.body)
				if a.status != http.StatusBadRequest || a.err.ErrorType != "invalid_request" {
					t.Fatalf("PATCH = %d %s; want 400 invalid_request", a.status, a.body)
				}
				if after := storedRecord(t, h.Hub, id); !reflect.DeepEqual(after, before) {
					t.Errorf("the record became %+v; want %+v", after, before)
				}
				if got := tokenExpiry(t, before.HubTokenID); !reflect.DeepEqual(got, expiry) {
					t.Errorf("the hub credential expires at %v; want %v", got, expiry)
				}
			})
		}
		t.Run("a deleted sandbox", func(t *testing.T) {
			id := sandboxOf(t, "demo--t3", sandbox.StateDeleted)
			before := storedRecord(t, h.Hub, id)
			a := h.call(t, http.MethodPatch, path("demo--t3", id), h.key, map[string]any{"ttl": "1h"})
			if a.status != http.StatusConflict || a.err.ErrorType != "sandbox_deleted" {
				t.Fatalf("PATCH = %d %s; want 409 sandbox_deleted", a.status, a.body)
			}
			if after := storedRecord(t, h.Hub, id); !reflect.DeepEqual(after, before) {
				t.Errorf("the record became %+v; want %+v", after, before)
			}
		})
	})

	t.Run("sets the members it is given and leaves the others", func(t *testing.T) {
		long := strings.Repeat("x", 200)
		for _, tc := range []struct {
			name, slug string
			body       map[string]any
			// expires is the expires_at the answer has, nil for the one it
			// had; purpose and labels are nil for those it had.
			expires *time.Time
			purpose *string
			labels  map[string]string
		}{
			{name: "a ttl of a minute", slug: "demo--t3", body: map[string]any{"ttl": "1m"}, expires: new(now.Add(time.Minute))},
			{name: "a ttl that ends before the fork", slug: "demo--t3", body: map[string]any{"ttl": "2h"}, expires: new(now.Add(2 * time.Hour))},
			{name: "the profile's ttl, cut to the fork's expiry", slug: "demo--t3", body: map[string]any{"ttl": "4h"}, expires: new(now.Add(3 * time.Hour))},
			{name: "the profile's ttl on a repository", slug: "demo", body: map[string]any{"ttl": "4h"}, expires: new(now.Add(4 * time.Hour))},
			{name: "a purpose of 200 characters", slug: "demo--t3", body: map[string]any{"purpose": long}, purpose: &long},
			{name: "labels", slug: "demo--t3", body: map[string]any{"labels": map[string]string{"team": "a"}}, labels: map[string]string{"team": "a"}},
			{name: "a ttl and a purpose", slug: "demo--t3", body: map[string]any{"ttl": "30m", "purpose": "p"}, expires: new(now.Add(30 * time.Minute)), purpose: new("p")},
		} {
			for _, state := range []string{sandbox.StateReady, sandbox.StateStopped} {
				t.Run(tc.name+"/"+state, func(t *testing.T) {
					id := sandboxOf(t, tc.slug, state)
					before := storedRecord(t, h.Hub, id)
					evs := events(t, id)

					a := h.call(t, http.MethodPatch, path(tc.slug, id), h.key, tc.body)
					var got struct {
						State     string            `json:"state"`
						ExpiresAt time.Time         `json:"expires_at"`
						UpdatedAt time.Time         `json:"updated_at"`
						Purpose   string            `json:"purpose"`
						Labels    map[string]string `json:"labels"`
						HubAccess *struct {
							ExpiresAt time.Time `json:"expires_at"`
						} `json:"hub_access"`
					}
					if a.status != http.StatusOK || json.Unmarshal(a.body, &got) != nil || got.State != state {
						t.Fatalf("PATCH = %d %s; want 200 with the sandbox %s", a.status, a.body, state)
					}
					want := struct {
						expires time.Time
						purpose string
						labels  map[string]string
					}{before.ExpiresAt, before.Purpose, before.Labels}
					if tc.expires != nil {
						want.expires = *tc.expires
					}
					if tc.purpose != nil {
						want.purpose = *tc.purpose
					}
					if tc.labels != nil {
						want.labels = tc.labels
					}
					if !got.ExpiresAt.Equal(want.expires) || got.Purpose != want.purpose || !maps.Equal(got.Labels, want.labels) || !got.UpdatedAt.Equal(now) {
						t.Errorf("expires_at %v, purpose %q, labels %v, updated_at %v; want %v, %q, %v, %v",
							got.ExpiresAt, got.Purpose, got.Labels, got.UpdatedAt, want.expires, want.purpose, want.labels, now)
					}
					if got.HubAccess == nil || !got.HubAccess.ExpiresAt.Equal(got.ExpiresAt) {
						t.Errorf("hub_access %+v; want it to expire with the sandbox at %v", got.HubAccess, got.ExpiresAt)
					}
					if stored := storedRecord(t, h.Hub, id); !stored.ExpiresAt.Equal(want.expires) || stored.Purpose != want.purpose {
						t.Errorf("the stored record expires at %v with purpose %q; want %v, %q", stored.ExpiresAt, stored.Purpose, want.expires, want.purpose)
					}
					if state == sandbox.StateReady {
						if at := tokenExpiry(t, before.HubTokenID); at == nil || !at.Equal(want.expires) {
							t.Errorf("the hub credential expires at %v; want %v", at, want.expires)
						}
					}
					if n := events(t, id); n != evs {
						t.Errorf("%d events about the sandbox were written; want none", n-evs)
					}
				})
			}
		}
	})
}
