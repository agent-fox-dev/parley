package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/agent-fox-dev/parley/internal/server"
)

// SettingKey is one key of a workspace's settings document: its name, its
// default and the rule its value is judged by. The registry applies and
// shows settings through these alone; RegisterSettingKeys registers the
// keys of the settings table.
type SettingKey struct {
	// Name is the key as the settings document names it.
	Name string
	// Default is the key's value when it was never set, given the
	// workspace's default branch, nil while it is unknown.
	Default func(defaultBranch *string) any
	// Validate judges a value of the key that is not null; its error is the
	// reason the value is refused.
	Validate func(value json.RawMessage) error
	// Normalize returns the form of a value Validate accepted that is
	// stored; nil stores the value as given.
	Normalize func(value json.RawMessage) json.RawMessage
}

// SettingsRegistry holds the setting keys in the order they were registered,
// which is the order a settings value is judged in. Its zero value holds no
// key and is ready to use.
type SettingsRegistry struct {
	mu   sync.RWMutex
	keys []SettingKey
}

// RegisterSetting adds k after the keys registered before it. A key without
// a name, a default or a rule, or with the name of a registered key, is
// refused.
func (r *SettingsRegistry) RegisterSetting(k SettingKey) error {
	if k.Name == "" || k.Default == nil || k.Validate == nil {
		return fmt.Errorf("setting %q needs a name, a default and a rule", k.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.ContainsFunc(r.keys, func(have SettingKey) bool { return have.Name == k.Name }) {
		return fmt.Errorf("setting %q is registered twice", k.Name)
	}
	r.keys = append(r.keys, k)
	return nil
}

// registered is a copy of the registered keys.
func (r *SettingsRegistry) registered() []SettingKey {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.keys)
}

// ApplySettings returns the stored settings document after applying raw, the
// settings value of a request, to stored, which it does not change. A raw
// value of null returns every key to its default; any other value must be an
// object, each of whose keys is registered: a key with the value null
// returns to its default, every other value must pass its key's rule and is
// stored in its key's normal form. The keys raw does not name keep their
// stored value. A refusal is a 400
// invalid_request naming the first unknown key, else the first refused key
// in registration order; nothing is applied then.
func (r *SettingsRegistry) ApplySettings(stored map[string]json.RawMessage, raw json.RawMessage) (map[string]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return map[string]json.RawMessage{}, nil
	}
	var given map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &given) != nil {
		return nil, server.InvalidRequest("settings: must be an object or null")
	}
	keys := r.registered()
	for _, name := range slices.Sorted(maps.Keys(given)) {
		if !slices.ContainsFunc(keys, func(k SettingKey) bool { return k.Name == name }) {
			return nil, server.InvalidRequest(fmt.Sprintf("unknown setting %q", name))
		}
	}
	out := maps.Clone(stored)
	if out == nil {
		out = map[string]json.RawMessage{}
	}
	for _, k := range keys {
		v, ok := given[k.Name]
		switch {
		case !ok:
		case bytes.Equal(bytes.TrimSpace(v), []byte("null")):
			delete(out, k.Name)
		default:
			if err := k.Validate(v); err != nil {
				return nil, server.InvalidRequest(fmt.Sprintf("settings.%s: %v", k.Name, err))
			}
			if k.Normalize != nil {
				v = k.Normalize(v)
			}
			out[k.Name] = v
		}
	}
	return out, nil
}

// EffectiveSettings returns every registered key with its stored value, or
// its default for the default branch defaultBranch when the stored document
// does not hold it. A stored value is decoded into the Go type of the key's
// default (a bool, a string, a []string), so that a reader gets one type
// per key whether the key was set or not; a stored value that does not
// decode so is returned as stored. Keys the document holds that are no
// longer registered are left out.
func (r *SettingsRegistry) EffectiveSettings(stored map[string]json.RawMessage, defaultBranch *string) map[string]any {
	out := map[string]any{}
	for _, k := range r.registered() {
		def := k.Default(defaultBranch)
		v, ok := stored[k.Name]
		if !ok {
			out[k.Name] = def
			continue
		}
		out[k.Name] = decodeLike(def, v)
	}
	return out
}

// decodeLike is raw decoded into a value of the dynamic type of def, or raw
// itself when def is nil or raw does not decode into that type.
func decodeLike(def any, raw json.RawMessage) any {
	if def == nil {
		return raw
	}
	p := reflect.New(reflect.TypeOf(def))
	if json.Unmarshal(raw, p.Interface()) != nil {
		return raw
	}
	return p.Elem().Interface()
}

// settingKind is the type of a setting key, which decides the values it
// accepts.
type settingKind int

const (
	kindBool  settingKind = iota // a JSON boolean
	kindEnum                     // one of the key's listed strings
	kindURL                      // "" or an absolute https URL
	kindGlobs                    // a list of branch globs
)

// settingSpec is one row of the settings table: a key, its kind, the strings
// an enum key accepts and the default of every kind but kindGlobs, whose
// default follows the workspace's default branch.
type settingSpec struct {
	name   string
	kind   settingKind
	values []string
	def    any
}

// maxProtectedBranches is the most globs protected_branches may hold.
const maxProtectedBranches = 50

