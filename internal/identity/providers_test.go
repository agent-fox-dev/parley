package identity

import (
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
)

// withRoutes mounts this package's routes over the configured providers,
// with the given identity reader.
func (h *testHub) withRoutes(reader IdentityReader, providers ...config.Provider) *Routes {
	h.t.Helper()
	rs := &Routes{Providers: ProvidersFrom(providers), Store: h.st, Reader: reader, Now: h.clock.Now}
	rs.Register(h.srv)
	return rs
}

func TestProviders_ListsTheConfiguredProvidersWithTheirAuthorizeURLs(t *testing.T) {
	h := newTestHub(t)
	h.withRoutes(nil,
		config.Provider{Name: "google", ClientID: "gg-id", ClientSecret: "gg-secret"},
		config.Provider{Name: "github", ClientID: "gh-id", ClientSecret: "gh-secret"},
	)
	r := h.get("/api/v1/auth/providers", "")
	if r.status != 200 || r.header.Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("%d %s, Cache-Control %q", r.status, r.body, r.header.Get("Cache-Control"))
	}
	if _, present := r.doc["next_cursor"]; !present || r.doc["next_cursor"] != nil {
		t.Errorf("next_cursor = %v", r.doc["next_cursor"])
	}
	items, _ := r.doc["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v", r.doc["items"])
	}
	gh, gg := items[0].(map[string]any), items[1].(map[string]any)
	if gh["name"] != "github" || gg["name"] != "google" {
		t.Errorf("order = %v, %v", gh["name"], gg["name"])
	}
	if gh["authorize_url"] != "https://github.com/login/oauth/authorize?client_id=gh-id&scope=user%3Aemail" {
		t.Errorf("github authorize_url = %v", gh["authorize_url"])
	}
	u, err := url.Parse(gg["authorize_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://accounts.google.com/o/oauth2/v2/auth" {
		t.Errorf("google endpoint = %s", u)
	}
	q := u.Query()
	if len(q) != 3 || q.Get("client_id") != "gg-id" || q.Get("scope") != "openid email profile" || q.Get("response_type") != "code" {
		t.Errorf("google query = %v", q)
	}
	for _, it := range []map[string]any{gh, gg} {
		var keys []string
		for k := range it {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "authorize_url,name" {
			t.Errorf("item members = %v", keys)
		}
	}
	for _, secret := range []string{"gh-secret", "gg-secret"} {
		if strings.Contains(r.body, secret) {
			t.Errorf("the body carries %s", secret)
		}
		for k, vs := range r.header {
			if strings.Contains(strings.Join(vs, " "), secret) {
				t.Errorf("header %s carries %s", k, secret)
			}
		}
	}
}

func TestProviders_AnEmptyConfigurationListsNothing(t *testing.T) {
	h := newTestHub(t)
	h.withRoutes(nil)
	r := h.get("/api/v1/auth/providers", "")
	if r.status != 200 || !strings.Contains(r.body, `"items":[]`) || r.doc["next_cursor"] != nil {
		t.Errorf("%d %s", r.status, r.body)
	}
}

func TestProviders_AConfiguredAuthorizeURLReplacesThePublicOne(t *testing.T) {
	h := newTestHub(t)
	h.withRoutes(nil, config.Provider{Name: "github", ClientID: "gh-id", ClientSecret: "s", AuthorizeURL: "https://ghe.example/login/oauth/authorize"})
	r := h.get("/api/v1/auth/providers", "")
	items, _ := r.doc["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["authorize_url"] != "https://ghe.example/login/oauth/authorize?client_id=gh-id&scope=user%3Aemail" {
		t.Errorf("items = %v", r.doc["items"])
	}
}

func TestPublicRoutes_AnswerWhateverCredentialIsSent(t *testing.T) {
	h := newTestHub(t)
	alice := h.seedUser("alice")
	revoked, revokedID := h.issueKey(alice.ID)
	h.exec(`UPDATE api_keys SET revoked_at = 1 WHERE key_id = ?`, revokedID)
	h.withRoutes(nil, config.Provider{Name: "github", ClientID: "gh-id", ClientSecret: "s"})
	plain := h.get("/api/v1/auth/providers", "")
	for _, auth := range []string{"", "Bearer hello", bearer(revoked), "Basic x"} {
		r := h.get("/api/v1/auth/providers", auth)
		if r.status != 200 || r.body != plain.body {
			t.Errorf("%q: %d %s", auth, r.status, r.body)
		}
	}
	if r := h.do("POST", "/api/v1/auth/callback", "Bearer hello", `{}`); r.status != 400 {
		t.Errorf("callback with a bad credential: %d %s", r.status, r.body)
	}
}
