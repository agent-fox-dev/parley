package workspace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/agent-fox-dev/parley/internal/server"
)

// SettingKey is one key of a workspace's settings document: its name, its
// default and the rule its value is judged by. The keys and their rules
// belong to the package that registers them, so that this package applies
// and shows settings without knowing any key.
type SettingKey struct {
	// Name is the key as the settings document names it.
	Name string
	// Default is the key's value when it was never set, given the
	// workspace's default branch, nil while it is unknown.
	Default func(defaultBranch *string) any
	// Validate judges a value of the key that is not null; its error is the
	// reason the value is refused.
	Validate func(value json.RawMessage) error
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
// returns to its default, every other value must pass its key's rule. The
// keys raw does not name keep their stored value. A refusal is a 400
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
			out[k.Name] = v
		}
	}
	return out, nil
}

// EffectiveSettings returns every registered key with its stored value, or
// its default for the default branch defaultBranch when the stored document
// does not hold it. Keys the document holds that are no longer registered
// are left out. With no key registered it is the empty object.
func (r *SettingsRegistry) EffectiveSettings(stored map[string]json.RawMessage, defaultBranch *string) map[string]any {
	out := map[string]any{}
	for _, k := range r.registered() {
		if v, ok := stored[k.Name]; ok {
			out[k.Name] = v
			continue
		}
		out[k.Name] = k.Default(defaultBranch)
	}
	return out
}