// settingsTable is the settings document of every workspace: each key with
// its kind and default, in the order the keys are judged in. It is the one
// definition of the keys; the defaults, the rules and the order all read it.
var settingsTable = []settingSpec{
	{name: "sync_enabled", kind: kindBool, def: true},
	{name: "protected_branches", kind: kindGlobs},
	{name: "protect.owner_push", kind: kindBool, def: true},
	{name: "land.strategy", kind: kindEnum, values: []string{"rebase", "squash"}, def: "rebase"},
	{name: "land.by_author", kind: kindBool, def: true},
	{name: "land.delete_source", kind: kindBool, def: true},
	{name: "land.allow_override", kind: kindBool, def: true},
	{name: "changes.require_intent", kind: kindBool, def: true},
	{name: "changes.auto_restack", kind: kindBool, def: true},
	{name: "audit_fetches", kind: kindBool, def: false},
	{name: "forge_api_url", kind: kindURL, def: ""},
	{name: "upstream_forge_api_url", kind: kindURL, def: ""},
	{name: "patch_branch_source", kind: kindEnum, values: []string{"hub", "origin"}, def: "hub"},
	{name: "patch_divergence_policy", kind: kindEnum, values: []string{"replace", "report"}, def: "replace"},
	{name: "push_patches_to_origin", kind: kindBool, def: false},
	{name: "auto_rebuild_after_push", kind: kindBool, def: true},
	{name: "auto_rebuild_after_sync", kind: kindBool, def: true},
	{name: "rebuild_strategy", kind: kindEnum, values: []string{"rebase", "merge"}, def: "rebase"},
	{name: "rebuild_fail_mode", kind: kindEnum, values: []string{"fail_fast", "continue"}, def: "fail_fast"},
	{name: "rebuild_push_integration_branch", kind: kindBool, def: false},
	{name: "squash_merge_detection", kind: kindEnum, values: []string{"ancestry_only", "content_based", "both"}, def: "both"},
}

// RegisterSettingKeys registers every key of the settings table on r, in
// the table's order. The hub calls it once on its workspace service's
// registry; an error means r already held one of the keys.
func RegisterSettingKeys(r *SettingsRegistry) error {
	for _, spec := range settingsTable {
		k := SettingKey{Name: spec.name, Default: spec.defaultFunc(), Validate: spec.validate}
		if spec.kind == kindURL {
			k.Normalize = trimString
		}
		if err := r.RegisterSetting(k); err != nil {
			return err
		}
	}
	return nil
}

// defaultFunc is the default of the key: its fixed default, or for a list
// of branch globs the list holding the default branch, empty (never null)
// while the default branch is unknown.
func (spec settingSpec) defaultFunc() func(*string) any {
	if spec.kind == kindGlobs {
		return func(defaultBranch *string) any {
			if defaultBranch == nil {
				return []string{}
			}
			return []string{*defaultBranch}
		}
	}
	return func(*string) any { return spec.def }
}

// validate judges a value of the key, which is not null, by its kind. Its
// errors never repeat the value, which for a URL could carry a secret.
func (spec settingSpec) validate(raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	switch spec.kind {
	case kindBool:
		if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
			return errors.New("must be a boolean")
		}
	case kindEnum:
		if s, ok := jsonString(raw); !ok || !slices.Contains(spec.values, s) {
			return fmt.Errorf("must be one of %q", spec.values)
		}
	case kindURL:
		s, ok := jsonString(raw)
		if !ok {
			return errors.New("must be a string")
		}
		return validateForgeURL(strings.TrimSpace(s))
	case kindGlobs:
		return validateGlobs(raw)
	}
	return nil
}

// jsonString is raw decoded as a JSON string; ok is false for any other
// JSON value.
func jsonString(raw json.RawMessage) (s string, ok bool) {
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// trimString is the JSON string raw without its surrounding white space, so
// that a URL key stores the URL its rule judged; raw is a JSON string.
func trimString(raw json.RawMessage) json.RawMessage {
	s, _ := jsonString(bytes.TrimSpace(raw))
	b, _ := json.Marshal(strings.TrimSpace(s)) // a string always marshals
	return b
}

// validateForgeURL accepts "" and an absolute https URL with a host and
// without user information, query or fragment.
func validateForgeURL(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	switch {
	case err != nil || !u.IsAbs() || u.Scheme != "https" || u.Hostname() == "":
		return errors.New(`must be "" or an absolute https:// URL with a host`)
	case u.User != nil:
		return errors.New("must not hold user information")
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("must not hold a query")
	case u.Fragment != "" || strings.Contains(s, "#"):
		return errors.New("must not hold a fragment")
	}
	return nil
}

// validateGlobs accepts a JSON array of at most maxProtectedBranches
// distinct branch globs.
func validateGlobs(raw json.RawMessage) error {
	var items []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &items) != nil {
		return errors.New("must be a list of branch globs")
	}
	if len(items) > maxProtectedBranches {
		return fmt.Errorf("must hold at most %d branch globs", maxProtectedBranches)
	}
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		g, ok := jsonString(item)
		if !ok {
			return fmt.Errorf("item %d: must be a string", i)
		}
		if err := ValidateBranchGlob(g); err != nil {
			return fmt.Errorf("item %d: %w", i, err)
		}
		if seen[g] {
			return fmt.Errorf("item %d: repeats an earlier item", i)
		}
		seen[g] = true
	}
	return nil
}
