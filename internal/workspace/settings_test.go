package workspace_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/server"
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

// applied applies the settings value raw, JSON text, to an empty stored
// document and returns the effective settings for the default branch main.
func applied(r *workspace.SettingsRegistry, raw string) (map[string]any, error) {
	stored, err := r.ApplySettings(nil, json.RawMessage(raw))
	if err != nil {
		return nil, err
	}
	return r.EffectiveSettings(stored, new("main")), nil
}

// invalidRequest is the message of err, which must be a 400
// invalid_request.
func invalidRequest(t *testing.T, err error) string {
	t.Helper()
	e, ok := errors.AsType[*server.Error](err)
	if !ok || e.Status() != http.StatusBadRequest || e.Type() != "invalid_request" {
		t.Fatalf("error = %v, want a 400 invalid_request", err)
	}
	return e.Message()
}

// globList is a JSON array of n distinct branch names.
func globList(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("b%d", i)
	}
	b, _ := json.Marshal(names)
	return string(b)
}

func TestSettings_StoresAValueItsKeyAcceptsAndRefusesAnyOther(t *testing.T) {
	t.Parallel()
	r := settingKeys(t)
	type row struct{ key, value, want string } // want is the effective value as JSON, "" for a refusal
	var rows []row
	enums := map[string][]string{
		"land.strategy": {"rebase", "squash"}, "patch_branch_source": {"hub", "origin"},
		"patch_divergence_policy": {"replace", "report"}, "rebuild_strategy": {"rebase", "merge"},
		"rebuild_fail_mode": {"fail_fast", "continue"}, "squash_merge_detection": {"ancestry_only", "content_based", "both"},
	}
	for key, def := range settingDefaults {
		if _, ok := def.(bool); ok {
			rows = append(rows, row{key, `true`, `true`}, row{key, `false`, `false`})
			for _, bad := range []string{`"true"`, `1`, `0`, `[]`, `{}`} {
				rows = append(rows, row{key, bad, ""})
			}
		}
	}
	for key, values := range enums {
		for _, v := range values {
			rows = append(rows, row{key, `"` + v + `"`, `"` + v + `"`})
		}
		for _, bad := range []string{`"Rebase"`, `""`, `"other"`, `true`, `3`} {
			rows = append(rows, row{key, bad, ""})
		}
	}
	for _, key := range []string{"forge_api_url", "upstream_forge_api_url"} {
		for value, want := range map[string]string{
			`"https://api.github.com"`: `"https://api.github.com"`, `"  https://ghe.example/api/v3  "`: `"https://ghe.example/api/v3"`,
			`"https://ghe.example:8443/api/v3/"`: `"https://ghe.example:8443/api/v3/"`, `""`: `""`, `"   "`: `""`,
		} {
			rows = append(rows, row{key, value, want})
		}
		for _, bad := range []string{`"http://api.example"`, `"api.example/v3"`, `"/api/v3"`, `"https://"`, `"https:///path"`,
			`"https://user@api.example"`, `"https://user:pw@api.example"`, `"https://api.example/v3?x=1"`,
			`"https://api.example/v3#frag"`, `"ssh://api.example"`, `5`, `true`, `[]`} {
			rows = append(rows, row{key, bad, ""})
		}
	}
	long := "release/" + strings.Repeat("x", 246) + "*" // 255 characters
	for _, list := range []string{`[]`, `["main"]`, `["main","release/*","feature/**","*"]`, globList(50), `["` + long + `"]`} {
		rows = append(rows, row{"protected_branches", list, list})
	}
	for _, bad := range []string{`"main"`, `5`, `{}`, `["main",5]`, `[""]`, `["main","main"]`, globList(51), `["` + long + `x"]`,
		`["a b"]`, `["a..b"]`, `["-x"]`, `["x/"]`, `["x.lock"]`, `["a~b"]`, `["a^b"]`, `["a:b"]`, `["a?b"]`, `["a[b"]`,
		`["a\\b"]`, `["@"]`, `["a//b"]`, `["a@{b"]`, `[".hidden/x"]`} {
		rows = append(rows, row{"protected_branches", bad, ""})
	}
	for _, c := range rows {
		got, err := applied(r, `{"`+c.key+`":`+c.value+`}`)
		if c.want == "" {
			if msg := invalidRequest(t, err); !strings.Contains(msg, c.key) || strings.Contains(msg, "pw") {
				t.Errorf("%s = %s: message %q, want one naming the key and not repeating the value", c.key, c.value, msg)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s = %s: %v, want it stored", c.key, c.value, err)
			continue
		}
		if b, _ := json.Marshal(got[c.key]); string(b) != c.want {
			t.Errorf("%s = %s: effective value %s, want %s", c.key, c.value, b, c.want)
		}
	}
}

func TestSettings_RefusesNamingOneKeyUnknownKeysFirst(t *testing.T) {
	t.Parallel()
	r := settingKeys(t)
	for _, c := range []struct {
		value  string
		want   string
		unwant []string
	}{
		{`"x"`, "settings", nil}, {`5`, "settings", nil}, {`true`, "settings", nil}, {`[]`, "settings", nil},
		{`{"nope":1}`, `unknown setting "nope"`, nil},
		{`{"land.Strategy":"squash"}`, `unknown setting "land.Strategy"`, nil},
		{`{"land":{"strategy":"squash"}}`, `unknown setting "land"`, nil},
		{`{"":true}`, `unknown setting ""`, nil},
		// Members in an order other than the table's.
		{`{"squash_merge_detection":"x","sync_enabled":"x"}`, "sync_enabled", []string{"squash_merge_detection"}},
		{`{"rebuild_strategy":1,"land.strategy":1,"protected_branches":1}`, "protected_branches",
			[]string{"rebuild_strategy", "land.strategy"}},
		{`{"forge_api_url":"http://x","audit_fetches":1}`, "audit_fetches", []string{"forge_api_url"}},
		{`{"sync_enabled":"x","zzz":1,"aaa":1}`, `unknown setting "aaa"`, []string{"zzz", "sync_enabled"}},
	} {
		_, err := applied(r, c.value)
		msg := invalidRequest(t, err)
		if !strings.Contains(msg, c.want) || slices.ContainsFunc(c.unwant, func(k string) bool { return strings.Contains(msg, k) }) {
			t.Errorf("%s: message %q, want it to name %s and none of %v", c.value, msg, c.want, c.unwant)
		}
	}
}

func TestSettings_ApplyToACreateFromTheDefaults(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	obj := w.create(t, w.uKey, map[string]any{"slug": "demo", "git_url": "https://git.example/x/demo",
		"settings": map[string]any{"land.strategy": "squash", "sync_enabled": nil}}).object(t)
	s, _ := obj["settings"].(map[string]any)
	if s["land.strategy"] != "squash" || s["sync_enabled"] != true || !reflect.DeepEqual(s["protected_branches"], []any{}) {
		t.Errorf("settings = %v, want land.strategy squash, sync_enabled true, protected_branches []", s)
	}
	bad := w.create(t, w.uKey, map[string]any{"slug": "demo2", "git_url": "https://git.example/x/demo2",
		"settings": map[string]any{"land.strategy": "merge"}})
	if bad.status != http.StatusBadRequest || bad.errorType(t) != "invalid_request" {
		t.Errorf("an invalid settings value = %d %s, want 400 invalid_request", bad.status, bad.body)
	}
	if a := w.get(t, w.uKey, "demo2"); a.status != http.StatusNotFound {
		t.Errorf("GET demo2 = %d %s, want 404", a.status, a.body)
	}
}
