package sandbox_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
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
	// Serial subtests: each counts the runtime's calls around its requests.
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
			t.Parallel()
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
			t.Parallel()
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

// fiveSandboxes are five sandboxes in three workspaces, created by u1 and u2,
// two of them at the same instant. Newest first, ties by descending id,
// they are sb-5, sb-3, sb-2, sb-4, sb-1.
func fiveSandboxes() []storedSandbox {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	return []storedSandbox{
		{"sb-1", "ws-a", "ready", "u1", t0.Add(time.Hour)},
		{"sb-2", "ws-b", "ready", "u2", t0.Add(3 * time.Hour)},
		{"sb-3", "ws-a", "stopped", "u1", t0.Add(3 * time.Hour)},
		{"sb-4", "ws-c", "deleted", "u2", t0.Add(2 * time.Hour)},
		{"sb-5", "ws-b", "failed", "u1", t0.Add(4 * time.Hour)},
	}
}

// sandboxList is the answer of GET /sandboxes, decoded.
type sandboxList struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// listSandboxes sends GET path with cred, wants 200 and decodes the list.
func listSandboxes(t *testing.T, h *probeHub, path, cred string) sandboxList {
	t.Helper()
	a := h.call(t, http.MethodGet, path, cred, nil)
	if a.status != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", path, a.status, a.body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(a.body, &raw); err != nil {
		t.Fatalf("GET %s: the body is not a JSON object (%v): %s", path, err, a.body)
	}
	if _, ok := raw["next_cursor"]; !ok {
		t.Errorf("GET %s = %s, want a next_cursor member", path, a.body)
	}
	var list sandboxList
	if err := json.Unmarshal(a.body, &list); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return list
}

// ids gives the id member of each item.
func ids(items []map[string]any) []any {
	out := []any{}
	for _, it := range items {
		out = append(out, it["id"])
	}
	return out
}

func TestSandboxList_GivesTheAdminTokenEverySandboxNewestFirstPageByPageWithoutTheRuntime(t *testing.T) {
	t.Parallel()
	src := &memorySource{sandboxes: fiveSandboxes()}
	h := startListHub(t, src)
	before := len(h.rt.Calls())

	var got []map[string]any
	path := "/sandboxes?limit=2"
	var sizes []int
	for range 3 {
		list := listSandboxes(t, h, path, h.Token)
		got = append(got, list.Items...)
		sizes = append(sizes, len(list.Items))
		if list.NextCursor == nil {
			break
		}
		path = "/sandboxes?limit=2&cursor=" + url.QueryEscape(*list.NextCursor)
	}
	if want := []int{2, 2, 1}; !reflect.DeepEqual(sizes, want) {
		t.Errorf("page sizes = %v, want %v", sizes, want)
	}
	if want := []any{"sb-5", "sb-3", "sb-2", "sb-4", "sb-1"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("ids = %v, want %v", ids(got), want)
	}
	if last := listSandboxes(t, h, path, h.Token); last.NextCursor != nil {
		t.Errorf("the last page's next_cursor = %q, want null", *last.NextCursor)
	}

	// Each item is the object the source gave, member for member.
	byID := map[string]storedSandbox{}
	for _, s := range fiveSandboxes() {
		byID[s.id] = s
	}
	for _, it := range got {
		raw, err := json.Marshal(byID[it["id"].(string)].object())
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(it, want) {
			t.Errorf("item = %v, want the source's object %v", it, want)
		}
	}

	all := listSandboxes(t, h, "/sandboxes", h.Token)
	if len(all.Items) != 5 || all.NextCursor != nil {
		t.Errorf("GET /sandboxes without a limit = %d items, next_cursor %v; want 5 and null", len(all.Items), all.NextCursor)
	}

	// The list reads stored state only, so it answers the same while the
	// runtime is unreachable.
	h.rt.SetUnreachable(true)
	t.Cleanup(func() { h.rt.SetUnreachable(false) })
	if list := listSandboxes(t, h, "/sandboxes", h.Token); len(list.Items) != 5 {
		t.Errorf("GET /sandboxes while the runtime is unreachable = %d items, want 5", len(list.Items))
	}
	if calls := h.rt.Calls()[before:]; len(calls) != 0 {
		t.Errorf("the runtime received %v, want no call", ops(calls))
	}
}

func TestSandboxList_GivesOnlyTheSandboxesInTheStateAndOfTheUserAsked(t *testing.T) {
	t.Parallel()
	h := startListHub(t, &memorySource{sandboxes: fiveSandboxes()})
	for _, c := range []struct {
		query string
		want  []any
	}{
		{"state=ready", []any{"sb-2", "sb-1"}},
		{"state=deleted", []any{"sb-4"}},
		{"user=u1", []any{"sb-5", "sb-3", "sb-1"}},
		{"state=ready&user=u1", []any{"sb-1"}},
		{"user=nobody", []any{}},
	} {
		t.Run(c.query, func(t *testing.T) {
			t.Parallel()
			list := listSandboxes(t, h, "/sandboxes?"+c.query, h.Token)
			if !reflect.DeepEqual(ids(list.Items), c.want) {
				t.Errorf("ids = %v, want %v", ids(list.Items), c.want)
			}
			if list.NextCursor != nil {
				t.Errorf("next_cursor = %q, want null", *list.NextCursor)
			}
		})
	}

	t.Run("a cursor of another filter", func(t *testing.T) {
		t.Parallel()
		first := listSandboxes(t, h, "/sandboxes?state=ready&limit=1", h.Token)
		if first.NextCursor == nil {
			t.Fatal("the first page of state=ready&limit=1 has no next_cursor")
		}
		a := h.call(t, http.MethodGet, "/sandboxes?state=stopped&cursor="+url.QueryEscape(*first.NextCursor), h.Token, nil)
		if a.status != http.StatusBadRequest || a.err.ErrorType != "invalid_request" {
			t.Errorf("GET with the cursor of state=ready and state=stopped = %d %s, want 400 invalid_request", a.status, a.body)
		}
	})
}

