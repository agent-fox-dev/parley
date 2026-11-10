package identity

import (
	"strings"
	"testing"
)

func TestAuthenticate_LiveCredentialsPassWithTheirCaller(t *testing.T) {
	h := newTestHub(t)
	admin := h.bootstrapAdmin()
	alice := h.seedUser("alice")
	key, keyID := h.issueKey(alice.ID)
	pat, tokenID := h.storePAT(alice.ID, PATSpec{Name: "ci", Permissions: []string{"git:read"}, Paths: []string{"docs/**"}})

	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		for _, tt := range []struct {
			name, cred, typ, id string
		}{
			{"admin", admin, "admin", "admin"},
			{"api key", key, "api_key", keyID},
			{"pat", pat, "pat", tokenID},
		} {
			r := h.get("/api/v1/test/whoami", scheme+" "+tt.cred)
			if r.status != 200 {
				t.Fatalf("%s %s: %d %s", scheme, tt.name, r.status, r.body)
			}
			if r.doc["type"] != tt.typ || r.doc["credential_id"] != tt.id {
				t.Errorf("%s %s: caller %v", scheme, tt.name, r.doc)
			}
			user, _ := r.doc["user"].(map[string]any)
			switch tt.typ {
			case "admin":
				if r.doc["user"] != nil {
					t.Errorf("admin caller has a user: %v", r.doc["user"])
				}
			default:
				if user["id"] != alice.ID || user["role"] != "user" || user["status"] != "active" {
					t.Errorf("%s: user %v", tt.name, user)
				}
			}
			if tt.typ == "pat" {
				if fmtList(r.doc["permissions"]) != "git:read" || fmtList(r.doc["paths"]) != "docs/**" || r.doc["workspace"] != nil {
					t.Errorf("pat caller %v", r.doc)
				}
			} else if r.doc["permissions"] != nil || r.doc["paths"] != nil || r.doc["workspace"] != nil {
				t.Errorf("%s caller carries token fields: %v", tt.name, r.doc)
			}
		}
	}
	if r := h.get("/api/v1/test/whoami", "Bearer  "+key+" "); r.status != 200 {
		t.Errorf("extra spaces: %d", r.status)
	}
}

func fmtList(v any) string {
	items, _ := v.([]any)
	var out []string
	for _, i := range items {
		out = append(out, i.(string))
	}
	return strings.Join(out, ",")
}

func TestAuthenticate_AMissingOrMalformedCredentialNeedsAuthentication(t *testing.T) {
	h := newTestHub(t)
	alice := h.seedUser("alice")
	key, _ := h.issueKey(alice.ID)
	hex63 := strings.Repeat("a", 63)
	values := []string{
		"", "Basic dXNlcjpwYXNz", "Bearer", "Bearer    ", "Bearer hello", "Token " + key,
		"Bearer parley_admin_" + hex63,
		"Bearer parley_admin_" + strings.Repeat("A", 64),
		"Bearer parley_pat_abcd1234_" + strings.Repeat("a", 31),
		"Bearer parley_pat_ABCD1234_" + strings.Repeat("a", 32),
		"Bearer parley_abc1234_" + strings.Repeat("a", 32),
		"Bearer parley_abcd1234_" + strings.Repeat("a", 33),
		"Bearer parley_abc-1234_" + strings.Repeat("a", 32),
	}
	for _, v := range values {
		r := h.get("/api/v1/test/whoami", v)
		if r.status != 401 || r.errorMember("error_type") != "unauthenticated" || r.errorMember("code") != float64(401) ||
			r.errorMember("message") != "authentication required" {
			t.Errorf("%q: %d %s", v, r.status, r.body)
		}
	}
	if n := h.whoamiCalls.Load(); n != 0 {
		t.Errorf("handler ran %d times", n)
	}
}

func TestAuthenticate_AnUnknownIDOrWrongSecretIsInvalid(t *testing.T) {
	h := newTestHub(t)
	admin := h.bootstrapAdmin()
	alice := h.seedUser("alice")
	_, keyID := h.issueKey(alice.ID)
	_, tokenID := h.storePAT(alice.ID, PATSpec{Name: "ci", Permissions: []string{"git:read"}})
	other := strings.Repeat("Z", 32)
	otherLower := strings.Repeat("z", 32)
	unknownKey := "parley_UNKNOWN1_" + other
	cases := []string{
		unknownKey,
		"parley_" + keyID + "_" + other,
		"parley_pat_unknown1_" + otherLower,
		"parley_pat_" + tokenID + "_" + otherLower,
		"parley_admin_" + strings.Repeat("0", 64),
	}
	var unknownBody string
	for i, cred := range cases {
		r := h.get("/api/v1/test/whoami", bearer(cred))
		if r.status != 401 || r.errorMember("error_type") != "unauthenticated" || r.errorMember("message") != "invalid credentials" {
			t.Errorf("%q: %d %s", cred, r.status, r.body)
		}
		if i == 0 {
			unknownBody = r.body
		}
		if i == 1 && r.body != unknownBody {
			t.Errorf("a wrong secret is answered differently from an unknown id: %s vs %s", r.body, unknownBody)
		}
	}
	if admin == "" {
		t.Fatal("no admin token")
	}
	fresh := newTestHub(t)
	if r := fresh.get("/api/v1/test/whoami", bearer(admin)); r.status != 401 || r.errorMember("message") != "invalid credentials" {
		t.Errorf("admin token on a hub without an admin hash: %d %s", r.status, r.body)
	}
}

func TestAuthenticate_TheAdminTokenIsRefusedWhereAUserIsNeeded(t *testing.T) {
	h := newTestHub(t)
	admin := h.bootstrapAdmin()
	alice := h.seedUser("alice")
	key, _ := h.issueKey(alice.ID)
	pat, _ := h.storePAT(alice.ID, PATSpec{Name: "ci", Permissions: []string{"git:read"}})
	r := h.get("/api/v1/test/me", bearer(admin))
	if r.status != 403 || r.errorMember("error_type") != "forbidden" || r.errorMember("message") != "this route needs a user" {
		t.Errorf("admin on /test/me: %d %s", r.status, r.body)
	}
	if h.meCalls.Load() != 0 {
		t.Error("the handler ran for the admin token")
	}
	for _, cred := range []string{key, pat} {
		if r := h.get("/api/v1/test/me", bearer(cred)); r.status != 200 {
			t.Errorf("%s on /test/me: %d", cred[:12], r.status)
		}
	}
	if r := h.get("/api/v1/test/whoami", bearer(admin)); r.status != 200 {
		t.Errorf("admin on /test/whoami: %d", r.status)
	}
}
