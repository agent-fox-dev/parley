package identity_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// patValue is the wire format of a PAT, its id and its secret.
var patValue = regexp.MustCompile(`^parley_pat_([a-z0-9]{8})_([a-z0-9]{32})$`)

// tokenMembers are the members of the Token object, sorted.
var tokenMembers = []string{"created_at", "expires_at", "name", "paths", "permissions", "revoked_at", "token_id", "user_id", "workspace"}

// asJSON is v as encoding/json decodes it after encoding it.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %v: %v", v, err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return out
}

func TestCreateToken_MintsAPATOwnedByTheCallersUser(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	manager, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"tokens:manage", "git:write", "changes:write"}})
	for name, caller := range map[string]string{"API key": h.key, "PAT holding tokens:manage": manager} {
		t.Run(name, func(t *testing.T) {
			r := h.createToken("Bearer "+caller, tokenBody(map[string]any{"permissions": []string{"git:write", "changes:write", "git:write"}}))
			if r.status != http.StatusCreated {
				t.Fatalf("POST /user/tokens = %d %s, want 201", r.status, r.body)
			}
			token, _ := r.json["token"].(string)
			m := patValue.FindStringSubmatch(token)
			if m == nil || m[1] != r.json["token_id"] {
				t.Fatalf("token = %q, want parley_pat_<token_id>_<secret> with token_id %v", token, r.json["token_id"])
			}
			if got := slices.Sorted(maps.Keys(withoutMembers(r.json, "token"))); !slices.Equal(got, tokenMembers) {
				t.Errorf("members besides token = %v, want %v", got, tokenMembers)
			}
			for member, want := range map[string]any{
				"user_id": h.alice.ID, "name": "ci", "permissions": []any{"git:write", "changes:write"}, "paths": nil,
				"workspace": nil, "revoked_at": nil, "created_at": h.now().Format(time.RFC3339),
			} {
				if !reflect.DeepEqual(r.json[member], want) {
					t.Errorf("%s = %v, want %v", member, r.json[member], want)
				}
			}
			if strings.Contains(h.snapshot(), m[2]) {
				t.Errorf("a stored column holds the secret")
			}
			if w := h.get("/api/v1/test/whoami", "Bearer "+token); w.status != http.StatusOK || w.json["credential_id"] != m[1] {
				t.Errorf("whoami with the new token = %d %s, want 200 as PAT %s", w.status, w.body, m[1])
			}
		})
	}
}

func TestCreateToken_KeepsTheNameAndThePathsAsGiven(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	for _, c := range []struct {
		name string
		set  map[string]any
	}{
		{"name of 255 characters", map[string]any{"name": strings.Repeat("n", 255)}},
		{"paths in order with duplicates, no write scope",
			map[string]any{"permissions": []string{"users:read"}, "paths": []string{"docs/**", "src/*.go", "docs/**"}}},
		{"50 globs", map[string]any{"paths": slices.Repeat([]string{"src/*.go"}, 50)}},
		{"a glob of 255 characters", map[string]any{"paths": []string{strings.Repeat("g", 255)}}},
		{"paths null", map[string]any{"paths": nil}},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := tokenBody(c.set)
			r := h.createToken("Bearer "+h.key, body)
			if r.status != http.StatusCreated {
				t.Fatalf("POST /user/tokens = %d %s, want 201", r.status, r.body)
			}
			if want := asJSON(t, body["paths"]); r.json["name"] != body["name"] || !reflect.DeepEqual(r.json["paths"], want) {
				t.Errorf("name, paths = %v, %v, want %v, %v", r.json["name"], r.json["paths"], body["name"], want)
			}
			id, _ := r.json["token_id"].(string)
			if got, want := h.get(tokenPath(id), "Bearer "+h.key).json, withoutMembers(r.json, "token"); !reflect.DeepEqual(got, want) {
				t.Errorf("GET /user/tokens/%s = %v, want the created token %v", id, got, want)
			}
		})
	}
}

