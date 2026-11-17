package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// varEntries are the key, value and origin of the items of the list r
// printed, one "key=value@origin" each, "@" alone for an item without an
// origin.
func varEntries(t *testing.T, r ran) []string {
	t.Helper()
	keys, values, origins := itemValues(t, r, "key"), itemValues(t, r, "value"), itemValues(t, r, "origin")
	entries := []string{}
	for i := range keys {
		origin, _ := origins[i].(string)
		entries = append(entries, keys[i].(string)+"="+values[i].(string)+"@"+origin)
	}
	return entries
}

// A developer sets a value at the org and at the workspace, reads what the
// workspace runs with, deletes the workspace's value and reads the org's in
// its place; every change is in the event log, and no event holds a value.
func TestVars_ADeveloperSetsValuesAtTwoLevelsAndReadsWhatTheWorkspaceRunsWith(t *testing.T) {
	parallel(t)
	h := hubtest.Start(t, nil)
	dev := h.CreateUser(t, "dev")
	org := h.CreateOrg(t, "o")
	h.AddMember(t, org, dev)
	h.AddWorkspace(t, access.Record{Slug: "w", OwnerID: dev, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady})
	key := h.IssueKey(t, dev)

	call := func(method, path string, body any, want int) []byte {
		t.Helper()
		status, b := h.Call(t, method, "/api/v1"+path, key, body)
		if status != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, status, b, want)
		}
		return b
	}
	entry := func(k, v string) map[string]any {
		return map[string]any{"entries": []any{map[string]any{"key": k, "value": v}}}
	}
	resolved := func() []string {
		t.Helper()
		var page struct {
			Items []struct{ Key, Value, Origin string } `json:"items"`
		}
		if err := json.Unmarshal(call(http.MethodGet, "/workspaces/w/vars/resolved", nil, http.StatusOK), &page); err != nil {
			t.Fatalf("decode the resolved variables: %v", err)
		}
		out := []string{}
		for _, it := range page.Items {
			out = append(out, it.Key+"="+it.Value+"@"+it.Origin)
		}
		return out
	}

	call(http.MethodPost, "/orgs/o/vars", entry("LOG_LEVEL", "info"), http.StatusCreated)
	call(http.MethodPost, "/workspaces/w/vars", entry("log_level", "debug"), http.StatusCreated)
	if got, want := resolved(), []string{"log_level=debug@workspace"}; !slices.Equal(got, want) {
		t.Errorf("resolved before the delete = %v, want %v", got, want)
	}
	call(http.MethodDelete, "/workspaces/w/vars/LOG_LEVEL", nil, http.StatusNoContent)
	if got, want := resolved(), []string{"LOG_LEVEL=info@org"}; !slices.Equal(got, want) {
		t.Errorf("resolved after the delete = %v, want %v", got, want)
	}
	if b := call(http.MethodGet, "/workspaces/w/vars", nil, http.StatusOK); !strings.Contains(string(b), `"items":[]`) {
		t.Errorf("the workspace's own variables = %s, want no items", b)
	}

	status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?after=0&limit=200", h.Token, nil)
	var log struct {
		Items []json.RawMessage `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &log) != nil {
		t.Fatalf("GET events/log = %d %s, want 200 with a page", status, body)
	}
	types := []string{}
	for _, raw := range log.Items {
		var ev struct{ Type string }
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode the event %s: %v", raw, err)
		}
		if !strings.HasPrefix(ev.Type, "parley.variable.") {
			continue
		}
		types = append(types, ev.Type)
		if strings.Contains(string(raw), `"info"`) || strings.Contains(string(raw), `"debug"`) {
			t.Errorf("the event %s holds a variable's value", raw)
		}
	}
	if want := []string{"parley.variable.create", "parley.variable.create", "parley.variable.delete"}; !slices.Equal(types, want) {
		t.Errorf("the variable events = %v, want %v", types, want)
	}
}

func TestVars_ADeveloperManagesVariablesFromTheShell(t *testing.T) {
	parallel(t)
	h := &identityHub{hubtest.Start(t, nil)}
	alice := h.CreateUser(t, "alice")
	org := h.CreateOrg(t, "acme")
	h.AddWorkspace(t, access.Record{Slug: "w", OwnerID: alice, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady})
	key := h.IssueKey(t, alice)

	created := h.must(t, key, "vars", "create", "A=1,B=x=y", "--user", "--workspace", "w")
	results, _ := created.json(t)["results"].([]any)
	scopes := []string{}
	for _, r := range results {
		result, _ := r.(map[string]any)
		scope, _ := result["scope"].(string)
		keys := []string{}
		items, _ := result["items"].([]any)
		for _, it := range items {
			item, _ := it.(map[string]any)
			k, _ := item["key"].(string)
			keys = append(keys, k)
		}
		if !slices.Equal(keys, []string{"A", "B"}) {
			t.Errorf("create printed the keys %v for the %s scope, want [A B]", keys, scope)
		}
		scopes = append(scopes, scope)
	}
	if !slices.Equal(scopes, []string{"user", "workspace"}) {
		t.Errorf("create printed results for %v, want [user workspace]: %s", scopes, created.stdout)
	}

	if got, want := varEntries(t, h.must(t, key, "vars", "resolve", "w")), []string{"A=1@workspace", "B=x=y@workspace"}; !slices.Equal(got, want) {
		t.Errorf("resolve printed %v, want %v", got, want)
	}
	if got, want := varEntries(t, h.must(t, key, "vars", "list", "--user")), []string{"A=1@", "B=x=y@"}; !slices.Equal(got, want) {
		t.Errorf("list --user printed %v, want %v", got, want)
	}
}
