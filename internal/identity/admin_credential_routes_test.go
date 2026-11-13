package identity_test

import (
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// credentialHub is a team hub where alice has, besides her live API key ku
// and her live PAT pu, a revoked API key and a revoked PAT bound to a
// workspace, created an hour after ku and pu, and an expired PAT, created
// an hour after those.
type credentialHub struct {
	teamHub
	keys, tokens []string // the ids of alice's keys and tokens, newest first
	expired      string   // the value of alice's expired PAT
}

func newCredentialHub(t *testing.T) credentialHub {
	t.Helper()
	h := credentialHub{teamHub: newTeamHub(t)}
	h.advance(time.Hour)
	_, revokedKey := h.seedKey(h.alice.ID, 0)
	h.revoke(revokedKey.KeyID)
	_, revokedToken := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, Workspace: "my-ws"})
	h.revoke(revokedToken.TokenID)
	h.advance(time.Hour)
	at := h.now()
	expired, expiredToken := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, ExpiresAt: &at})
	h.advance(time.Hour)
	h.keys = []string{revokedKey.KeyID, h.kuID}
	h.tokens = []string{expiredToken.TokenID, revokedToken.TokenID, h.puID}
	h.expired = "Bearer " + expired
	return h
}

func TestAdminListCredentials_AnswersEveryKeyAndTokenOfTheSelectedUserNewestFirst(t *testing.T) {
	t.Parallel()
	h := newCredentialHub(t)
	for _, caller := range []string{h.admin, h.ka} {
		for _, sel := range []string{h.alice.ID, "alice@example.com", "alice"} {
			keys := h.get("/api/v1/users/"+sel+"/keys", caller)
			if got := itemValues(t, keys, "key_id"); keys.status != http.StatusOK || !slices.Equal(got, h.keys) {
				t.Errorf("GET /users/%s/keys = %d, key ids %v, want 200 with %v", sel, keys.status, got, h.keys)
			}
			for _, item := range keys.json["items"].([]any) {
				if got := slices.Sorted(maps.Keys(item.(map[string]any))); !slices.Equal(got, keyMembers) {
					t.Errorf("GET /users/%s/keys: item members %v, want %v", sel, got, keyMembers)
				}
			}
			tokens := h.get("/api/v1/users/"+sel+"/tokens", caller)
			if got := itemValues(t, tokens, "token_id"); tokens.status != http.StatusOK || !slices.Equal(got, h.tokens) {
				t.Errorf("GET /users/%s/tokens = %d, token ids %v, want 200 with %v", sel, tokens.status, got, h.tokens)
			}
			for _, item := range tokens.json["items"].([]any) {
				if _, has := item.(map[string]any)["token"]; has {
					t.Errorf("GET /users/%s/tokens: item %v has a token member", sel, item)
				}
			}
		}
	}
}

func TestAdminRevokeCredential_RevokesTheSelectedUsersKeyOrTokenOnce(t *testing.T) {
	t.Parallel()
	h := newCredentialHub(t)
	for _, c := range []struct {
		name, path, value string
	}{
		{"a live key", "/api/v1/users/alice/keys/" + h.kuID, h.ku},
		{"a live token", "/api/v1/users/alice/tokens/" + h.puID, h.pu},
		{"an expired token", "/api/v1/users/alice/tokens/" + h.tokens[0], h.expired},
	} {
		now := h.now().Format(time.RFC3339)
		first := h.call(http.MethodDelete, c.path, h.admin, nil)
		if first.status != http.StatusOK || first.json["revoked_at"] != now {
			t.Errorf("%s: DELETE = %d %s, want 200 with revoked_at %s", c.name, first.status, first.body, now)
		}
		if got := h.get("/api/v1/user", c.value).status; got != http.StatusUnauthorized {
			t.Errorf("%s: GET /user with the revoked credential = %d, want 401", c.name, got)
		}
		h.advance(time.Hour)
		if second := h.call(http.MethodDelete, c.path, h.ka, nil); second.status != http.StatusOK || second.json["revoked_at"] != now {
			t.Errorf("%s: DELETE again = %d %s, want 200 with revoked_at %s", c.name, second.status, second.body, now)
		}
	}
}

func TestAdminCredentialRoutes_AnswerNotFoundForAnotherUsersOrAMissingTarget(t *testing.T) {
	t.Parallel()
	h := newCredentialHub(t)
	bob := h.createUser("bob", identity.RoleUser).ID
	_, bobKey := h.seedCredential("API key", bob)
	_, bobToken := h.seedCredential("PAT", bob)
	for _, c := range []struct {
		method, path, message string
	}{
		{http.MethodGet, "/api/v1/users/nobody/keys", "user not found"},
		{http.MethodGet, "/api/v1/users/nobody/tokens", "user not found"},
		{http.MethodDelete, "/api/v1/users/nobody/keys/" + h.kuID, "user not found"},
		{http.MethodDelete, "/api/v1/users/alice/keys/zzzzzzzz", "API key not found"},
		{http.MethodDelete, "/api/v1/users/alice/keys/" + bobKey, "API key not found"},
		{http.MethodDelete, "/api/v1/users/alice/tokens/zzzzzzzz", "token not found"},
		{http.MethodDelete, "/api/v1/users/alice/tokens/" + bobToken, "token not found"},
	} {
		before := h.snapshot()
		checkRefusal(t, c.method+" "+c.path, h.call(c.method, c.path, h.admin, nil), http.StatusNotFound, "not_found", c.message)
		if h.snapshot() != before {
			t.Errorf("%s %s: the store changed", c.method, c.path)
		}
	}
}