func TestSandboxList_RefusesEveryCredentialButTheAdminTokenWithoutReadingAnySandbox(t *testing.T) {
	t.Parallel()
	src := &memorySource{sandboxes: fiveSandboxes()}
	h := startListHub(t, src)
	root := h.CreateUser(t, "root")
	if a := h.call(t, http.MethodPost, "/users/"+root+"/promote", h.Token, nil); a.status != http.StatusOK {
		t.Fatalf("promote root = %d %s, want 200", a.status, a.body)
	}
	dev := h.CreateUser(t, "dev")
	// Serial subtests: the source's calls are counted once they have all run.
	for _, c := range []struct {
		name, cred string
		status     int
		errorType  string
	}{
		{"the API key of a user with role admin", h.IssueKey(t, root), http.StatusForbidden, "forbidden"},
		{"the API key of a plain user", h.IssueKey(t, dev), http.StatusForbidden, "forbidden"},
		{"a PAT with sandboxes:read and sandboxes:write", h.pat(t, dev, "sandboxes:read", "sandboxes:write"), http.StatusForbidden, "forbidden"},
		{"no credential", "", http.StatusUnauthorized, "unauthenticated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := h.call(t, http.MethodGet, "/sandboxes", c.cred, nil)
			if a.status != c.status || a.err.ErrorType != c.errorType {
				t.Errorf("GET /sandboxes = %d %s, want %d %s", a.status, a.body, c.status, c.errorType)
			}
		})
	}
	if n := src.calls.Load(); n != 0 {
		t.Errorf("the source was called %d times, want 0", n)
	}
}

func TestSandboxList_RefusesAnUnknownStateABadLimitOrAMalformedCursorWithoutReadingAnySandbox(t *testing.T) {
	t.Parallel()
	src := &memorySource{sandboxes: fiveSandboxes()}
	h := startListHub(t, src)
	// Serial subtests: each counts the source's calls around its request.
	for _, c := range []struct {
		query  string
		status int
	}{
		{"state=running", http.StatusBadRequest},
		{"state=READY", http.StatusBadRequest},
		{"state=", http.StatusBadRequest},
		{"limit=0", http.StatusBadRequest},
		{"limit=201", http.StatusBadRequest},
		{"limit=abc", http.StatusBadRequest},
		{"cursor=%%%", http.StatusBadRequest},
		{"cursor=bm90LWEtY3Vyc29y", http.StatusBadRequest},
		{"limit=1", http.StatusOK},
		{"limit=200", http.StatusOK},
	} {
		t.Run(c.query, func(t *testing.T) {
			before := src.calls.Load()
			a := h.call(t, http.MethodGet, "/sandboxes?"+c.query, h.Token, nil)
			if a.status != c.status {
				t.Fatalf("GET /sandboxes?%s = %d %s, want %d", c.query, a.status, a.body, c.status)
			}
			if c.status != http.StatusBadRequest {
				return
			}
			if a.err.ErrorType != "invalid_request" {
				t.Errorf("error_type = %q, want invalid_request", a.err.ErrorType)
			}
			if n := src.calls.Load() - before; n != 0 {
				t.Errorf("the source was called %d times, want 0", n)
			}
		})
	}
}

func TestSandboxList_IsEmptyWithoutASource(t *testing.T) {
	t.Parallel()
	h := startListHub(t, nil)
	a := h.call(t, http.MethodGet, "/sandboxes", h.Token, nil)
	if a.status != http.StatusOK || string(bytes.TrimSpace(a.body)) != `{"items":[],"next_cursor":null}` {
		t.Errorf("GET /sandboxes = %d %s, want 200 {\"items\":[],\"next_cursor\":null}", a.status, a.body)
	}
}

func TestSandboxList_AnswersAFailingSourceWithTheInternalErrorAndNoItems(t *testing.T) {
	t.Parallel()
	// The source's error is of a runtime kind, and still not the runtime's
	// answer: the list never comes from the runtime.
	for _, err := range []error{errors.New("db gone"), fmt.Errorf("db gone: %w", runtime.ErrUnavailable)} {
		t.Run(err.Error(), func(t *testing.T) {
			t.Parallel()
			h := startListHub(t, &memorySource{err: err})
			a := h.call(t, http.MethodGet, "/sandboxes", h.Token, nil)
			if a.status != http.StatusInternalServerError || a.err.ErrorType != "internal" || a.err.Message != "internal error" {
				t.Errorf("GET /sandboxes = %d %s, want 500 internal \"internal error\"", a.status, a.body)
			}
			var body map[string]any
			_ = json.Unmarshal(a.body, &body)
			if _, ok := body["items"]; ok || bytes.Contains(a.body, []byte("db gone")) {
				t.Errorf("the answer %s holds items or the source's error", a.body)
			}
		})
	}
}
