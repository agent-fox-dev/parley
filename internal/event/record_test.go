package event

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// recordMembers are the members of an event's JSON form, in their order.
var recordMembers = []string{"seq", "id", "type", "time", "workspace", "actor_type", "actor_id",
	"resource_type", "resource_id", "action", "session_id", "metadata"}

// memberOrder decodes a JSON object and returns its member names in order
// and its values.
func memberOrder(t *testing.T, data []byte) ([]string, map[string]json.RawMessage) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("%s: not a JSON object", data)
	}
	var names []string
	values := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		name := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		values[name] = v
	}
	return names, values
}

func TestRecord_JSONForm(t *testing.T) {
	t.Parallel()
	rfc3339 := regexp.MustCompile(`^"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z"$`)
	for _, tc := range []struct {
		name     string
		in       Input
		null     []string
		metadata string
	}{
		{
			name: "every field set, session_id in upper case",
			in: Input{Type: "parley.sandbox.create", Workspace: ptr("api"), ActorType: "pat", ActorID: ptr("u_1"),
				ResourceType: "sandbox", ResourceID: "s1", SessionID: ptr("0F8FAD5B-D9CB-469F-A165-70867728950E"),
				Metadata: map[string]any{"profile": "agent", "ttl": "4h"}},
			metadata: `{"profile":"agent","ttl":"4h"}`,
		},
		{
			name:     "workspace, actor_id and session_id absent and no metadata",
			in:       Input{Type: "parley.rebuild.complete", ActorType: "system", ResourceType: "rebuild", ResourceID: "j1"},
			null:     []string{"workspace", "actor_id", "session_id"},
			metadata: `{}`,
		},
		{
			name: "nested metadata object",
			in: Input{Type: "parley.change.land", Workspace: ptr("api"), ActorType: "api_key", ActorID: ptr("u_2"),
				ResourceType: "change", ResourceID: "42", Metadata: json.RawMessage(`{"target": {"branch": "main", "commits": [1, 2]}}`)},
			null:     []string{"session_id"},
			metadata: `{"target":{"branch":"main","commits":[1,2]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			emitted := f.emit(tc.in)
			read := f.readAll(emitted.Seq - 1)
			if len(read) != 1 {
				t.Fatalf("ReadAfter = %d records, want 1", len(read))
			}
			data, err := json.Marshal(read[0])
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if again, _ := json.Marshal(emitted); !bytes.Equal(again, data) {
				t.Errorf("the emitted record marshals to %s, the record read back to %s", again, data)
			}
			if bytes.ContainsAny(data, "\r\n") {
				t.Errorf("%s: holds a line break", data)
			}

			names, values := memberOrder(t, data)
			if !reflect.DeepEqual(names, recordMembers) {
				t.Errorf("members = %v, want %v", names, recordMembers)
			}
			for _, name := range tc.null {
				if string(values[name]) != "null" {
					t.Errorf("%s = %s, want null", name, values[name])
				}
			}
			if got := string(values["metadata"]); got != tc.metadata {
				t.Errorf("metadata = %s, want %s", got, tc.metadata)
			}
			if !rfc3339.Match(values["time"]) {
				t.Errorf("time = %s, want RFC 3339 UTC at whole seconds", values["time"])
			}
			if s := string(values["session_id"]); s != strings.ToLower(s) {
				t.Errorf("session_id = %s, want lower case", s)
			}
		})
	}
}

func TestEmit_RejectsInvalidInput(t *testing.T) {
	t.Parallel()
	with := func(change func(*Input)) Input {
		in := valid("parley.change.land")
		change(&in)
		return in
	}
	for _, tc := range []struct {
		name  string
		in    Input
		field string
	}{
		{"type without the parley prefix", with(func(in *Input) { in.Type = "change.land" }), "type"},
		{"type of two segments", with(func(in *Input) { in.Type = "parley.land" }), "type"},
		{"type with an empty segment", with(func(in *Input) { in.Type = "parley..land" }), "type"},
		{"type with an empty last segment", with(func(in *Input) { in.Type = "parley.change." }), "type"},
		{"empty resource_type", with(func(in *Input) { in.ResourceType = "" }), "resource_type"},
		{"empty resource_id", with(func(in *Input) { in.ResourceID = "" }), "resource_id"},
		{"unknown actor_type", with(func(in *Input) { in.ActorType = "user" }), "actor_type"},
		{"actor_id for the system", with(func(in *Input) { in.ActorType = "system" }), "actor_id"},
		{"empty actor_id for a PAT", with(func(in *Input) { in.ActorID = ptr("") }), "actor_id"},
		{"session_id that is not a UUID", with(func(in *Input) { in.SessionID = ptr("abc") }), "session_id"},
		{"empty workspace", with(func(in *Input) { in.Workspace = ptr("") }), "workspace"},
		{"metadata that is a JSON array", with(func(in *Input) { in.Metadata = json.RawMessage(`["a"]`) }), "metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()
			err := f.store.Tx(ctx, func(tx *sql.Tx) error {
				_, err := f.events.Emit(ctx, tx, tc.in)
				return err
			})
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("Emit = %v, want ErrInvalidEvent", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name the field %s", err, tc.field)
			}
			if got := f.lastSeq(); got != 0 {
				t.Errorf("last_seq = %d, want 0", got)
			}
		})
	}
}
