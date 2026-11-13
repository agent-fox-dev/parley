package identity_test

import (
	"bytes"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// keyMembers are the members of the Key object, sorted.
var keyMembers = []string{"created_at", "expires_at", "key_id", "revoked_at", "user_id"}

// keyPath is the path of the caller's API key keyID.
func keyPath(keyID string) string { return "/api/v1/user/keys/" + keyID }

// refreshPath is the path that refreshes the caller's API key keyID.
func refreshPath(keyID string) string { return keyPath(keyID) + "/refresh" }

func TestListKeys_AnswersEveryKeyOfTheCallerNewestFirst(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	h.advance(time.Hour)
	// The revoked and the expired key are created at the same instant, so
	// the id decides their order.
	_, revoked := h.seedKey(h.alice.ID, 0)
	h.revoke(revoked.KeyID)
	_, expired := h.seedKey(h.alice.ID, 30)
	h.expireAt(expired.KeyID, h.now())
	h.seedKey(h.seedUser("bob").ID, 0)
	later, earlier := revoked.KeyID, expired.KeyID
	if later < earlier {
		later, earlier = earlier, later
	}
	want := []string{later, earlier, h.keyRec.KeyID}

	r := h.get("/api/v1/user/keys", "Bearer "+h.key)
	if got := itemValues(t, r, "key_id"); r.status != http.StatusOK || !slices.Equal(got, want) {
		t.Fatalf("GET /user/keys = %d, key ids %v, want 200 with %v", r.status, got, want)
	}
	for _, item := range r.json["items"].([]any) {
		obj := item.(map[string]any)
		if got := slices.Sorted(maps.Keys(obj)); !slices.Equal(got, keyMembers) {
			t.Errorf("item members %v, want %v", got, keyMembers)
		}
		if stored := asJSON(t, h.storedKey(obj["key_id"].(string))); !reflect.DeepEqual(item, stored) {
			t.Errorf("item %v, want the stored key %v", item, stored)
		}
	}
	if first := r.json["items"].([]any)[2].(map[string]any); first["revoked_at"] != nil || first["expires_at"] == nil {
		t.Errorf("the live key = %v, want revoked_at null and an expires_at", first)
	}
	if got, requests := h.walkBy("/api/v1/user/keys", "Bearer "+h.key, 2, "key_id"); !slices.Equal(got, want) || requests != 2 {
		t.Errorf("pages of 2 = %v in %d requests, want %v in 2", got, requests, want)
	}
	for _, scope := range []string{"keys:read", "keys:manage"} {
		pat, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{scope}})
		if r := h.get("/api/v1/user/keys", "Bearer "+pat); r.status != http.StatusOK {
			t.Errorf("a PAT with %s: GET /user/keys = %d %s, want 200", scope, r.status, r.body)
		}
	}
	checkRefusal(t, "a PAT with git:read", h.get("/api/v1/user/keys", "Bearer "+h.pat),
		http.StatusForbidden, "insufficient_scope", "this route needs the scope keys:read")
}

func TestRefreshKey_ReplacesTheSecretAndRestartsTheLifetime(t *testing.T) {
	t.Parallel()
	for _, days := range []int{30, 0} {
		h := newAliceHub(t)
		old, rec := h.seedKey(h.alice.ID, days)
		h.advance(10 * 24 * time.Hour)
		var wantExpiry any
		if days > 0 {
			wantExpiry = h.now().Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		}

		r := h.call(http.MethodPost, refreshPath(rec.KeyID), "Bearer "+old, nil)
		key, _ := r.json["key"].(string)
		value := regexp.MustCompile(`^parley_` + rec.KeyID + `_[0-9A-Za-z]{32}$`)
		if r.status != http.StatusOK || !value.MatchString(key) || key == old || r.json["key_id"] != rec.KeyID ||
			r.json["expires_at"] != wantExpiry || len(r.json) != 3 {
			t.Fatalf("days %d: refresh = %d %s, want 200 with a new key, key_id %s and expires_at %v", days, r.status, r.body, rec.KeyID, wantExpiry)
		}
		if got := h.get("/api/v1/user/keys", "Bearer "+old).status; got != http.StatusUnauthorized {
			t.Errorf("days %d: GET /user/keys with the old key = %d, want 401", days, got)
		}
		list := h.get("/api/v1/user/keys", "Bearer "+key)
		if list.status != http.StatusOK {
			t.Fatalf("days %d: GET /user/keys with the new key = %d %s, want 200", days, list.status, list.body)
		}
		for _, item := range list.json["items"].([]any) {
			if obj := item.(map[string]any); obj["key_id"] == rec.KeyID && obj["expires_at"] != wantExpiry {
				t.Errorf("days %d: listed key %v, want expires_at %v", days, obj, wantExpiry)
			}
		}
	}
}

func TestRefreshKey_RefusesAPATAndTheAdminTokenBeforeTheLookup(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	pat, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"keys:manage"}})
	for _, c := range []struct{ name, caller, message string }{
		{"a PAT holding keys:manage", "Bearer " + pat, "this route needs an API key"},
		{"the admin token", "Bearer " + h.adminToken, "this route needs a user"},
	} {
		for _, id := range []string{h.keyRec.KeyID, "zzzzzzzz"} {
			before := h.snapshot()
			r := h.call(http.MethodPost, refreshPath(id), c.caller, nil)
			checkRefusal(t, c.name+" refreshing "+id, r, http.StatusForbidden, "forbidden", c.message)
			if h.snapshot() != before {
				t.Errorf("%s refreshing %s: the store changed", c.name, id)
			}
		}
	}
	if got := h.get("/api/v1/test/whoami", "Bearer "+h.key).status; got != http.StatusOK {
		t.Errorf("whoami with alice's key = %d, want 200", got)
	}
}

