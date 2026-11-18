package forge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Outcome is what the hub made of one webhook delivery.
type Outcome string

// The outcomes of a delivery. A reduction ends in one of the first four;
// OutcomeDuplicate is the answer to a delivery id already recorded, which
// is decided before any reduction.
const (
	OutcomeRelayed            Outcome = "relayed"
	OutcomeIgnored            Outcome = "ignored"
	OutcomeRepositoryMismatch Outcome = "repository_mismatch"
	OutcomeMalformed          Outcome = "malformed"
	OutcomeDuplicate          Outcome = "duplicate"
)

// maxActionBytes is the most bytes of a delivery's action the hub keeps, so
// that a record has a bounded size.
const maxActionBytes = 64

// Repos are the repositories, as owner/name, a workspace receives
// deliveries for: its origin's and, for a carry-patch workspace, its
// upstream's. "" names none.
type Repos struct {
	Origin   string
	Upstream string
}

// HookEvent is the draft of one hub event a delivery relays.
type HookEvent struct {
	// Type is the event type, parley.forge.<...>.
	Type         string
	ResourceType string
	// ResourceID is never empty: a mapping reads it from a required field.
	ResourceID string
	// Metadata is what the mapping read for the event itself, such as the
	// label of a label event; nil when nothing.
	Metadata map[string]any
}

// Reduction is what a delivery's payload comes to: the action and target
// it names, its outcome, and the events to relay, which only the outcome
// OutcomeRelayed has.
type Reduction struct {
	// Action is the payload's action, cut to 64 bytes; "" when it names
	// none.
	Action string
	// Target is the workspace's repository the payload names, nil when it
	// names neither; it is set whatever the outcome.
	Target  *Target
	Outcome Outcome
	Events  []HookEvent
}

// mapping is one forge's table from an event name and action to hub
// events. Given the payload, it answers errNoMapping when the table has no
// row for them, an error matching errMissingField when a field the row
// requires is missing or of the wrong type, and otherwise the events of
// the row in order, which may be none.
type mapping func(event, action string, payload map[string]any) ([]HookEvent, error)

// The answers of a mapping that yields no events.
var (
	errNoMapping    = errors.New("no mapping for the event and action")
	errMissingField = errors.New("a required field is missing or of the wrong type")
)

// mappingOf is the mapping of the forge kind; a kind without one maps
// nothing.
func mappingOf(kind ForgeKind) mapping {
	if kind == KindGitHub {
		return mapGitHub
	}
	return func(string, string, map[string]any) ([]HookEvent, error) { return nil, errNoMapping }
}

// Reduce decides what the hub makes of a verified delivery of the forge
// kind: event is the delivery's event name, body its raw payload, and repos
// the workspace's repositories. It does no I/O.
func Reduce(kind ForgeKind, event string, body []byte, repos Repos) Reduction {
	return reduce(kind, event, body, repos, mappingOf(kind))
}

// reduce is Reduce with the mapping m.
func reduce(kind ForgeKind, event string, body []byte, repos Repos, m mapping) Reduction {
	payload, ok := decodeObject(body)
	if !ok {
		return Reduction{Outcome: OutcomeMalformed}
	}
	actionPath, repoPath := []string{"action"}, []string{"repository", "full_name"}
	if kind == KindGitLab {
		actionPath, repoPath = []string{"object_attributes", "action"}, []string{"project", "path_with_namespace"}
	}
	action, _ := member(payload, actionPath...).(string)
	path, _ := member(payload, repoPath...).(string)
	red := Reduction{Action: cutUTF8(action, maxActionBytes), Target: repos.match(path)}

	events, err := m(event, action, payload)
	switch {
	case errors.Is(err, errNoMapping):
		red.Outcome = OutcomeIgnored
	case err != nil:
		red.Outcome = OutcomeMalformed
	case len(events) == 0:
		red.Outcome = OutcomeIgnored
	case path == "":
		red.Outcome = OutcomeMalformed
	case red.Target == nil:
		red.Outcome = OutcomeRepositoryMismatch
	default:
		red.Outcome, red.Events = OutcomeRelayed, events
	}
	return red
}

// match is the target whose repository is path, ignoring case: the origin
// first, then the upstream; nil when neither is.
func (r Repos) match(path string) *Target {
	switch {
	case path == "":
		return nil
	case r.Origin != "" && strings.EqualFold(path, r.Origin):
		return new(TargetOrigin)
	case r.Upstream != "" && strings.EqualFold(path, r.Upstream):
		return new(TargetUpstream)
	}
	return nil
}

// decodeObject decodes body as exactly one JSON value, which must be an
// object. Numbers stay json.Number, so that an integer is told from a
// fraction.
func decodeObject(body []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return obj, true
}

// member is the value at the path of object members in payload, nil when a
// step is missing or not an object.
func member(payload map[string]any, path ...string) any {
	var v any = payload
	for _, name := range path {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = obj[name]
	}
	return v
}

// requiredInt is the JSON integer of at least 1 at path in payload, or an
// error matching errMissingField.
func requiredInt(payload map[string]any, path ...string) (int64, error) {
	if n, ok := member(payload, path...).(json.Number); ok {
		if v, err := strconv.ParseInt(string(n), 10, 64); err == nil && v >= 1 {
			return v, nil
		}
	}
	return 0, missing(path)
}

// requiredString is the non-empty JSON string at path in payload, or an
// error matching errMissingField.
func requiredString(payload map[string]any, path ...string) (string, error) {
	if s, ok := member(payload, path...).(string); ok && s != "" {
		return s, nil
	}
	return "", missing(path)
}

func missing(path []string) error {
	return fmt.Errorf("%w: %s", errMissingField, strings.Join(path, "."))
}

// cutUTF8 is the longest prefix of s of at most n bytes that ends at a
// character boundary. A byte that does not start a valid character counts
// as one character of its own.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := 0
	for end < n {
		_, size := utf8.DecodeRuneInString(s[end:])
		if end+size > n {
			break
		}
		end += size
	}
	return s[:end]
}