func TestCreateToken_SetsExpiresAtFromTheLifetimeInDays(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	// The lifetime counts from the whole second of the creation.
	h.advance(500 * time.Millisecond)
	created, day := h.now().Truncate(time.Second), 24*time.Hour
	for _, c := range []struct {
		name    string
		expires any
		want    any
	}{
		{"absent", omitted{}, created.Add(90 * day).Format(time.RFC3339)},
		{"0", 0, nil},
		{"30", 30, created.Add(30 * day).Format(time.RFC3339)},
		{"60", 60, created.Add(60 * day).Format(time.RFC3339)},
		{"90", 90, created.Add(90 * day).Format(time.RFC3339)},
	} {
		r := h.createToken("Bearer "+h.key, tokenBody(map[string]any{"expires": c.expires}))
		if r.status != http.StatusCreated || r.json["expires_at"] != c.want {
			t.Errorf("expires %s: answer = %d, expires_at %v, want 201, %v", c.name, r.status, r.json["expires_at"], c.want)
		}
	}
	r := h.createToken("Bearer "+h.key, tokenBody(map[string]any{"expires": 30}))
	token, _ := r.json["token"].(string)
	at := created.Add(30 * day)
	for _, c := range []struct {
		offset time.Duration
		want   int
	}{{-time.Second, http.StatusOK}, {0, http.StatusUnauthorized}, {time.Second, http.StatusUnauthorized}} {
		h.advance(at.Add(c.offset).Sub(h.now()))
		if got := h.get("/api/v1/test/whoami", "Bearer "+token).status; got != c.want {
			t.Errorf("at expires_at %+v: whoami = %d, want %d", c.offset, got, c.want)
		}
	}
}

func TestCreateToken_RefusesAnInvalidBodyAndCreatesNothing(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	// The PAT holds neither git:admin nor git:write: a body that is invalid
	// is refused as invalid, before the rows that ask for more than it holds
	// meet the subset rule.
	pat, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"tokens:manage", "git:read"}})
	before := h.tokenCount(h.alice.ID)
	for _, c := range []struct {
		name string
		set  map[string]any
	}{
		{"no name", map[string]any{"name": omitted{}}},
		{"empty name", map[string]any{"name": ""}},
		{"name of 256 characters", map[string]any{"name": strings.Repeat("n", 256)}},
		{"name a number", map[string]any{"name": 5}},
		{"no permissions", map[string]any{"permissions": omitted{}}},
		{"permissions empty", map[string]any{"permissions": []string{}}},
		{"permissions a string", map[string]any{"permissions": "git:read"}},
		{"an unregistered scope", map[string]any{"permissions": []string{"git:read", "git:admin"}}},
		{"a scope in capitals", map[string]any{"permissions": []string{"GIT:READ"}}},
		{"expires 45", map[string]any{"permissions": []string{"git:write"}, "expires": 45}},
		{"expires -1", map[string]any{"expires": -1}},
		{"expires a string", map[string]any{"expires": "30"}},
		{"expires a fraction", map[string]any{"expires": 30.5}},
		{"paths empty", map[string]any{"paths": []string{}}},
		{"51 globs", map[string]any{"paths": slices.Repeat([]string{"docs/**"}, 51)}},
		{"an absolute glob", map[string]any{"paths": []string{"/src/**"}}},
	} {
		for caller, credential := range map[string]string{"API key": h.key, "PAT": pat} {
			r := h.createToken("Bearer "+credential, tokenBody(c.set))
			if r.status != http.StatusBadRequest || r.errorField("error_type") != "invalid_request" {
				t.Errorf("%s, %s: answer = %d %s, want 400 invalid_request", caller, c.name, r.status, r.body)
			}
		}
	}
	if n := h.tokenCount(h.alice.ID); n != before {
		t.Errorf("alice has %d PATs after the refusals, want %d", n, before)
	}
}

func TestCreateToken_RefusesAPATWithoutTokensManageBeforeReadingTheBody(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	for _, scope := range []string{"tokens:write", "tokens:read", "git:write"} {
		caller, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{scope}})
		before := h.tokenCount(h.alice.ID)
		for name, body := range map[string]any{"valid body": tokenBody(nil), "empty body": map[string]any{}} {
			r := h.createToken("Bearer "+caller, body)
			checkRefusal(t, scope+", "+name, r, http.StatusForbidden, "insufficient_scope", "this route needs the scope tokens:manage")
		}
		if n := h.tokenCount(h.alice.ID); n != before {
			t.Errorf("%s: alice has %d PATs, want %d", scope, n, before)
		}
	}
}