func TestRefreshKey_RefusesARevokedOrExpiredKey(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	_, revoked := h.seedKey(h.alice.ID, 30)
	h.revoke(revoked.KeyID)
	_, expired := h.seedKey(h.alice.ID, 30)
	h.expireAt(expired.KeyID, h.now())
	for state, id := range map[string]string{"revoked": revoked.KeyID, "expired": expired.KeyID} {
		before := h.snapshot()
		r := h.call(http.MethodPost, refreshPath(id), "Bearer "+h.key, nil)
		checkRefusal(t, "refresh of the "+state+" key", r, http.StatusConflict, "conflict", "API key is "+state)
		if h.snapshot() != before {
			t.Errorf("refresh of the %s key: the store changed", state)
		}
	}
}

func TestRevokeKey_SetsRevokedAtAndTheKeyStopsAuthenticating(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	_, expired := h.seedKey(h.alice.ID, 30)
	h.expireAt(expired.KeyID, h.now())
	k3, live := h.seedKey(h.alice.ID, 0)
	reader, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"keys:read"}})
	manager, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"keys:manage"}})
	h.advance(time.Hour)

	checkRefusal(t, "a PAT with keys:read", h.call(http.MethodDelete, keyPath(live.KeyID), "Bearer "+reader, nil),
		http.StatusForbidden, "insufficient_scope", "this route needs the scope keys:manage")
	if h.storedKey(live.KeyID).RevokedAt != nil {
		t.Errorf("a PAT with keys:read revoked the key")
	}
	before := h.count(`SELECT count(*) FROM api_keys`)
	for _, c := range []struct {
		name, caller string
		target       identity.APIKey
		value        string // "" when the key no longer authenticates anyway
	}{
		{"an API key, an expired key", h.key, expired, ""},
		{"a PAT holding keys:manage, a live key", manager, live, k3},
		{"an API key, itself", h.key, h.keyRec, h.key},
	} {
		r := h.call(http.MethodDelete, keyPath(c.target.KeyID), "Bearer "+c.caller, nil)
		want := asJSON(t, h.storedKey(c.target.KeyID)).(map[string]any)
		if r.status != http.StatusOK || !reflect.DeepEqual(r.json, want) || want["revoked_at"] != h.now().Format(time.RFC3339) {
			t.Errorf("%s: DELETE = %d %s, want 200 with the key revoked at %s", c.name, r.status, r.body, h.now().Format(time.RFC3339))
		}
		if c.value != "" {
			if got := h.get("/api/v1/user/keys", "Bearer "+c.value).status; got != http.StatusUnauthorized {
				t.Errorf("%s: GET /user/keys with the revoked key = %d, want 401", c.name, got)
			}
		}
	}
	if n := h.count(`SELECT count(*) FROM api_keys`); n != before {
		t.Errorf("%d API keys after the revocations, want %d kept", n, before)
	}
}

func TestRevokeKey_AnswersARevokedKeyUnchanged(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	_, k2 := h.seedKey(h.alice.ID, 0)
	revokedAt := h.now().Format(time.RFC3339)
	first := h.call(http.MethodDelete, keyPath(k2.KeyID), "Bearer "+h.key, nil)
	h.advance(time.Hour)
	second := h.call(http.MethodDelete, keyPath(k2.KeyID), "Bearer "+h.key, nil)
	if second.status != http.StatusOK || second.json["revoked_at"] != revokedAt || !reflect.DeepEqual(second.json, first.json) {
		t.Errorf("DELETE twice = %d %s, then %d %s, want 200 and the key revoked at %s", first.status, first.body, second.status, second.body, revokedAt)
	}
}

func TestOwnKeyRoutes_AnswerAMissingAndAnotherUsersKeyAlike(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	bobKey, bobs := h.seedKey(h.seedUser("bob").ID, 0)
	for _, c := range []struct {
		method string
		path   func(string) string
	}{
		{http.MethodPost, refreshPath},
		{http.MethodDelete, keyPath},
	} {
		before := h.snapshot()
		what := c.method + " " + c.path("<id>")
		missing := h.call(c.method, c.path("zzzzzzzz"), "Bearer "+h.key, nil)
		foreign := h.call(c.method, c.path(bobs.KeyID), "Bearer "+h.key, nil)
		checkRefusal(t, what+" of a missing key", missing, http.StatusNotFound, "not_found", "API key not found")
		if !bytes.Equal(foreign.body, missing.body) || foreign.status != missing.status {
			t.Errorf("%s of bob's key = %d %s, want the answer for a missing one %d %s", what, foreign.status, foreign.body, missing.status, missing.body)
		}
		if h.snapshot() != before {
			t.Errorf("%s: the store changed", what)
		}
	}
	if got := h.get("/api/v1/test/whoami", "Bearer "+bobKey).status; got != http.StatusOK {
		t.Errorf("whoami with bob's key = %d, want 200", got)
	}
}
