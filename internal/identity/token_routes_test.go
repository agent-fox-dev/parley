package identity_test

import (
	"context"
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
			stored, err := identity.PATByID(context.Background(), h.st.Main(), id)
			if err != nil {
				t.Fatalf("read the new PAT: %v", err)
			}
			if got, want := asJSON(t, stored), any(withoutMembers(r.json, "token")); !reflect.DeepEqual(got, want) {
				t.Errorf("Token object of the stored PAT = %v, want the created one %v", got, want)
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
		{"expires 45", map[string]any{"expires": 45}},
		{"expires -1", map[string]any{"expires": -1}},
		{"expires a string", map[string]any{"expires": "30"}},
		{"expires a fraction", map[string]any{"expires": 30.5}},
		{"paths empty", map[string]any{"paths": []string{}}},
		{"51 globs", map[string]any{"paths": slices.Repeat([]string{"docs/**"}, 51)}},
		{"an absolute glob", map[string]any{"paths": []string{"/src/**"}}},
	} {
		r := h.createToken("Bearer "+h.key, tokenBody(c.set))
		if r.status != http.StatusBadRequest || r.errorField("error_type") != "invalid_request" {
			t.Errorf("%s: answer = %d %s, want 400 invalid_request", c.name, r.status, r.body)
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