func TestCreateToken_ReplaysTheFirstAnswerWithoutTheTokenValue(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	before := h.tokenCount(h.alice.ID)
	header := http.Header{"Authorization": {"Bearer " + h.key}, "Idempotency-Key": {"k1"}}
	first := h.do(http.MethodPost, "/api/v1/user/tokens", tokenBody(nil), header)
	second := h.do(http.MethodPost, "/api/v1/user/tokens", tokenBody(nil), header)
	if first.status != http.StatusCreated || second.status != http.StatusOK {
		t.Fatalf("answers = %d, %d, want 201, 200", first.status, second.status)
	}
	if _, ok := first.json["token"]; !ok {
		t.Errorf("the first answer has no token")
	}
	if want := withoutMembers(first.json, "token"); !reflect.DeepEqual(second.json, want) {
		t.Errorf("replay = %v, want the first answer without token %v", second.json, want)
	}
	if n := h.tokenCount(h.alice.ID); n != before+1 {
		t.Errorf("alice has %d PATs, want %d", n, before+1)
	}
}

func TestCreateToken_CreatesWithinWhatTheCallingPATHoldsAndCovers(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	reader, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"tokens:manage", "git:read"}})
	writer, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"tokens:manage", "git:write"}, Paths: []string{"src/**"}})
	for _, c := range []struct {
		name, caller string
		set          map[string]any
	}{
		{"no paths held, none asked", reader, map[string]any{"permissions": []string{"git:read"}}},
		{"tokens:manage passed on", reader, map[string]any{"permissions": []string{"git:read", "tokens:manage"}}},
		{"no paths held, paths asked", reader, map[string]any{"paths": []string{"docs/**"}}},
		{"a narrower glob", writer, map[string]any{"permissions": []string{"git:write"}, "paths": []string{"src/api/*.go"}}},
		{"the same glob", writer, map[string]any{"permissions": []string{"git:write"}, "paths": []string{"src/**"}}},
		{"an API key, every scope and **", h.key, map[string]any{"permissions": identity.RegisteredScopes(), "paths": []string{"**"}}},
	} {
		body := tokenBody(c.set)
		r := h.createToken("Bearer "+c.caller, body)
		if r.status != http.StatusCreated {
			t.Errorf("%s: POST /user/tokens = %d %s, want 201", c.name, r.status, r.body)
			continue
		}
		if want := asJSON(t, []any{body["permissions"], body["paths"]}); !reflect.DeepEqual([]any{r.json["permissions"], r.json["paths"]}, want) {
			t.Errorf("%s: permissions, paths = %v, %v, want %v", c.name, r.json["permissions"], r.json["paths"], want)
		}
	}
}

