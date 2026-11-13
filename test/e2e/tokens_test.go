package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// sendJSON sends method to url with authorization as the Authorization
// header and body as JSON, and returns the status and the answer as a JSON
// object.
func sendJSON(t *testing.T, method, url, authorization string, body any) (int, map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	var answer map[string]any
	if err := json.NewDecoder(res.Body).Decode(&answer); err != nil {
		t.Fatalf("%s %s: the body is not a JSON object: %v", method, url, err)
	}
	return res.StatusCode, answer
}

// itemByID is the item of the list on stdout whose member id is value.
func itemByID(t *testing.T, r ran, id string, value any) map[string]any {
	t.Helper()
	items, _ := r.json(t)["items"].([]any)
	for _, it := range items {
		if item, _ := it.(map[string]any); item[id] == value {
			return item
		}
	}
	t.Fatalf("no item with %s %v in %s", id, value, r.stdout)
	return nil
}

func TestTokens_ADeveloperMintsAPathScopedTokenThatAnAgentNarrowsAndTheDeveloperRevokes(t *testing.T) {
	h := startIdentityHub(t)
	h.createUser(t, "alice")
	key := h.issueKey(t, "alice")
	tokens := h.url + "/api/v1/user/tokens"

	created := h.must(t, key, "tokens", "create", "--name", "ci", "--permissions", "git:read,tokens:manage", "--paths", "docs/**")
	if !strings.Contains(created.stderr, "not be shown again") {
		t.Errorf("tokens create stderr = %q, want the show-once warning", created.stderr)
	}
	first := created.json(t)
	value, _ := first["token"].(string)
	agent := "Bearer " + value

	status, refused := sendJSON(t, http.MethodPost, tokens, agent, map[string]any{"name": "w", "permissions": []string{"git:write"}})
	e, _ := refused["error"].(map[string]any)
	if msg, _ := e["message"].(string); status != http.StatusForbidden || e["error_type"] != "insufficient_scope" || !strings.Contains(msg, "git:write") {
		t.Errorf("the agent's request for git:write = %d %v, want 403 insufficient_scope naming git:write", status, refused)
	}
	status, second := sendJSON(t, http.MethodPost, tokens, agent,
		map[string]any{"name": "n", "permissions": []string{"git:read"}, "paths": []string{"docs/api/*.md"}})
	if status != http.StatusCreated {
		t.Fatalf("the agent's request for git:read on docs/api/*.md = %d %v, want 201", status, second)
	}

	h.must(t, key, "tokens", "revoke", first["token_id"].(string))
	if status, _ := getAuthorized(t, tokens, agent); status != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/user/tokens with the revoked token = %d, want 401", status)
	}

	list := h.must(t, key, "tokens", "list")
	if ids := itemValues(t, list, "token_id"); len(ids) != 2 {
		t.Fatalf("alice's tokens = %v, want the two created (the refused request created none)", ids)
	}
	revoked := itemByID(t, list, "token_id", first["token_id"])
	if revoked["revoked_at"] == nil || !slices.Equal(asStrings(revoked["paths"]), []string{"docs/**"}) {
		t.Errorf("the first token = %v, want it revoked with paths [docs/**]", revoked)
	}
	narrowed := itemByID(t, list, "token_id", second["token_id"])
	if narrowed["revoked_at"] != nil || !slices.Equal(asStrings(narrowed["permissions"]), []string{"git:read"}) ||
		!slices.Equal(asStrings(narrowed["paths"]), []string{"docs/api/*.md"}) {
		t.Errorf("the second token = %v, want it live with permissions [git:read] and paths [docs/api/*.md]", narrowed)
	}
}

func TestTokens_AnOperatorRevokesAUsersToken(t *testing.T) {
	h := startIdentityHub(t)
	h.createUser(t, "alice")
	pat := h.must(t, h.issueKey(t, "alice"), "tokens", "create", "--name", "ops", "--permissions", "users:read").json(t)
	bearer := "Bearer " + pat["token"].(string)
	user := h.url + "/api/v1/user"

	items := itemValues(t, h.must(t, h.token, "admin", "tokens", "list", "alice"), "revoked_at")
	if len(items) != 1 || items[0] != nil {
		t.Fatalf("alice's tokens as the operator sees them: revoked_at = %v, want one live token", items)
	}
	if status, _ := getAuthorized(t, user, bearer); status != http.StatusOK {
		t.Fatalf("GET /api/v1/user with alice's token = %d, want 200", status)
	}

	if r := h.must(t, h.token, "admin", "tokens", "revoke", "alice", pat["token_id"].(string)).json(t); r["revoked_at"] == nil {
		t.Errorf("admin tokens revoke printed %v, want revoked_at set", r)
	}
	if got := itemValues(t, h.must(t, h.token, "admin", "tokens", "list", "alice"), "revoked_at"); len(got) != 1 || got[0] == nil {
		t.Errorf("alice's tokens after the revocation: revoked_at = %v, want it set", got)
	}
	if status, _ := getAuthorized(t, user, bearer); status != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/user with the revoked token = %d, want 401", status)
	}
}

// asStrings is a decoded JSON array of strings as a []string; anything else
// is nil.
func asStrings(v any) []string {
	items, _ := v.([]any)
	if items == nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := it.(string)
		out = append(out, s)
	}
	return out
}
