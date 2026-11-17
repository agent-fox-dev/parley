package secret

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// decodeEntries reads the body of a create request of secrets, {"entries":
// [{"key", "value"}, ...]}, and checks it in a fixed order, so that a request
// with several faults is always answered with the same one: entries is a
// non-empty array; then, entry by entry, its key is a legal key and its
// value is absent, stored as the empty string, or a JSON string within the
// size limit; then no two keys are equal ignoring ASCII case. The first
// fault is answered 400 invalid_request, naming the entry's key where it
// has one. Members other than entries, key and value are ignored.
func decodeEntries(r *http.Request) ([]Entry, error) { return decodeBody(r, false) }

// decodeVarEntries reads the body of a create request of variables as
// decodeEntries does, except that it checks every entry's key before any
// entry's value: a request with an invalid key is answered naming that key,
// whatever fault an earlier entry's value has.
func decodeVarEntries(r *http.Request) ([]Entry, error) { return decodeBody(r, true) }

// decodeBody reads and checks a create body, with every key checked before
// any value when keysFirst, else entry by entry.
func decodeBody(r *http.Request, keysFirst bool) ([]Entry, error) {
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
	members := make([]map[string]json.RawMessage, len(raws))
	for i, raw := range raws {
		var err error
		if members[i], entries[i].Key, err = decodeEntryKey(i, raw); err != nil {
			return nil, err
		}
		if keysFirst {
			continue
		}
		if entries[i].Value, err = decodeEntryValue(entries[i].Key, members[i]); err != nil {
			return nil, err
		}
	}
	if keysFirst {
		for i := range entries {
			var err error
			if entries[i].Value, err = decodeEntryValue(entries[i].Key, members[i]); err != nil {
				return nil, err
			}
		}
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

// decodeEntryKey decodes the entry at index i and checks its key. The members
// are matched by their exact names, so that a value is either present or
// not.
func decodeEntryKey(i int, raw json.RawMessage) (map[string]json.RawMessage, string, error) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || members == nil {
		return nil, "", server.InvalidRequest(fmt.Sprintf("entries[%d] must be an object", i))
	}
	var key string
	if !isString(members["key"]) || json.Unmarshal(members["key"], &key) != nil {
		return nil, "", server.InvalidRequest(fmt.Sprintf("entries[%d] must have a key that is a string", i))
	}
	if err := CheckKey(key); err != nil {
		return nil, "", server.InvalidRequest(err.Error())
	}
	return members, key, nil
}

// decodeEntryValue checks the value of the entry of key: absent is the
// empty string, otherwise a JSON string within the size limit.
func decodeEntryValue(key string, members map[string]json.RawMessage) (string, error) {
	var value string
	if v, ok := members["value"]; ok {
		if !isString(v) || json.Unmarshal(v, &value) != nil {
			return "", server.InvalidRequest(fmt.Sprintf("the value of key %q must be a string", key))
		}
	}
	if err := CheckValue(key, value); err != nil {
		return "", server.InvalidRequest(err.Error())
	}
	return value, nil
}

// isString reports whether raw is a JSON string; null is not one.
func isString(raw json.RawMessage) bool { return len(raw) > 0 && raw[0] == '"' }
