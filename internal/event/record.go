package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// ErrInvalidEvent is returned, wrapped with the name of the offending field,
// by an emit call whose input breaks the record contract. Nothing is written
// then.
var ErrInvalidEvent = errors.New("invalid event")

// The actor types an event may name.
const (
	ActorAdminToken = "admin_token"
	ActorAPIKey     = "api_key"
	ActorPAT        = "pat"
	ActorForge      = "forge"
	ActorSystem     = "system"
)

// Input is what an emitting package supplies for one event. The component
// adds seq, id, time and action.
type Input struct {
	// Type is the event type, such as parley.change.land: at least three
	// dot-separated, non-empty segments beginning with parley.
	Type string
	// Workspace is the slug of the workspace the event concerns; nil for an
	// event of no workspace.
	Workspace *string
	// ActorType is one of the Actor constants.
	ActorType string
	// ActorID is the user the credential acts for: set for api_key and pat,
	// nil for admin_token, forge and system.
	ActorID *string
	// ResourceType and ResourceID name what the event is about; neither may
	// be empty.
	ResourceType string
	ResourceID   string
	// SessionID is the agent session the event belongs to, a UUID in the
	// 8-4-4-4-12 form; nil for none. It is stored in lower case.
	SessionID *string
	// Metadata is anything encoding/json encodes as a JSON object, such as a
	// map[string]any or a json.RawMessage; nil for none, stored as {}. A
	// list of paths is built with PathList.
	Metadata any
	// Parent is the slug of the fork's parent when Workspace is a task fork.
	// The component reads it from the workspace record when it is empty; a
	// caller that deletes the fork's record before it emits passes it.
	Parent string
}

// Record is one stored hub event. Its JSON form is the one every surface
// serves: the twelve members in a fixed order on one line.
type Record struct {
	Seq          int64
	ID           string
	Type         string
	Time         time.Time
	Workspace    *string
	ActorType    string
	ActorID      *string
	ResourceType string
	ResourceID   string
	Action       string
	SessionID    *string
	// Metadata is a JSON object, {} when the event has none.
	Metadata json.RawMessage

	// parent is the slug of the parent of a task fork the event concerns,
	// kept so that the fork's events stay visible to the parent's readers
	// after the fork is deleted. Consumers never see it.
	parent string
}

