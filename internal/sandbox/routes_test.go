package sandbox_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// profileList is the answer of GET /sandbox-profiles, decoded.
type profileList struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// listProfiles sends GET path with cred and decodes a 200 answer.
func listProfiles(t *testing.T, h *probeHub, path, cred string) (answer, profileList) {
	t.Helper()
	a := h.call(t, http.MethodGet, path, cred, nil)
	var list profileList
	if a.status == http.StatusOK {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(a.body, &raw); err != nil {
			t.Fatalf("GET %s: the body is not a JSON object (%v): %s", path, err, a.body)
		}
		if _, ok := raw["next_cursor"]; !ok {
			t.Errorf("GET %s = %s, want a next_cursor member", path, a.body)
		}
		if err := json.Unmarshal(a.body, &list); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}
	return a, list
}

// names gives the name member of each item.
func names(items []map[string]any) []any {
	out := []any{}
	for _, it := range items {
		out = append(out, it["name"])
	}
	return out
}

// item finds the item called name.
func item(t *testing.T, items []map[string]any, name string) map[string]any {
	t.Helper()
	for _, it := range items {
		if it["name"] == name {
			return it
		}
	}
	t.Fatalf("no item %q in %v", name, items)
	return nil
}

func TestProfileList_GivesEveryCallerWithTheReadScopeEveryProfileInNameOrderOnOnePage(t *testing.T) {
	t.Parallel()
	h, _ := startProfilesHub(t)
	// A user who owns no workspace and belongs to no organization of anyone
	// else.
	loner := h.CreateUser(t, "loner")
	creds := []struct{ name, cred string }{
		{"the API key of a user with no workspace", h.IssueKey(t, loner)},
		{"the admin token", h.Token},
		{"a PAT with sandboxes:read", h.pat(t, loner, "sandboxes:read")},
	}
	for _, c := range creds {
		t.Run(c.name, func(t *testing.T) {
			before := len(h.rt.Calls())
			for _, path := range []string{"/sandbox-profiles", "/sandbox-profiles?limit=1&cursor=x"} {
				a, list := listProfiles(t, h, path, c.cred)
				if a.status != http.StatusOK {
					t.Fatalf("GET %s = %d %s, want 200", path, a.status, a.body)
				}
				if got, want := names(list.Items), []any{"agent", "plain"}; !reflect.DeepEqual(got, want) {
					t.Errorf("GET %s: names = %v, want %v", path, got, want)
				}
				if list.NextCursor != nil {
					t.Errorf("GET %s: next_cursor = %q, want null", path, *list.NextCursor)
				}
			}
			if calls := h.rt.Calls()[before:]; len(calls) != 0 {
				t.Errorf("the runtime received %v, want no call", ops(calls))
			}
		})
	}

	h.rt.SetUnreachable(true)
	t.Cleanup(func() { h.rt.SetUnreachable(false) })
	for _, c := range creds {
		if a, list := listProfiles(t, h, "/sandbox-profiles", c.cred); a.status != http.StatusOK || len(list.Items) != 2 {
			t.Errorf("GET /sandbox-profiles with %s while the runtime is unreachable = %d %s, want 200 with both profiles", c.name, a.status, a.body)
		}
	}
}

func TestProfileList_ShowsWhatAProfileOffersWithoutItsPolicyPathEnvValuesOrSecrets(t *testing.T) {
	t.Parallel()
	h, policyPath := startProfilesHub(t)
	a, list := listProfiles(t, h, "/sandbox-profiles", h.IssueKey(t, h.CreateUser(t, "dev")))
	if a.status != http.StatusOK {
		t.Fatalf("GET /sandbox-profiles = %d %s, want 200", a.status, a.body)
	}

	want := map[string]map[string]any{
		"agent": {
			"name":                 "agent",
			"default":              true,
			"description":          "an agent",
			"image":                "agent:1",
			"cpu":                  "2",
			"memory":               "4Gi",
			"gpu":                  float64(1),
			"ttl":                  "4h",
			"ttl_seconds":          float64(14400),
			"idle_timeout":         "30m",
			"policy_summary":       []any{"a.test", "b.test"},
			"credentials":          []any{"model"},
			"optional_credentials": []any{"search"},
			"exposed_secrets": []any{
				map[string]any{"name": "A_TOKEN", "optional": true},
				map[string]any{"name": "Z_TOKEN", "optional": false},
			},
			"hub_access":      "none",
			"env_keys":        []any{"A", "B"},
			"checkpoint":      false,
			"allow_proposals": false,
		},
		"plain": {
			"name":                 "plain",
			"default":              false,
			"description":          "",
			"image":                "",
			"cpu":                  "",
			"memory":               "",
			"gpu":                  float64(0),
			"ttl":                  "1d",
			"ttl_seconds":          float64(86400),
			"idle_timeout":         "0s",
			"policy_summary":       []any{},
			"credentials":          []any{},
			"optional_credentials": []any{},
			"exposed_secrets":      []any{},
			"hub_access":           "fork",
			"env_keys":             []any{},
			"checkpoint":           true,
			"allow_proposals":      true,
		},
	}
	for name, w := range want {
		t.Run(name, func(t *testing.T) {
			got := item(t, list.Items, name)
			// Comparing the whole object also proves that it has exactly the
			// eighteen members and none of policy, env, expose_secrets and
			// optional_expose_secrets.
			if !reflect.DeepEqual(got, w) {
				t.Errorf("item %s =\n%v\nwant\n%v", name, got, w)
			}
		})
	}
	for _, hidden := range []string{`"va"`, `"vb"`, policyPath, "policy.yaml"} {
		if bytes.Contains(a.body, []byte(hidden)) {
			t.Errorf("the answer %s holds %s", a.body, hidden)
		}
	}
}

func TestProfileList_RefusesAPATWithoutTheReadScopeAndARequestWithoutACredential(t *testing.T) {
	t.Parallel()
	h, _ := startProfilesHub(t)
	for _, c := range []struct {
		name, cred string
		status     int
		errorType  string
	}{
		{"a PAT without sandboxes:read", h.pat(t, h.CreateUser(t, "dev"), "workspaces:read"), http.StatusForbidden, "insufficient_scope"},
		{"no credential", "", http.StatusUnauthorized, "unauthenticated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := h.call(t, http.MethodGet, "/sandbox-profiles", c.cred, nil)
			if a.status != c.status || a.err.ErrorType != c.errorType {
				t.Errorf("GET /sandbox-profiles = %d %s, want %d %s", a.status, a.body, c.status, c.errorType)
			}
			var body map[string]any
			_ = json.Unmarshal(a.body, &body)
			if _, ok := body["items"]; ok || bytes.Contains(a.body, []byte("agent")) {
				t.Errorf("the refusal %s holds profile data", a.body)
			}
		})
	}
}