func TestCreateToken_RefusesAPATBeyondItsOwnScopesAndPaths(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	limited := identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"tokens:manage", "git:write"}, Paths: []string{"src/**", "docs/*"}}
	for _, c := range []struct {
		name   string
		caller identity.PATSpec
		set    map[string]any
		named  string
	}{
		{"write does not give read", identity.PATSpec{Permissions: []string{"tokens:manage", "git:write"}},
			map[string]any{"permissions": []string{"git:read"}}, `"git:read"`},
		{"the first in request order", identity.PATSpec{Permissions: []string{"tokens:manage", "git:read"}},
			map[string]any{"permissions": []string{"git:read", "changes:write", "vars:read"}}, `"changes:write"`},
		{"manage does not give write", identity.PATSpec{Permissions: []string{"tokens:manage"}},
			map[string]any{"permissions": []string{"tokens:write"}}, `"tokens:write"`},
		{"write does not give approve", identity.PATSpec{Permissions: []string{"tokens:manage", "changes:write"}},
			map[string]any{"permissions": []string{"changes:approve"}}, `"changes:approve"`},
		{"paths absent", limited, map[string]any{"permissions": []string{"git:write"}}, "paths is required"},
		{"paths null", limited, map[string]any{"permissions": []string{"git:write"}, "paths": nil}, "paths is required"},
		{"the first uncovered glob", limited, map[string]any{"permissions": []string{"git:write"}, "paths": []string{"src/a.go", "lib/**"}}, `"lib/**"`},
		{"* does not cover **", limited, map[string]any{"permissions": []string{"git:write"}, "paths": []string{"docs/**"}}, `"docs/**"`},
		{"everything", limited, map[string]any{"permissions": []string{"git:write"}, "paths": []string{"**"}}, `"**"`},
		{"a permission before a glob", limited, map[string]any{"permissions": []string{"git:read"}, "paths": []string{"lib/**"}}, `"git:read"`},
	} {
		c.caller.UserID = h.alice.ID
		caller, _ := h.seedPAT(c.caller)
		before := h.tokenCount(h.alice.ID)
		r := h.createToken("Bearer "+caller, tokenBody(c.set))
		msg, _ := r.errorField("message").(string)
		if r.status != http.StatusForbidden || r.errorField("error_type") != "insufficient_scope" || !strings.Contains(msg, c.named) {
			t.Errorf("%s: answer = %d %s, want 403 insufficient_scope naming %s", c.name, r.status, r.body, c.named)
		}
		if n := h.tokenCount(h.alice.ID); n != before {
			t.Errorf("%s: alice has %d PATs, want %d", c.name, n, before)
		}
	}
}

// tokenPath is the path of the caller's token tokenID.
func tokenPath(tokenID string) string { return "/api/v1/user/tokens/" + tokenID }

// permissionsPath is the path of the permissions of the caller's token
// tokenID.
func permissionsPath(tokenID string) string { return tokenPath(tokenID) + "/permissions" }

func TestListTokens_AnswersEveryTokenOfTheCallerNewestFirst(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	now := h.now()
	_, revoked := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, CreatedAt: now.Add(-3 * time.Hour)})
	h.revoke(revoked.TokenID)
	// The bound and the expired token were created at the same instant, so
	// the id decides their order.
	past := now.Add(-time.Minute)
	_, expired := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, ExpiresAt: &past, CreatedAt: now.Add(-time.Hour)})
	_, bound := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, Workspace: "my-ws", CreatedAt: now.Add(-time.Hour)})
	h.seedPAT(identity.PATSpec{UserID: h.seedUser("bob").ID, Permissions: []string{"git:read"}})
	later, earlier := bound.TokenID, expired.TokenID
	if later < earlier {
		later, earlier = earlier, later
	}
	want := []string{h.patRec.TokenID, later, earlier, revoked.TokenID}

	r := h.get("/api/v1/user/tokens", "Bearer "+h.key)
	if got := itemValues(t, r, "token_id"); r.status != http.StatusOK || !slices.Equal(got, want) {
		t.Fatalf("GET /user/tokens = %d, token ids %v, want 200 with %v", r.status, got, want)
	}
	for _, item := range r.json["items"].([]any) {
		id, _ := item.(map[string]any)["token_id"].(string)
		if stored := asJSON(t, h.storedPAT(id)); !reflect.DeepEqual(item, stored) {
			t.Errorf("item %v, want the stored token %v", item, stored)
		}
		if ws := item.(map[string]any)["workspace"]; (id == bound.TokenID) != (ws == "my-ws") {
			t.Errorf("token %s has the workspace %v", id, ws)
		}
	}
	if got, requests := h.walkBy("/api/v1/user/tokens", "Bearer "+h.key, 2, "token_id"); !slices.Equal(got, want) || requests != 2 {
		t.Errorf("pages of 2 = %v in %d requests, want %v in 2", got, requests, want)
	}
	for _, scope := range []string{"tokens:read", "tokens:write", "tokens:manage"} {
		pat, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{scope}})
		if r := h.get("/api/v1/user/tokens", "Bearer "+pat); r.status != http.StatusOK {
			t.Errorf("a PAT with %s: GET /user/tokens = %d %s, want 200", scope, r.status, r.body)
		}
	}
}

