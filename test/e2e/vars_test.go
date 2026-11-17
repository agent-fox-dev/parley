package e2e

import (
	"slices"
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