// wireRecord is the JSON form of a Record. The pointers have no omitempty,
// so an absent value is written as null.
type wireRecord struct {
	Seq          int64           `json:"seq"`
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Time         string          `json:"time"`
	Workspace    *string         `json:"workspace"`
	ActorType    string          `json:"actor_type"`
	ActorID      *string         `json:"actor_id"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Action       string          `json:"action"`
	SessionID    *string         `json:"session_id"`
	Metadata     json.RawMessage `json:"metadata"`
}

// MarshalJSON writes the record as one line, without HTML escaping, so that
// the stream, the log and the audit query serve the same bytes.
func (r Record) MarshalJSON() ([]byte, error) {
	meta := r.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	return encode(wireRecord{
		Seq: r.Seq, ID: r.ID, Type: r.Type, Time: timefmt.Format(r.Time), Workspace: r.Workspace,
		ActorType: r.ActorType, ActorID: r.ActorID, ResourceType: r.ResourceType, ResourceID: r.ResourceID,
		Action: r.Action, SessionID: r.SessionID, Metadata: meta,
	})
}

// encode writes v as compact JSON without HTML escaping and without the
// encoder's trailing newline.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// invalid is ErrInvalidEvent for field, with the reason.
func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidEvent, field, fmt.Sprintf(format, args...))
}

// check validates in against the record contract and returns the record it
// describes, without seq, id and time, and with its metadata encoded. It
// reads nothing, so an invalid input is refused before the store is touched.
func (in Input) check() (Record, error) {
	segments := strings.Split(in.Type, ".")
	switch {
	case segments[0] != "parley":
		return Record{}, invalid("type", "%q does not begin with parley.", in.Type)
	case len(segments) < 3:
		return Record{}, invalid("type", "%q has fewer than three segments", in.Type)
	}
	for _, s := range segments {
		if s == "" {
			return Record{}, invalid("type", "%q has an empty segment", in.Type)
		}
	}
	if in.Workspace != nil && *in.Workspace == "" {
		return Record{}, invalid("workspace", "is an empty string")
	}
	if in.ResourceType == "" {
		return Record{}, invalid("resource_type", "is empty")
	}
	if in.ResourceID == "" {
		return Record{}, invalid("resource_id", "is empty")
	}
	switch in.ActorType {
	case ActorAPIKey, ActorPAT:
		if in.ActorID == nil || *in.ActorID == "" {
			return Record{}, invalid("actor_id", "is required for actor_type %s", in.ActorType)
		}
	case ActorAdminToken, ActorForge, ActorSystem:
		if in.ActorID != nil {
			return Record{}, invalid("actor_id", "must be absent for actor_type %s", in.ActorType)
		}
	default:
		return Record{}, invalid("actor_type", "%q is not one of admin_token, api_key, pat, forge, system", in.ActorType)
	}
	var session *string
	if in.SessionID != nil {
		if !isUUID(*in.SessionID) {
			return Record{}, invalid("session_id", "%q is not a UUID", *in.SessionID)
		}
		s := strings.ToLower(*in.SessionID)
		session = &s
	}
	meta, err := encodeMetadata(in.Metadata)
	if err != nil {
		return Record{}, err
	}
	return Record{
		Type: in.Type, Workspace: in.Workspace, ActorType: in.ActorType, ActorID: in.ActorID,
		ResourceType: in.ResourceType, ResourceID: in.ResourceID, Action: segments[len(segments)-1],
		SessionID: session, Metadata: meta, parent: in.Parent,
	}, nil
}

// encodeMetadata encodes the metadata of an input as a compact JSON object;
// none, and JSON null, is {}.
func encodeMetadata(m any) (json.RawMessage, error) {
	var raw []byte
	switch v := m.(type) {
	case nil:
		return json.RawMessage(`{}`), nil
	case json.RawMessage:
		raw = v
	case []byte:
		raw = v
	default:
		b, err := encode(v)
		if err != nil {
			return nil, invalid("metadata", "cannot be encoded: %v", err)
		}
		raw = b
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil, invalid("metadata", "is not valid JSON")
	}
	switch {
	case b.String() == "null":
		return json.RawMessage(`{}`), nil
	case b.Len() == 0 || b.Bytes()[0] != '{':
		return nil, invalid("metadata", "is not a JSON object")
	}
	return json.RawMessage(b.Bytes()), nil
}

// isUUID reports whether s is a UUID in the 8-4-4-4-12 form, in either case.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// maxPaths is the most paths a metadata list built with PathList holds.
const maxPaths = 100

// PathList stores the first 100 of paths, in their order, in meta under key,
// and sets meta["truncated"] to true when it had to cut the list; with 100
// paths or fewer it stores them all and adds no truncated member. meta must
// not be nil. The component cannot tell which metadata member is a list of
// paths, so an emitting package builds its path lists with PathList; other
// lists are stored whole.
func PathList(meta map[string]any, key string, paths []string) {
	kept := make([]string, min(len(paths), maxPaths))
	copy(kept, paths)
	meta[key] = kept
	if len(paths) > maxPaths {
		meta["truncated"] = true
	}
}

// scrubMetadata returns the metadata object raw, valid compact JSON, with
// every string value at any depth, in objects and in arrays, passed through
// s. Member names, numbers, booleans and null are written back as they were,
// in their order; a nil s returns raw unchanged.
func scrubMetadata(raw json.RawMessage, s *scrub.Scrubber) (json.RawMessage, error) {
	if s == nil {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out bytes.Buffer
	if err := scrubValue(dec, &out, s); err != nil {
		return nil, fmt.Errorf("scrub the metadata: %w", err)
	}
	return json.RawMessage(out.Bytes()), nil
}

// scrubValue copies the next JSON value of dec to out, scrubbing its strings.
func scrubValue(dec *json.Decoder, out *bytes.Buffer, s *scrub.Scrubber) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		out.WriteRune(rune(v))
		for i := 0; dec.More(); i++ {
			if i > 0 {
				out.WriteByte(',')
			}
			if v == '{' {
				name, err := dec.Token()
				if err != nil {
					return err
				}
				if err := writeJSON(out, name); err != nil {
					return err
				}
				out.WriteByte(':')
			}
			if err := scrubValue(dec, out, s); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		out.WriteRune(rune(end.(json.Delim)))
		return nil
	case string:
		return writeJSON(out, s.Scrub(v))
	case json.Number:
		out.WriteString(v.String())
		return nil
	default: // a bool or nil
		return writeJSON(out, v)
	}
}

// writeJSON appends v to out as encode writes it.
func writeJSON(out *bytes.Buffer, v any) error {
	b, err := encode(v)
	if err != nil {
		return err
	}
	out.Write(b)
	return nil
}