func TestShowToken_AnswersTheCallersTokenInAnyState(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	_, revoked := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:write"}})
	h.revoke(revoked.TokenID)
	past := h.now().Add(-time.Minute)
	_, expired := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, ExpiresAt: &past})
	for state, id := range map[string]string{"live": h.patRec.TokenID, "revoked": revoked.TokenID, "expired": expired.TokenID} {
		r := h.get(tokenPath(id), "Bearer "+h.key)
		if want := asJSON(t, h.storedPAT(id)); r.status != http.StatusOK || !reflect.DeepEqual(any(r.json), want) {
			t.Errorf("%s: GET /user/tokens/%s = %d %s, want 200 with %v", state, id, r.status, r.body, want)
		}
	}
}

func TestRevokeToken_SetsRevokedAtAndTheTokenStopsAuthenticating(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	t1, live := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:write"}})
	past := h.now().Add(-time.Minute)
	_, expired := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, ExpiresAt: &past})
	t3, manager := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"tokens:manage"}})
	h.advance(time.Hour)
	before := h.tokenCount(h.alice.ID)
	for _, c := range []struct {
		name, caller string
		target       identity.PAT
		value        string // "" when the token no longer authenticates anyway
	}{
		{"an API key, a live token", h.key, live, t1},
		{"an API key, an expired token", h.key, expired, ""},
		{"a PAT holding tokens:manage, itself", t3, manager, t3},
	} {
		r := h.call(http.MethodDelete, tokenPath(c.target.TokenID), "Bearer "+c.caller, nil)
		want := asJSON(t, c.target).(map[string]any)
		want["revoked_at"] = h.now().Format(time.RFC3339)
		if r.status != http.StatusOK || !reflect.DeepEqual(r.json, want) {
			t.Errorf("%s: DELETE = %d %s, want 200 with %v", c.name, r.status, r.body, want)
		}
		if c.value != "" {
			if got := h.get("/api/v1/test/whoami", "Bearer "+c.value).status; got != http.StatusUnauthorized {
				t.Errorf("%s: whoami with the revoked token = %d, want 401", c.name, got)
			}
		}
	}
	if n := h.tokenCount(h.alice.ID); n != before {
		t.Errorf("alice has %d PATs after the revocations, want %d kept", n, before)
	}
}

func TestRevokeToken_AnswersARevokedTokenUnchanged(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	revokedAt := h.now().Format(time.RFC3339)
	first := h.call(http.MethodDelete, tokenPath(h.patRec.TokenID), "Bearer "+h.key, nil)
	h.advance(time.Hour)
	second := h.call(http.MethodDelete, tokenPath(h.patRec.TokenID), "Bearer "+h.key, nil)
	if second.status != http.StatusOK || second.json["revoked_at"] != revokedAt || !reflect.DeepEqual(second.json, first.json) {
		t.Errorf("DELETE twice = %d %s, then %d %s, want 200 and the token revoked at %s", first.status, first.body, second.status, second.body, revokedAt)
	}
}

func TestOwnTokenRoutes_AnswerAMissingAndAnotherUsersTokenAlike(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	_, bobs := h.seedPAT(identity.PATSpec{UserID: h.seedUser("bob").ID, Permissions: []string{"git:write"}})
	change := map[string]any{"permissions": []string{"git:read"}}
	for _, c := range []struct {
		method string
		path   func(string) string
		body   any
	}{
		{http.MethodGet, tokenPath, nil},
		{http.MethodDelete, tokenPath, nil},
		{http.MethodPut, permissionsPath, change},
		{http.MethodPatch, permissionsPath, change},
		{http.MethodDelete, permissionsPath, change},
	} {
		before := h.snapshot()
		what := c.method + " " + c.path("<id>")
		missing := h.call(c.method, c.path("zzzzzzzz"), "Bearer "+h.key, c.body)
		foreign := h.call(c.method, c.path(bobs.TokenID), "Bearer "+h.key, c.body)
		checkRefusal(t, what+" of a missing token", missing, http.StatusNotFound, "not_found", "token not found")
		if !bytes.Equal(foreign.body, missing.body) || foreign.status != missing.status {
			t.Errorf("%s of bob's token = %d %s, want the answer for a missing one %d %s", what, foreign.status, foreign.body, missing.status, missing.body)
		}
		if h.snapshot() != before {
			t.Errorf("%s: the store changed", what)
		}
	}
}

