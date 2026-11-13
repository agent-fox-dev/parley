package identity_test

import (
	"encoding/json"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
)

func TestProviders_ListsTheConfiguredProvidersWithTheirAuthorizeURL(t *testing.T) {
	t.Parallel()
	github := config.Provider{Name: "github", ClientID: "gh-id", ClientSecret: "gh-secret"}
	google := config.Provider{Name: "google", ClientID: "gg-id", ClientSecret: "gg-secret"}
	enterprise := github
	enterprise.AuthorizeURL = "https://ghe.example/login/oauth/authorize"
	const githubURL = "https://github.com/login/oauth/authorize?client_id=gh-id&scope=user%3Aemail"
	googleURL := func(t *testing.T, got string) {
		t.Helper()
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("google authorize_url %q: %v", got, err)
		}
		if base := u.Scheme + "://" + u.Host + u.Path; base != "https://accounts.google.com/o/oauth2/v2/auth" {
			t.Errorf("google authorize_url endpoint = %q", base)
		}
		want := url.Values{"client_id": {"gg-id"}, "scope": {"openid email profile"}, "response_type": {"code"}}
		if q := u.Query(); !maps.EqualFunc(q, want, slices.Equal) {
			t.Errorf("google authorize_url query = %v, want %v", q, want)
		}
	}
	exactly := func(want string) func(*testing.T, string) {
		return func(t *testing.T, got string) {
			if got != want {
				t.Errorf("authorize_url = %q, want %q", got, want)
			}
		}
	}
	type item struct {
		name string
		url  func(*testing.T, string)
	}
	cases := []struct {
		name      string
		providers []config.Provider
		want      []item
	}{
		{"none configured", nil, nil},
		{"ordered by name, on the public endpoints", []config.Provider{google, github},
			[]item{{"github", exactly(githubURL)}, {"google", googleURL}}},
		{"on a configured authorize_url", []config.Provider{enterprise},
			[]item{{"github", exactly("https://ghe.example/login/oauth/authorize?client_id=gh-id&scope=user%3Aemail")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHub(t, withProviders(tc.providers...))

			r := h.get("/api/v1/auth/providers", "")

			if r.status != 200 {
				t.Fatalf("status = %d, want 200; body %s", r.status, r.body)
			}
			if cc := r.header.Get("Cache-Control"); cc != "public, max-age=300" {
				t.Errorf("Cache-Control = %q, want public, max-age=300", cc)
			}
			var list struct {
				Items      []map[string]json.RawMessage `json:"items"`
				NextCursor *string                      `json:"next_cursor"`
			}
			if err := json.Unmarshal(r.body, &list); err != nil || list.Items == nil {
				t.Fatalf("body %s is not a list with an items array (%v)", r.body, err)
			}
			if list.NextCursor != nil {
				t.Errorf("next_cursor = %q, want null", *list.NextCursor)
			}
			if len(list.Items) != len(tc.want) {
				t.Fatalf("items = %s, want %d", r.body, len(tc.want))
			}
			for i, it := range list.Items {
				if keys := slices.Sorted(maps.Keys(it)); !slices.Equal(keys, []string{"authorize_url", "name"}) {
					t.Errorf("item %d has the members %v, want authorize_url and name", i, keys)
				}
				var name, authorizeURL string
				_ = json.Unmarshal(it["name"], &name)
				_ = json.Unmarshal(it["authorize_url"], &authorizeURL)
				if name != tc.want[i].name {
					t.Errorf("item %d name = %q, want %q", i, name, tc.want[i].name)
				}
				tc.want[i].url(t, authorizeURL)
			}
			for _, secret := range []string{"gh-secret", "gg-secret"} {
				if strings.Contains(string(r.body), secret) {
					t.Errorf("body holds the client secret %s", secret)
				}
				for k, vs := range r.header {
					if strings.Contains(strings.Join(vs, " "), secret) {
						t.Errorf("header %s holds the client secret %s", k, secret)
					}
				}
			}
		})
	}
}
