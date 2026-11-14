package secret

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// decodeEntries reads the body of a create request, {"entries": [{"key",
// "value"}, ...]}, and checks it in a fixed order, so that a request with
// several faults is always answered with the same one: entries is a
// non-empty array; then, entry by entry, its key is a legal key and its
// value is absent, stored as the empty string, or a JSON string within the
// size limit; then no two keys are equal ignoring ASCII case. The first
// fault is answered 400 invalid_request, naming the entry's key where it
// has one. Members other than entries, key and value are ignored.
func decodeEntries(r *http.Request) ([]Entry, error) {
	var body struct {
		Entries json.RawMessage `json:"entries"`
	}
	if err := server.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	var raws []json.RawMessage
	if len(body.Entries) == 0 || json.Unmarshal(body.Entries, &raws) != nil || len(raws) == 0 {
		return nil, server.InvalidRequest("entries must be a non-empty array of {key, value} objects")
	}
	entries := make([]Entry, len(raws))
	for i, raw := range raws {
		e, err := decodeEntry(i, raw)
		if err != nil {
			return nil, err
		}
		entries[i] = e
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		lower := asciiLower(e.Key)
		if seen[lower] {
			return nil, server.InvalidRequest(fmt.Sprintf("key %q is given more than once, ignoring case", e.Key))
		}
		seen[lower] = true
	}
	return entries, nil
}

// decodeEntry decodes and checks the entry at index i. The members are
// matched by their exact names, so that a value is either present or not.
func decodeEntry(i int, raw json.RawMessage) (Entry, error) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || members == nil {
		return Entry{}, server.InvalidRequest(fmt.Sprintf("entries[%d] must be an object", i))
	}
	var e Entry
	if !isString(members["key"]) || json.Unmarshal(members["key"], &e.Key) != nil {
		return Entry{}, server.InvalidRequest(fmt.Sprintf("entries[%d] must have a key that is a string", i))
	}
	if err := CheckKey(e.Key); err != nil {
		return Entry{}, server.InvalidRequest(err.Error())
	}
	if v, ok := members["value"]; ok {
		if !isString(v) || json.Unmarshal(v, &e.Value) != nil {
			return Entry{}, server.InvalidRequest(fmt.Sprintf("the value of key %q must be a string", e.Key))
		}
	}
	if err := CheckValue(e.Key, e.Value); err != nil {
		return Entry{}, server.InvalidRequest(err.Error())
	}
	return e, nil
}

// isString reports whether raw is a JSON string; null is not one.
func isString(raw json.RawMessage) bool { return len(raw) > 0 && raw[0] == '"' }
