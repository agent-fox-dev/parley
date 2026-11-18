package sandbox_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
)

// allHubScopes are the six scopes a hub credential can carry, sorted.
var allHubScopes = []string{"audit:write", "changes:read", "changes:write", "git:read", "git:write", "sessions:write"}

// jsonList is list as a decoded answer holds it: nil for nil.
func jsonList(list []string) any {
	if list == nil {
		return nil
	}
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}

// bindEvents counts the parley.token.bind events of the token id, or of
// every token for "".
func (h *createHub) bindEvents(t *testing.T, id string) int {
	t.Helper()
	return h.count(t, `SELECT count(*) FROM hub_events WHERE type = 'parley.token.bind' AND (? = '' OR resource_id = ?)`, id, id)
}

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

	// The value goes to the provisioner, in memory, and to the sandbox's
	// scrubbing, and nowhere else.
	values := h.minter.minted()
	if len(values) != 1 || values[0] == "" {
		t.Fatalf("minted values %d; want 1", len(values))
	}
	value := values[0]
	if got, ok := h.svc.HubCredential(id); !ok || got != value {
		t.Errorf("HubCredential(%s) = %t; want the minted value", id, ok)
	}
	if got := h.svc.ScrubSandbox(ctx, id, "token "+value); got != "token [redacted]" {
		t.Errorf("ScrubSandbox kept the hub credential: %q", strings.ReplaceAll(got, value, "<value>"))
	}
	all := strings.Join([]string{string(a.body), dump(t, h.Store.Main(), "hub_events"), dump(t, h.Store.Main(), "sandboxes"), h.Log.String()}, "\n")
	if strings.Contains(all, value) {
		t.Error("the hub credential's value is in the answer, an event, the record or the log")
	}
}

func TestHubCredential_ScopesAndPaths(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	pat := func(paths []string, permissions ...string) string {
		return h.IssuePAT(t, identity.PATSpec{UserID: h.dana, Permissions: permissions, Paths: paths})
	}
	cases := []struct {
		name       string
		cred       string
		wantScopes []string
		wantPaths  []string
		checkout   bool
	}{
		{"an API key", h.key, allHubScopes, nil, true},
		{"a PAT with every scope", pat(nil, "sandboxes:write", "git:read", "git:write", "changes:read", "changes:write", "sessions:write", "audit:write"), allHubScopes, nil, true},
		{"a PAT with some", pat(nil, "sandboxes:write", "git:read", "changes:read"), []string{"changes:read", "git:read"}, nil, true},
		{"a PAT with git:write and others", pat(nil, "sandboxes:write", "git:write", "secrets:read", "workspaces:read"), []string{"git:write"}, nil, true},
		{"a PAT limited to paths", pat([]string{"src/**", "docs/**"}, "sandboxes:write", "git:read", "git:write"), []string{"git:read", "git:write"}, []string{"src/**", "docs/**"}, true},
		// A hub credential that cannot read the repository cannot clone it.
		{"a PAT without git", pat(nil, "sandboxes:write", "changes:read"), []string{"changes:read"}, nil, false},
	}
	var limited string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.tokenIDs(t)
			s := sandboxObject(t, h.call(t, http.MethodPost, "/workspaces/demo--t1/sandboxes", tc.cred, map[string]any{}))
			ha, _ := s["hub_access"].(map[string]any)
			if ha == nil || !reflect.DeepEqual(ha["scopes"], jsonList(tc.wantScopes)) || !reflect.DeepEqual(ha["paths"], jsonList(tc.wantPaths)) {
				t.Errorf("hub_access = %#v; want scopes %v and paths %v", s["hub_access"], tc.wantScopes, tc.wantPaths)
			}
			if (s["checkout"] != nil) != tc.checkout {
				t.Errorf("checkout = %#v; want one: %t", s["checkout"], tc.checkout)
			}
			minted := h.newTokens(t, before)
			if len(minted) != 1 {
				t.Fatalf("the create minted %d tokens; want 1", len(minted))
			}
			if got := slices.Sorted(slices.Values(minted[0].Permissions)); !slices.Equal(got, tc.wantScopes) || !reflect.DeepEqual(minted[0].Paths, tc.wantPaths) {
				t.Errorf("token permissions %v, paths %v; want %v, %v", got, minted[0].Paths, tc.wantScopes, tc.wantPaths)
			}
			if tc.wantPaths != nil {
				limited = s["id"].(string)
			}
		})
	}

	// A replacement, as a start or a resume mints it, keeps the scopes and
	// paths of the stored hub credential, whose token the same transaction
	// revokes.
	t.Run("a replacement keeps them", func(t *testing.T) {
		ctx := context.Background()
		rec, err := sandbox.Get(ctx, h.Store.Main(), limited)
		if err != nil {
			t.Fatal(err)
		}
		expires := recordStart.Add(2 * time.Hour)
		var pat identity.PAT
		h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
			if err := h.svc.RevokeHubCredential(ctx, tx, rec); err != nil {
				return err
			}
			pat, _, err = h.svc.MintHubCredential(ctx, tx, rec, expires)
			return err
		})
		old, err := identity.PATByID(ctx, h.Store.Main(), rec.HubTokenID)
		if err != nil || old.RevokedAt == nil {
			t.Errorf("the stored token %s: %+v, %v; want it revoked", rec.HubTokenID, old, err)
		}
		if pat.TokenID == rec.HubTokenID || pat.Workspace != "demo--t1" || pat.UserID != h.dana || pat.ExpiresAt == nil || !pat.ExpiresAt.Equal(expires) ||
			!slices.Equal(slices.Sorted(slices.Values(pat.Permissions)), rec.HubAccess.Scopes) || !slices.Equal(pat.Paths, rec.HubAccess.Paths) {
			t.Errorf("replacement %+v; want a new token of dana bound to demo--t1, expiring at %s, with %+v", pat, expires, rec.HubAccess)
		}
		if n := h.bindEvents(t, pat.TokenID); n != 1 {
			t.Errorf("parley.token.bind events of the replacement = %d; want 1", n)
		}
	})
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
