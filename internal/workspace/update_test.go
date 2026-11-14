package workspace_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// settingsJSON is settings as JSON, its keys in a fixed order, so that two
// settings objects compare whatever Go types hold them.
func settingsJSON(t *testing.T, settings any) string {
	t.Helper()
	b, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// withDefaults is settingDefaults with the keys of changes changed.
func withDefaults(changes map[string]any) map[string]any {
	out := maps.Clone(settingDefaults)
	maps.Copy(out, changes)
	return out
}

func TestUpdate_StoresTheGivenSettingsAndKeepsTheOthers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, DefaultBranch: new("main")})
	w.patch(t, w.uKey, "demo", map[string]any{"settings": map[string]any{"land.strategy": "squash"}}).decode(t, http.StatusOK)
	r := w.patch(t, w.uKey, "demo", map[string]any{"settings": map[string]any{
		"sync_enabled": false, "protected_branches": []string{"main", "release/**"}}})
	want := withDefaults(map[string]any{"land.strategy": "squash", "sync_enabled": false,
		"protected_branches": []string{"main", "release/**"}})
	if got := settingsJSON(t, r.decode(t, http.StatusOK)["settings"]); got != settingsJSON(t, want) {
		t.Errorf("settings = %s, want %s", got, settingsJSON(t, want))
	}
	if after := w.get(t, w.uKey, "demo"); !bytes.Equal(after.body, r.body) {
		t.Errorf("GET after the update = %s, want the update's answer %s", after.body, r.body)
	}
}

func TestUpdate_ReturnsSettingsGivenNullToTheirDefaults(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	stored := map[string]json.RawMessage{"land.strategy": json.RawMessage(`"squash"`),
		"protected_branches": json.RawMessage(`[]`), "audit_fetches": json.RawMessage(`true`)}
	for _, c := range []struct {
		slug     string
		settings any
		want     map[string]any
	}{
		{"null-keys", map[string]any{"land.strategy": nil, "protected_branches": nil}, withDefaults(map[string]any{"audit_fetches": true})},
		{"null-settings", nil, settingDefaults},
	} {
		w.put(t, workspace.Workspace{Slug: c.slug, Status: workspace.StatusReady, DefaultBranch: new("main"), Settings: stored})
		r := w.patch(t, w.uKey, c.slug, map[string]any{"settings": c.settings})
		if got := settingsJSON(t, r.decode(t, http.StatusOK)["settings"]); got != settingsJSON(t, c.want) {
			t.Errorf("%s: settings = %s, want %s", c.slug, got, settingsJSON(t, c.want))
		}
	}
}

func TestUpdate_RefusesAnInvalidSettingsValueChangingNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, DefaultBranch: new("main")})
	for _, c := range []struct {
		body any
		want string // what the message names
	}{
		{map[string]any{"description": "new", "settings": map[string]any{"sync_enabled": false, "land.strategy": "merge"}}, "land.strategy"},
		{map[string]any{"settings": []any{}}, "settings"},
		{map[string]any{"settings": map[string]any{"nope": 1}}, `unknown setting "nope"`},
	} {
		before := w.get(t, w.uKey, "demo")
		w.advance(time.Minute)
		r := w.patch(t, w.uKey, "demo", c.body)
		if errorType, msg := r.refusal(t); r.status != http.StatusBadRequest || errorType != "invalid_request" || !strings.Contains(msg, c.want) {
			t.Errorf("%v = %d %s, want 400 invalid_request naming %s", c.body, r.status, r.body, c.want)
		}
		if after := w.get(t, w.uKey, "demo"); !bytes.Equal(after.body, before.body) {
			t.Errorf("%v: the workspace = %s, want it unchanged %s", c.body, after.body, before.body)
		}
	}
}
