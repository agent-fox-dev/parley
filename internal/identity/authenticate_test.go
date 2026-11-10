package identity

import (
	"strings"
	"testing"
	"time"
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

// stateHub has alice with a live API key and a live PAT.
type stateHub struct {
	*testHub
	alice    User
	key      string
	keyID    string
	pat      string
	tokenID  string
	unknown  string
	tableFor map[string]string
	idFor    map[string]string
}

func newStateHub(t *testing.T) *stateHub {
	h := &stateHub{testHub: newTestHub(t)}
	h.alice = h.seedUser("alice")
	h.key, h.keyID = h.issueKey(h.alice.ID)
	h.pat, h.tokenID = h.storePAT(h.alice.ID, PATSpec{Name: "ci", Permissions: []string{"git:read"}})
	h.unknown = "parley_UNKNOWN1_" + strings.Repeat("Z", 32)
	h.tableFor = map[string]string{h.key: "api_keys", h.pat: "pats"}
	h.idFor = map[string]string{h.key: "key_id = '" + h.keyID + "'", h.pat: "token_id = '" + h.tokenID + "'"}
	return h
}

func (h *stateHub) revoke(cred string) {
	h.exec("UPDATE "+h.tableFor[cred]+" SET revoked_at = ? WHERE "+h.idFor[cred], nanos(h.clock.Now()))
}

func (h *stateHub) expireAt(cred string, at *time.Time) {
	h.exec("UPDATE "+h.tableFor[cred]+" SET expires_at = ? WHERE "+h.idFor[cred], nullNanos(at))
}

func (h *stateHub) setStatus(status string) {
	h.exec("UPDATE users SET status = ? WHERE id = ?", status, h.alice.ID)
}

// wrongSecret keeps a credential's id and replaces its secret.
func wrongSecret(cred string) string {
	i := strings.LastIndex(cred, "_")
	repl := strings.Repeat("Z", 32)
	if strings.HasPrefix(cred, "parley_pat_") {
		repl = strings.Repeat("z", 32)
	}
	return cred[:i+1] + repl
}

func TestAuthenticate_ARevokedCredentialIsRefused(t *testing.T) {
	h := newStateHub(t)
	for _, cred := range []string{h.key, h.pat} {
		if r := h.get("/api/v1/test/whoami", bearer(cred)); r.status != 200 {
			t.Fatalf("live: %d", r.status)
		}
		h.revoke(cred)
		r := h.get("/api/v1/test/whoami", bearer(cred))
		if r.status != 401 || r.errorMember("error_type") != "unauthenticated" || r.errorMember("message") != "credential revoked" {
			t.Errorf("revoked: %d %s", r.status, r.body)
		}
	}
}

func TestAuthenticate_AnExpiredCredentialIsRefused(t *testing.T) {
	now := t0
	before, after := now.Add(-time.Second), now.Add(time.Second)
	for _, at := range []*time.Time{&before, &now, &after, nil} {
		h := newStateHub(t)
		h.clock.set(now)
		for _, cred := range []string{h.key, h.pat} {
			h.expireAt(cred, at)
			r := h.get("/api/v1/test/whoami", bearer(cred))
			if at != nil && !at.After(now) {
				if r.status != 401 || r.errorMember("error_type") != "unauthenticated" || r.errorMember("message") != "credential expired" {
					t.Errorf("expires_at %v: %d %s", at, r.status, r.body)
				}
			} else if r.status != 200 {
				t.Errorf("expires_at %v: %d %s", at, r.status, r.body)
			}
		}
	}
}

func TestAuthenticate_ABlockedUsersCredentialIsForbidden(t *testing.T) {
	h := newStateHub(t)
	h.setStatus(StatusBlocked)
	for _, cred := range []string{h.key, h.pat} {
		r := h.get("/api/v1/test/whoami", bearer(cred))
		if r.status != 403 || r.errorMember("error_type") != "forbidden" || r.errorMember("message") != "user is blocked" {
			t.Errorf("blocked: %d %s", r.status, r.body)
		}
	}
	var revoked int
	if err := h.st.DB().QueryRow(`SELECT (SELECT count(*) FROM api_keys WHERE revoked_at IS NOT NULL) + (SELECT count(*) FROM pats WHERE revoked_at IS NOT NULL)`).Scan(&revoked); err != nil || revoked != 0 {
		t.Errorf("blocking revoked %d credentials (%v)", revoked, err)
	}
}

func TestAuthenticate_TheFirstFailingCheckDecidesAndAWrongSecretRevealsNothing(t *testing.T) {
	past := t0.Add(-time.Hour)
	states := []struct {
		name    string
		arrange func(h *stateHub, cred string)
		message string
	}{
		{"revoked and expired", func(h *stateHub, c string) { h.revoke(c); h.expireAt(c, &past) }, "credential revoked"},
		{"revoked, user blocked", func(h *stateHub, c string) { h.revoke(c); h.setStatus(StatusBlocked) }, "credential revoked"},
		{"expired, user blocked", func(h *stateHub, c string) { h.expireAt(c, &past); h.setStatus(StatusBlocked) }, "credential expired"},
	}
	for _, s := range states {
		for _, pick := range []string{"key", "pat"} {
			h := newStateHub(t)
			cred := map[string]string{"key": h.key, "pat": h.pat}[pick]
			s.arrange(h, cred)
			if r := h.get("/api/v1/test/whoami", bearer(cred)); r.status != 401 || r.errorMember("message") != s.message {
				t.Errorf("%s %s: %d %s", s.name, pick, r.status, r.body)
			}
		}
	}
	for _, s := range []struct {
		name    string
		arrange func(h *stateHub, cred string)
	}{
		{"revoked", func(h *stateHub, c string) { h.revoke(c) }},
		{"expired", func(h *stateHub, c string) { h.expireAt(c, &past) }},
		{"user blocked", func(h *stateHub, _ string) { h.setStatus(StatusBlocked) }},
	} {
		for _, pick := range []string{"key", "pat"} {
			h := newStateHub(t)
			cred := map[string]string{"key": h.key, "pat": h.pat}[pick]
			s.arrange(h, cred)
			r := h.get("/api/v1/test/whoami", bearer(wrongSecret(cred)))
			unknown := h.get("/api/v1/test/whoami", bearer(h.unknown))
			if r.status != 401 || r.errorMember("message") != "invalid credentials" || r.body != unknown.body {
				t.Errorf("%s %s with a wrong secret: %d %s", s.name, pick, r.status, r.body)
			}
		}
	}
}

func TestAuthenticate_UnblockingRestoresTheLiveCredentials(t *testing.T) {
	h := newStateHub(t)
	revokedKey, revokedID := h.issueKey(h.alice.ID)
	h.exec("UPDATE api_keys SET revoked_at = ? WHERE key_id = ?", nanos(t0), revokedID)
	expiredPAT, expiredID := h.storePAT(h.alice.ID, PATSpec{Name: "old", Permissions: []string{"git:read"}})
	h.exec("UPDATE pats SET expires_at = ? WHERE token_id = ?", nanos(t0.Add(-time.Hour)), expiredID)
	h.setStatus(StatusBlocked)
	want := map[string]string{h.key: "user is blocked", h.pat: "user is blocked", revokedKey: "credential revoked", expiredPAT: "credential expired"}
	for cred, msg := range want {
		if r := h.get("/api/v1/test/whoami", bearer(cred)); r.errorMember("message") != msg {
			t.Errorf("blocked: %s %s, want %s", cred[:14], r.body, msg)
		}
	}
	h.setStatus(StatusActive)
	for _, cred := range []string{h.key, h.pat} {
		if r := h.get("/api/v1/test/whoami", bearer(cred)); r.status != 200 {
			t.Errorf("after unblocking: %d %s", r.status, r.body)
		}
	}
	if r := h.get("/api/v1/test/whoami", bearer(revokedKey)); r.errorMember("message") != "credential revoked" {
		t.Errorf("revoked key after unblocking: %s", r.body)
	}
	if r := h.get("/api/v1/test/whoami", bearer(expiredPAT)); r.errorMember("message") != "credential expired" {
		t.Errorf("expired PAT after unblocking: %s", r.body)
	}
}
