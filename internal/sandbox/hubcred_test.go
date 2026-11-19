package sandbox_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestCreate_MintsHubCredential(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	ctx := context.Background()
	before := h.tokenIDs(t)

	a := h.post(t, "demo--t1", map[string]any{"ttl": "1h"})
	s := sandboxObject(t, a)
	id := s["id"].(string)
	expires := stamp(recordStart.Add(time.Hour))
	ha, _ := s["hub_access"].(map[string]any)
	if ha == nil || ha["expires_at"] != expires || s["expires_at"] != expires {
		t.Errorf("hub_access = %#v, expires_at %v; want hub_access expiring with the sandbox at %s", s["hub_access"], s["expires_at"], expires)
	}
	minted := h.newTokens(t, before)
	if len(minted) != 1 {
		t.Fatalf("the create minted %d tokens; want 1", len(minted))
	}
	tok := minted[0]
	if tok.Workspace != "demo--t1" || tok.UserID != h.dana || tok.ExpiresAt == nil || stamp(*tok.ExpiresAt) != expires || tok.RevokedAt != nil {
		t.Errorf("token %+v; want a live token bound to demo--t1, owned by dana, expiring at %s", tok, expires)
	}
	if got := slices.Sorted(slices.Values(tok.Permissions)); ha == nil || !reflect.DeepEqual(jsonList(got), ha["scopes"]) {
		t.Errorf("token permissions %v; want the scopes of hub_access %v", got, ha)
	}
	if n := h.bindEvents(t, tok.TokenID); n != 1 {
		t.Errorf("parley.token.bind events of %s = %d; want 1", tok.TokenID, n)
	}
	rec, err := sandbox.Get(ctx, h.Store.Main(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.HubTokenID != tok.TokenID {
		t.Errorf("the record keeps token %q; want %q", rec.HubTokenID, tok.TokenID)
	}

	// The value goes to the provisioner, in memory.
	values := h.minter.minted()
	if len(values) != 1 || values[0] == "" {
		t.Fatalf("minted values %d; want 1", len(values))
	}
	if got, ok := h.svc.HubCredential(id); !ok || got != values[0] {
		t.Errorf("HubCredential(%s) = %t; want the minted value", id, ok)
	}
}

// The provisioner finds a sandbox as soon as its create commits: the hub
// credential's value is held before then, so that the provisioner does not
// take the sandbox for one an earlier process created, and it is dropped
// with a create that does not commit.
func TestCreate_HoldsHubCredentialBeforeTheCommit(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	for _, tc := range []struct {
		name string
		fail error
	}{
		{"a create that commits", nil},
		{"a create that rolls back", errors.New("the session store is gone")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				id   string
				held bool
			)
			h.linker.setOnRecord(func(sandboxID string) {
				id = sandboxID
				_, held = h.svc.HubCredential(sandboxID)
			})
			h.linker.setFail(tc.fail)
			t.Cleanup(func() { h.linker.setOnRecord(nil); h.linker.setFail(nil) })
			a := h.post(t, "demo--t1", map[string]any{})
			if id == "" {
				t.Fatalf("create = %d %s; the session link was not recorded", a.status, a.body)
			}
			if !held {
				t.Error("the hub credential's value was not held while the creating transaction was open")
			}
			value, kept := h.svc.HubCredential(id)
			switch {
			case tc.fail == nil && (a.status != http.StatusCreated || !kept || !slices.Contains(h.minter.minted(), value)):
				t.Errorf("create = %d, value kept %t; want 201 and the minted value kept", a.status, kept)
			case tc.fail != nil && (a.status != http.StatusInternalServerError || kept):
				t.Errorf("create = %d, value kept %t; want 500 and the value dropped", a.status, kept)
			}
		})
	}
}

