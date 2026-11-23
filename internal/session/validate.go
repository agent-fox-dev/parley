package session

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/server"
)

// maxLabel is the longest node_id, archetype or model, in characters.
const maxLabel = 255

// runIDForm is the form of a run id: YYYYMMDD_HHMMSS_ and six lower-case
// hexadecimal digits.
var runIDForm = regexp.MustCompile(`^\d{8}_\d{6}_[0-9a-f]{6}$`)

// isNull reports whether the member raw of a body is absent or null.
func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// optionalString reads the member name of body as a string: nil when it is
// absent or null, and 400 naming the member when it is not a string.
func optionalString(body map[string]json.RawMessage, name string) (*string, error) {
	raw := body[name]
	if isNull(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, server.InvalidRequest(name + " must be a string")
	}
	return &s, nil
}

// label reads the label name of body (node_id, archetype or model): a
// string of at most maxLabel characters, or nil when absent.
func label(body map[string]json.RawMessage, name string) (*string, error) {
	s, err := optionalString(body, name)
	if err != nil || s == nil {
		return s, err
	}
	if utf8.RuneCountInString(*s) > maxLabel {
		return nil, server.InvalidRequest(fmt.Sprintf("%s must be at most %d characters", name, maxLabel))
	}
	return s, nil
}

// sessionIDOf reads the member name of body as a session id: a UUID in any
// letter case, returned in lower case, or "" when absent.
func sessionIDOf(body map[string]json.RawMessage, name string) (string, error) {
	s, err := optionalString(body, name)
	if err != nil || s == nil {
		return "", err
	}
	if !server.IsUUID(*s) {
		return "", server.InvalidRequest(name + " must be a UUID in the 8-4-4-4-12 form")
	}
	return strings.ToLower(*s), nil
}

// runIDOf reads the run_id of body, nil when absent.
func runIDOf(body map[string]json.RawMessage) (*string, error) {
	s, err := optionalString(body, "run_id")
	if err != nil || s == nil {
		return s, err
	}
	if !runIDForm.MatchString(*s) {
		return nil, server.InvalidRequest("run_id must have the form YYYYMMDD_HHMMSS_ and six lower-case hexadecimal digits")
	}
	return s, nil
}