func TestOwnTokenRoutes_RefuseAPATWithoutTheScopeBeforeTheLookup(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	change := map[string]any{"permissions": []string{"git:read"}}
	for _, c := range []struct {
		method, path, held, needed string
		body                       any
	}{
		{http.MethodGet, "/api/v1/user/tokens", "git:read", "tokens:read", nil},
		{http.MethodGet, tokenPath("%s"), "keys:manage", "tokens:read", nil},
		{http.MethodDelete, tokenPath("%s"), "tokens:write", "tokens:manage", nil},
		{http.MethodDelete, tokenPath("%s"), "tokens:read", "tokens:manage", nil},
		{http.MethodPut, permissionsPath("%s"), "tokens:read", "tokens:write", change},
		{http.MethodPatch, permissionsPath("%s"), "tokens:read", "tokens:write", change},
		{http.MethodDelete, permissionsPath("%s"), "git:write", "tokens:write", change},
	} {
		caller, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{c.held}})
		for _, id := range []string{h.patRec.TokenID, "zzzzzzzz"} {
			path := strings.Replace(c.path, "%s", id, 1)
			before := h.snapshot()
			r := h.call(c.method, path, "Bearer "+caller, c.body)
			checkRefusal(t, c.held+" "+c.method+" "+path, r, http.StatusForbidden, "insufficient_scope", "this route needs the scope "+c.needed)
			if h.snapshot() != before {
				t.Errorf("%s %s with %s: the store changed", c.method, path, c.held)
			}
		}
	}
}

func TestTokenAnswers_CarryTheValueOnlyWhenTheTokenIsCreated(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	created := h.createToken("Bearer "+h.key, tokenBody(nil))
	token, _ := created.json["token"].(string)
	id, _ := created.json["token_id"].(string)
	secret := strings.TrimPrefix(token, "parley_pat_"+id+"_")
	alice, admin := "Bearer "+h.key, "Bearer "+h.adminToken
	for _, rq := range []struct {
		method, path, caller string
		body                 any
	}{
		{http.MethodGet, "/api/v1/user/tokens", alice, nil},
		{http.MethodGet, tokenPath(id), alice, nil},
		{http.MethodGet, "/api/v1/users/alice/tokens", admin, nil},
		{http.MethodPut, permissionsPath(id), alice, map[string]any{"permissions": []string{"git:read", "vars:read"}}},
		{http.MethodDelete, "/api/v1/users/alice/tokens/" + id, admin, nil},
		{http.MethodDelete, tokenPath(id), alice, nil},
	} {
		r := h.call(rq.method, rq.path, rq.caller, rq.body)
		objects := []any{any(r.json)}
		if items, ok := r.json["items"].([]any); ok {
			objects = items
		}
		for _, o := range objects {
			if _, has := o.(map[string]any)["token"]; has || r.status != http.StatusOK || strings.Contains(string(r.body), secret) {
				t.Errorf("%s %s = %d %s, want 200 without the token or its secret", rq.method, rq.path, r.status, r.body)
			}
		}
	}
}