func TestHubCredential_ScopesAndPaths(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	pat := func(paths []string, permissions ...string) string {
		return h.IssuePAT(t, identity.PATSpec{UserID: h.dana, Permissions: permissions, Paths: paths})
	}
	// Each of these hub credentials can clone the repository, git:write
	// alone too, so each sandbox gets a checkout.
	cases := []struct {
		name       string
		cred       string
		wantScopes []string
		wantPaths  []string
	}{
		{"an API key", h.key, allHubScopes, nil},
		{"a PAT with every scope", pat(nil, "sandboxes:write", "git:read", "git:write", "changes:read", "changes:write", "sessions:write", "audit:write"), allHubScopes, nil},
		{"a PAT with some", pat(nil, "sandboxes:write", "git:read", "changes:read"), []string{"changes:read", "git:read"}, nil},
		{"a PAT with git:write and others", pat(nil, "sandboxes:write", "git:write", "secrets:read", "workspaces:read"), []string{"git:write"}, nil},
		{"a PAT limited to paths", pat([]string{"src/**", "docs/**"}, "sandboxes:write", "git:read", "git:write"), []string{"git:read", "git:write"}, []string{"src/**", "docs/**"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.tokenIDs(t)
			s := sandboxObject(t, h.call(t, http.MethodPost, "/workspaces/demo--t1/sandboxes", tc.cred, map[string]any{}))
			ha, _ := s["hub_access"].(map[string]any)
			if ha == nil || !reflect.DeepEqual(ha["scopes"], jsonList(tc.wantScopes)) || !reflect.DeepEqual(ha["paths"], jsonList(tc.wantPaths)) {
				t.Errorf("hub_access = %#v; want scopes %v and paths %v", s["hub_access"], tc.wantScopes, tc.wantPaths)
			}
			if s["checkout"] == nil {
				t.Error("checkout = null; want one")
			}
			minted := h.newTokens(t, before)
			if len(minted) != 1 {
				t.Fatalf("the create minted %d tokens; want 1", len(minted))
			}
			if got := slices.Sorted(slices.Values(minted[0].Permissions)); !slices.Equal(got, tc.wantScopes) || !reflect.DeepEqual(minted[0].Paths, tc.wantPaths) {
				t.Errorf("token permissions %v, paths %v; want %v, %v", got, minted[0].Paths, tc.wantScopes, tc.wantPaths)
			}
		})
	}
}

// noHubProfile is a profile without hub access.
const noHubProfile = "[[sandbox.profiles]]\nname = \"plain\"\nttl = \"4h\"\nhub_access = \"none\"\n"

func TestCreate_WithoutHubCredential(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile+noHubProfile)
	h.world(t)
	// [server].external_url must be set for sandboxes to be on, so a hub
	// without it is one whose create is given none.
	noURL := startCreateHub(t, agentProfile, func(d *sandbox.Deps) { d.ExternalURL = "" })
	noURL.world(t)
	cases := []struct {
		name    string
		hub     *createHub
		profile string
		cred    string
	}{
		{"a profile without hub access", h, "plain", h.key},
		{"a credential without a hub scope", h, "agent", h.IssuePAT(t, identity.PATSpec{UserID: h.dana, Permissions: []string{"sandboxes:write"}})},
		{"a hub without an external URL", noURL, "agent", noURL.key},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.hub.footprint(t)
			s := sandboxObject(t, tc.hub.call(t, http.MethodPost, "/workspaces/demo--t1/sandboxes", tc.cred, map[string]any{"profile": tc.profile}))
			if s["hub_access"] != nil || s["checkout"] != nil {
				t.Errorf("hub_access = %#v, checkout = %#v; want both null", s["hub_access"], s["checkout"])
			}
			if after := tc.hub.footprint(t); after.tokens != before.tokens {
				t.Errorf("the create added %d tokens; want none", after.tokens-before.tokens)
			}
			if n := tc.hub.bindEvents(t, ""); n != 0 {
				t.Errorf("parley.token.bind events = %d; want none", n)
			}
		})
	}
}

func TestCreate_MintFailureRollsBack(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	before := h.footprint(t)
	h.minter.setErr(errors.New("the minting component failed"))
	a := h.post(t, "demo--t1", map[string]any{})
	if a.status != http.StatusInternalServerError || a.err.ErrorType != "internal" {
		t.Errorf("create with a failing minting component = %d %s; want 500 internal", a.status, a.body)
	}
	if after := h.footprint(t); after != before {
		t.Errorf("a create whose mint failed left %+v; want %+v", after, before)
	}
	h.minter.setErr(nil)
	sandboxObject(t, h.post(t, "demo--t1", map[string]any{}))
}
