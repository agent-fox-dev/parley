package identity_test

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// aliceHub is a bootstrapped test hub where the user alice (role user,
// status active) holds a live API key and a live PAT with permissions
// [git:read], paths [docs/**] and no workspace.
type aliceHub struct {
	*testHub
	alice    identity.User
	key, pat string
	keyRec   identity.APIKey
	patRec   identity.PAT
}

func newAliceHub(t *testing.T) aliceHub {
	t.Helper()
	h := aliceHub{testHub: newTestHub(t)}
	h.alice = h.seedUser("alice")
	h.key, h.keyRec = h.seedKey(h.alice.ID, 90)
	h.pat, h.patRec = h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, Paths: []string{"docs/**"}})
	return h
}

func TestAuthenticate_ReportsTheCallerOfEachCredentialType(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	checkAlice := func(t *testing.T, r response) {
		user, _ := r.json["user"].(map[string]any)
		if user["id"] != h.alice.ID || user["role"] != "user" || user["status"] != "active" {
			t.Errorf("user = %v, want alice with role user and status active", r.json["user"])
		}
	}
	type row struct {
		credential string
		check      func(t *testing.T, r response)
	}
	creds := map[string]row{
		"admin token": {h.adminToken, func(t *testing.T, r response) {
			if r.json["type"] != "admin" || r.json["credential_id"] != "admin" || r.json["user"] != nil {
				t.Errorf("caller = %s, want type admin, credential_id admin and no user", r.body)
			}
		}},
		"API key": {h.key, func(t *testing.T, r response) {
			if r.json["type"] != "api_key" || r.json["credential_id"] != h.keyRec.KeyID {
				t.Errorf("caller = %s, want type api_key and credential_id %s", r.body, h.keyRec.KeyID)
			}
			checkAlice(t, r)
			if r.json["permissions"] != nil || r.json["paths"] != nil || r.json["workspace"] != nil {
				t.Errorf("caller = %s, want no permissions, paths or workspace", r.body)
			}
		}},
		"PAT": {h.pat, func(t *testing.T, r response) {
			if r.json["type"] != "pat" || r.json["credential_id"] != h.patRec.TokenID {
				t.Errorf("caller = %s, want type pat and credential_id %s", r.body, h.patRec.TokenID)
			}
			checkAlice(t, r)
			if !reflect.DeepEqual(r.json["permissions"], []any{"git:read"}) ||
				!reflect.DeepEqual(r.json["paths"], []any{"docs/**"}) || r.json["workspace"] != nil {
				t.Errorf("caller = %s, want permissions [git:read], paths [docs/**] and workspace null", r.body)
			}
		}},
	}
	for name, c := range creds {
		for _, header := range []string{"Bearer " + c.credential, "bearer " + c.credential, "BEARER " + c.credential, "Bearer  " + c.credential + " "} {
			t.Run(name+"/"+strings.ReplaceAll(header, c.credential, "<credential>"), func(t *testing.T) {
				r := h.get("/api/v1/test/whoami", header)
				if r.status != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body %s)", r.status, r.body)
				}
				c.check(t, r)
			})
		}
	}
}