func TestChangePermissions_ReplacesAddsAndRemovesScopes(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	remover, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"tokens:write"}})
	changesRead, varsRead := h.needsScope("changes:read"), h.needsScope("vars:read")
	for _, c := range []struct {
		name, method, caller string
		held, given, want    []string
		// what the token's value gets on the routes needing changes:read
		// and vars:read after the change
		changes, vars int
	}{
		{"PUT replaces, duplicates dropped", http.MethodPut, h.key, []string{"git:read", "changes:write"},
			[]string{"vars:read", "git:write", "vars:read"}, []string{"vars:read", "git:write"}, http.StatusForbidden, http.StatusOK},
		{"PATCH appends what is new, in order", http.MethodPatch, h.key, []string{"git:read", "changes:write"},
			[]string{"changes:write", "vars:read", "vars:read"}, []string{"git:read", "changes:write", "vars:read"}, http.StatusOK, http.StatusOK},
		{"DELETE removes, ignoring what the token lacks", http.MethodDelete, remover, []string{"git:read", "changes:write", "vars:read"},
			[]string{"changes:write", "git:write", "not-a-scope"}, []string{"git:read", "vars:read"}, http.StatusForbidden, http.StatusOK},
	} {
		value, target := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: c.held})
		r := h.call(c.method, permissionsPath(target.TokenID), "Bearer "+c.caller, map[string]any{"permissions": c.given})
		if want := asJSON(t, c.want); r.status != http.StatusOK || !reflect.DeepEqual(r.json["permissions"], want) || r.json["revoked_at"] != nil {
			t.Errorf("%s: answer = %d %s, want 200 with permissions %v and revoked_at null", c.name, r.status, r.body, c.want)
		}
		if got := h.get(tokenPath(target.TokenID), "Bearer "+h.key); !reflect.DeepEqual(got.json, r.json) {
			t.Errorf("%s: GET = %s, want the answer %s", c.name, got.body, r.body)
		}
		if got := h.get(changesRead, "Bearer "+value).status; got != c.changes {
			t.Errorf("%s: a route needing changes:read = %d, want %d", c.name, got, c.changes)
		}
		if got := h.get(varsRead, "Bearer "+value).status; got != c.vars {
			t.Errorf("%s: a route needing vars:read = %d, want %d", c.name, got, c.vars)
		}
	}
}

func TestChangePermissions_RevokesATokenLeftWithoutScopes(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	both := []string{"git:read", "vars:read"}
	type change struct {
		method string
		given  []string
	}
	for _, c := range []struct {
		name    string
		changes []change
	}{
		{"PUT []", []change{{http.MethodPut, []string{}}}},
		{"DELETE every scope", []change{{http.MethodDelete, both}}},
		{"DELETE one scope, then the other", []change{{http.MethodDelete, []string{"vars:read"}}, {http.MethodDelete, []string{"git:read"}}}},
	} {
		value, target := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: both})
		h.advance(time.Minute)
		var r response
		for _, ch := range c.changes {
			r = h.call(ch.method, permissionsPath(target.TokenID), "Bearer "+h.key, map[string]any{"permissions": ch.given})
		}
		if r.status != http.StatusOK || !reflect.DeepEqual(r.json["permissions"], []any{}) || r.json["revoked_at"] != h.now().Format(time.RFC3339) {
			t.Errorf("%s: answer = %d %s, want 200 with permissions [] and revoked_at %s", c.name, r.status, r.body, h.now().Format(time.RFC3339))
		}
		if got := h.get("/api/v1/test/whoami", "Bearer "+value).status; got != http.StatusUnauthorized {
			t.Errorf("%s: whoami with the token = %d, want 401", c.name, got)
		}
	}
}

func TestChangePermissions_RefusesAnInvalidBodyAndChangesNothing(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	_, target := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}})
	for _, c := range []struct {
		method string
		body   any
	}{
		{http.MethodPut, map[string]any{}},
		{http.MethodPut, map[string]any{"permissions": "git:read"}},
		{http.MethodPut, map[string]any{"permissions": nil}},
		{http.MethodPut, map[string]any{"permissions": []string{"git:admin"}}},
		{http.MethodPut, map[string]any{"permissions": []any{5}}},
		{http.MethodPatch, map[string]any{}},
		{http.MethodPatch, map[string]any{"permissions": []string{}}},
		{http.MethodPatch, map[string]any{"permissions": []string{"git:read", "nope"}}},
		{http.MethodDelete, map[string]any{}},
		{http.MethodDelete, map[string]any{"permissions": []string{}}},
		{http.MethodDelete, map[string]any{"permissions": "git:read"}},
	} {
		before := h.snapshot()
		r := h.call(c.method, permissionsPath(target.TokenID), "Bearer "+h.key, c.body)
		if r.status != http.StatusBadRequest || r.errorField("error_type") != "invalid_request" {
			t.Errorf("%s %v: answer = %d %s, want 400 invalid_request", c.method, c.body, r.status, r.body)
		}
		if h.snapshot() != before {
			t.Errorf("%s %v: the store changed", c.method, c.body)
		}
	}
}

