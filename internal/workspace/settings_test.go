package workspace_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// settingDefaults is every setting key with its default for the default
// branch main, as EffectiveSettings shows it.
var settingDefaults = map[string]any{
	"sync_enabled": true, "protected_branches": []string{"main"}, "protect.owner_push": true,
	"land.strategy": "rebase", "land.by_author": true, "land.delete_source": true, "land.allow_override": true,
	"changes.require_intent": true, "changes.auto_restack": true, "audit_fetches": false,
	"forge_api_url": "", "upstream_forge_api_url": "", "patch_branch_source": "hub",
	"patch_divergence_policy": "replace", "push_patches_to_origin": false, "auto_rebuild_after_push": true,
	"auto_rebuild_after_sync": true, "rebuild_strategy": "rebase", "rebuild_fail_mode": "fail_fast",
	"rebuild_push_integration_branch": false, "squash_merge_detection": "both",
}

// settingKeys is a registry holding the hub's setting keys.
func settingKeys(t *testing.T) *workspace.SettingsRegistry {
	t.Helper()
	var r workspace.SettingsRegistry
	if err := workspace.RegisterSettingKeys(&r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestSettings_ShowsEveryKeyWithItsStoredValueOrItsDefault(t *testing.T) {
	t.Parallel()
	r := settingKeys(t)
	main := "main"
	stored := map[string]json.RawMessage{"land.strategy": json.RawMessage(`"squash"`), "audit_fetches": json.RawMessage(`true`)}
	for _, c := range []struct {
		name   string
		stored map[string]json.RawMessage
		want   map[string]any
	}{
		{"nothing stored", map[string]json.RawMessage{}, settingDefaults},
		{"two keys stored", stored, func() map[string]any {
			want := maps.Clone(settingDefaults)
			want["land.strategy"], want["audit_fetches"] = "squash", true
			return want
		}()},
	} {
		got := r.EffectiveSettings(c.stored, &main)
		if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, slices.Sorted(maps.Keys(settingDefaults))) {
			t.Errorf("%s: keys = %v, want the %d setting keys", c.name, keys, len(settingDefaults))
		}
		for k, want := range c.want {
			if !reflect.DeepEqual(got[k], want) {
				t.Errorf("%s: %s = %#v, want %#v", c.name, k, got[k], want)
			}
		}
	}
}

func TestSettings_DefaultsProtectedBranchesToTheDefaultBranch(t *testing.T) {
	t.Parallel()
	r := settingKeys(t)
	for _, c := range []struct {
		name          string
		stored        string // the stored protected_branches, "" for unset
		defaultBranch *string
		want          string // the effective value as JSON
	}{
		{"unset, no default branch", "", nil, `[]`},
		{"unset, main", "", new("main"), `["main"]`},
		{"unset, trunk", "", new("trunk"), `["trunk"]`},
		{"the empty list, main", `[]`, new("main"), `[]`},
		{"a list, main", `["release/*"]`, new("main"), `["release/*"]`},
		{"a list, no default branch", `["release/*"]`, nil, `["release/*"]`},
	} {
		stored := map[string]json.RawMessage{}
		if c.stored != "" {
			stored["protected_branches"] = json.RawMessage(c.stored)
		}
		got, err := json.Marshal(r.EffectiveSettings(stored, c.defaultBranch)["protected_branches"])
		if err != nil || string(got) != c.want {
			t.Errorf("%s: protected_branches = %s (%v), want %s", c.name, got, err, c.want)
		}
	}
}