func TestAuthenticate_RequiresACredentialOfAKnownFormat(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	id, secret := strings.Repeat("a", 8), strings.Repeat("b", 32)
	for name, header := range map[string]string{
		"no header":                       "",
		"Basic scheme":                    "Basic dXNlcjpwYXNz",
		"scheme alone":                    "Bearer",
		"only spaces after the scheme":    "Bearer    ",
		"unknown format":                  "Bearer hello",
		"Token scheme":                    "Token " + h.key,
		"admin token of 63 hex":           "Bearer parley_admin_" + strings.Repeat("a", 63),
		"admin token of uppercase hex":    "Bearer parley_admin_" + strings.Repeat("A", 64),
		"PAT secret of 31 characters":     "Bearer parley_pat_" + id + "_" + secret[:31],
		"PAT id in uppercase":             "Bearer parley_pat_" + strings.ToUpper(id) + "_" + secret,
		"API key id of 7 characters":      "Bearer parley_" + id[:7] + "_" + secret,
		"API key secret of 33 characters": "Bearer parley_" + id + "_" + secret + "b",
		"API key id with a dash":          "Bearer parley_" + id[:7] + "-_" + secret,
	} {
		t.Run(name, func(t *testing.T) {
			r := h.get("/api/v1/test/whoami", header)
			if r.status != http.StatusUnauthorized || r.errorField("code") != float64(401) ||
				r.errorField("error_type") != "unauthenticated" || r.errorField("message") != "authentication required" {
				t.Errorf("answer = %d %s, want 401 unauthenticated %q", r.status, r.body, "authentication required")
			}
		})
	}
	if n := h.whoamiCalls.Load(); n != 0 {
		t.Errorf("the handler ran %d times, want 0", n)
	}
}

func TestAuthenticate_RefusesAnUnknownIDAndAWrongSecretAlike(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	empty := newTestHub(t, notBootstrapped())
	otherSecret := strings.Repeat("Z", 32)
	keyID, patID := h.keyRec.KeyID, h.patRec.TokenID
	cases := []struct {
		name       string
		hub        *testHub
		credential string
	}{
		{"unknown key_id", h.testHub, "parley_" + strings.Repeat("Q", 8) + h.key[len(h.key)-33:]},
		{"wrong key secret", h.testHub, "parley_" + keyID + "_" + otherSecret},
		{"unknown token_id", h.testHub, "parley_pat_" + strings.Repeat("q", 8) + h.pat[len(h.pat)-33:]},
		{"wrong PAT secret", h.testHub, "parley_pat_" + patID + "_" + strings.ToLower(otherSecret)},
		{"another admin token", h.testHub, "parley_admin_" + strings.Repeat("0", 64)},
		{"admin token of a hub not bootstrapped", empty, h.adminToken},
	}
	bodies := map[string][]byte{}
	for _, c := range cases {
		r := c.hub.get("/api/v1/test/whoami", "Bearer "+c.credential)
		if r.status != http.StatusUnauthorized || r.errorField("error_type") != "unauthenticated" ||
			r.errorField("message") != "invalid credentials" {
			t.Errorf("%s: answer = %d %s, want 401 unauthenticated %q", c.name, r.status, r.body, "invalid credentials")
		}
		bodies[c.name] = r.body
	}
	if !bytes.Equal(bodies["unknown key_id"], bodies["wrong key secret"]) {
		t.Errorf("unknown key_id answers %s, wrong secret %s; want the same body", bodies["unknown key_id"], bodies["wrong key secret"])
	}
}

func TestAuthenticate_RefusesTheAdminTokenOnARouteThatNeedsAUser(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	r := h.get("/api/v1/test/me", "Bearer "+h.adminToken)
	if r.status != http.StatusForbidden || r.errorField("error_type") != "forbidden" ||
		r.errorField("message") != "this route needs a user" {
		t.Errorf("admin token on a route that needs a user: %d %s, want 403 forbidden %q", r.status, r.body, "this route needs a user")
	}
	if n := h.meCalls.Load(); n != 0 {
		t.Errorf("the handler ran %d times for the admin token, want 0", n)
	}
	for name, cred := range map[string]string{"API key": h.key, "PAT": h.pat} {
		if r := h.get("/api/v1/test/me", "Bearer "+cred); r.status != http.StatusOK {
			t.Errorf("%s on a route that needs a user: %d %s, want 200", name, r.status, r.body)
		}
	}
	if r := h.get("/api/v1/test/whoami", "Bearer "+h.adminToken); r.status != http.StatusOK {
		t.Errorf("admin token on a route that needs no user: %d %s, want 200", r.status, r.body)
	}
}