func TestChangePermissions_HoldsACallingPATToTheScopesItAdds(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	for _, c := range []struct {
		name, method string
		itself       bool // the change is to the calling PAT itself
		given        []string
		named        string // the scope a refusal names, "" when the PAT may make the change
	}{
		{"PUT adding git:read", http.MethodPut, false, []string{"git:write", "git:read"}, `"git:read"`},
		{"PATCH adding vars:read", http.MethodPatch, false, []string{"vars:read"}, `"vars:read"`},
		{"PATCH widening itself", http.MethodPatch, true, []string{"tokens:manage"}, `"tokens:manage"`},
		{"PUT keeping git:write", http.MethodPut, false, []string{"git:write"}, ""},
		{"PATCH with one present and one held", http.MethodPatch, false, []string{"git:write", "tokens:write"}, ""},
	} {
		value, caller := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"tokens:write", "git:write"}})
		_, target := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:write"}})
		if c.itself {
			target = caller
		}
		body := map[string]any{"permissions": c.given}
		before := h.snapshot()
		r := h.call(c.method, permissionsPath(target.TokenID), "Bearer "+value, body)
		if c.named == "" {
			if r.status != http.StatusOK {
				t.Errorf("%s: answer = %d %s, want 200", c.name, r.status, r.body)
			}
			continue
		}
		msg, _ := r.errorField("message").(string)
		if r.status != http.StatusForbidden || r.errorField("error_type") != "insufficient_scope" || !strings.Contains(msg, c.named) {
			t.Errorf("%s: answer = %d %s, want 403 insufficient_scope naming %s", c.name, r.status, r.body, c.named)
		}
		if h.snapshot() != before {
			t.Errorf("%s: the store changed", c.name)
		}
		if r := h.call(c.method, permissionsPath(target.TokenID), "Bearer "+h.key, body); r.status != http.StatusOK {
			t.Errorf("%s with an API key: answer = %d %s, want 200", c.name, r.status, r.body)
		}
	}
}

func TestChangePermissions_RefusesARevokedOrExpiredToken(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	_, revoked := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}})
	h.revoke(revoked.TokenID)
	// A token expires at its expires_at, as authentication counts it.
	now := h.now()
	_, expired := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, ExpiresAt: &now})
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for state, target := range map[string]string{"revoked": revoked.TokenID, "expired": expired.TokenID} {
			before := h.snapshot()
			r := h.call(method, permissionsPath(target), "Bearer "+h.key, map[string]any{"permissions": []string{"git:read"}})
			checkRefusal(t, method+" a "+state+" token", r, http.StatusConflict, "conflict", "token is "+state)
			if h.snapshot() != before {
				t.Errorf("%s a %s token: the store changed", method, state)
			}
		}
	}
}

func TestChangePermissions_KeepsEverythingButThePermissions(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	expires := h.now().Add(30 * 24 * time.Hour)
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		_, target := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Name: "agent", Permissions: []string{"git:read", "vars:read"},
			Paths: []string{"docs/**"}, Workspace: "my-ws", ExpiresAt: &expires})
		want := withoutMembers(asJSON(t, target).(map[string]any), "permissions")
		r := h.call(method, permissionsPath(target.TokenID), "Bearer "+h.key,
			map[string]any{"permissions": []string{"git:read"}, "paths": []string{"**"}, "workspace": "other"})
		if got := withoutMembers(r.json, "permissions"); r.status != http.StatusOK || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: answer = %d %s, want 200 with %v", method, r.status, r.body, want)
		}
		if got := withoutMembers(asJSON(t, h.storedPAT(target.TokenID)).(map[string]any), "permissions"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: stored %v, want %v", method, got, want)
		}
	}
}
