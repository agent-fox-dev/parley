package event

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
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

// jsonValue decodes data with its numbers kept as written, so that two
// documents compare equal exactly when they hold the same values.
func jsonValue(t *testing.T, data []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	return v
}

func TestRecord_EveryResourceFamilyPassesThrough(t *testing.T) {
	t.Parallel()
	const session = "0f8fad5b-d9cb-469f-a165-70867728950e"
	pat := func(typ, rtype, rid string, meta string) Input {
		return Input{Type: typ, Workspace: ptr("api"), ActorType: ActorPAT, ActorID: ptr("u_1"),
			ResourceType: rtype, ResourceID: rid, Metadata: json.RawMessage(meta)}
	}
	system := func(typ, rtype, rid string, meta string) Input {
		in := pat(typ, rtype, rid, meta)
		in.ActorType, in.ActorID = ActorSystem, nil
		return in
	}
	forge := func(typ, rtype, rid string, meta string) Input {
		in := pat(typ, rtype, rid, meta)
		in.ActorType, in.ActorID = ActorForge, nil
		return in
	}
	withSession := func(in Input) Input { in.SessionID = ptr(session); return in }
	noWorkspace := func(in Input) Input { in.Workspace = nil; return in }
	for _, in := range []Input{
		pat("parley.workspace.create", "workspace", "api", `{"default_branch":"main","kind":"repository"}`),
		pat("parley.workspace.push", "branch", "main", `{"old":"`+strings.Repeat("a", 40)+`","new":"`+strings.Repeat("b", 40)+`","forced":false}`),
		system("parley.session.force_closed", "session", session, `{"reason":"expired","tokens":{"input":12345678901234567890,"cost":0.0125}}`),
		pat("parley.token.bind", "token", "tok_1", `{"workspace":"api","scopes":["code:read","audit:read"]}`),
		withSession(pat("parley.git.push", "ref", "refs/heads/main", `{"commits":3,"rejected":null}`)),
		pat("parley.scm.commit", "commit", strings.Repeat("c", 40), `{"message":"fix: <escape> & \"quote\"","files":["a.go","b/c.go"]}`),
		pat("parley.change.land", "change", "42", `{"target":"main","version":2}`),
		pat("parley.policy.update", "policy", "api", `{"rules":[{"path":"internal/**","reviewers":2}]}`),
		pat("parley.claim.create", "claim", "c1", `{"paths":["docs/"],"ttl":"4h"}`),
		pat("parley.patch.create", "patch", "p1", `{"change":42,"empty":{}}`),
		system("parley.rebuild.complete", "rebuild", "j1", `{"status":"ok","duration_ms":1500}`),
		noWorkspace(pat("parley.secret.create", "secret", "KEY", `{"scope":"user","scope_id":"u_1"}`)),
		pat("parley.variable.update", "variable", "KEY", `{"scope":"workspace"}`),
		forge("parley.forge.issue.opened", "issue", "7", `{"title":"Crash on start","labels":["bug"]}`),
		forge("parley.forge.comment.create", "comment", "7", `{"issue":7,"body":"ünïcode ✓"}`),
		pat("parley.forge.label.create", "label", "bug", `{"color":"d73a4a"}`),
		pat("parley.forge.pull_request.create", "pull_request", "9", `{"head":"feature/x","draft":true}`),
		pat("parley.forge.webhook.set", "webhook", "api", `{"events":["push"]}`),
		withSession(pat("parley.sandbox.create", "sandbox", "s1", `{"profile":"agent"}`)),
		pat("parley.sandbox.rule_add", "sandbox_rule", "r1", `{"host":"proxy.golang.org","port":443}`),
		pat("parley.webhook.create", "event_webhook", "w1", `{"url":"https://hooks.example.com/parley","types":["parley.change.*"]}`),
	} {
		t.Run(in.ResourceType, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.emit(in)
			read := f.readAll(0)
			if len(read) != 1 {
				t.Fatalf("ReadAfter = %d records, want 1", len(read))
			}
			got := read[0]
			want := in
			got.parent, want.Parent = "", ""
			if got.Type != want.Type || !reflect.DeepEqual(got.Workspace, want.Workspace) ||
				got.ActorType != want.ActorType || !reflect.DeepEqual(got.ActorID, want.ActorID) ||
				got.ResourceType != want.ResourceType || got.ResourceID != want.ResourceID ||
				!reflect.DeepEqual(got.SessionID, want.SessionID) {
				t.Errorf("read back %+v, want the fields of %+v", got, want)
			}
			if g, w := jsonValue(t, got.Metadata), jsonValue(t, want.Metadata.(json.RawMessage)); !reflect.DeepEqual(g, w) {
				t.Errorf("metadata = %s, want %s", got.Metadata, want.Metadata)
			}
			segments := strings.Split(in.Type, ".")
			if last := segments[len(segments)-1]; got.Action != last {
				t.Errorf("action = %q, want %q", got.Action, last)
			}
		})
	}
}

func TestPathList_CutsAtOneHundred(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 100, 101, 250} {
		t.Run(strconv.Itoa(n)+" paths", func(t *testing.T) {
			t.Parallel()
			paths := make([]string, n)
			for i := range paths {
				paths[i] = fmt.Sprintf("dir/file%03d.go", i)
			}
			meta := map[string]any{"commit": "abc"}
			PathList(meta, "files", paths)
			in := valid("parley.workspace.push")
			in.Metadata = meta
			f := newFixture(t)
			f.emit(in)

			var stored struct {
				Files     *[]string `json:"files"`
				Truncated *bool     `json:"truncated"`
				Commit    string    `json:"commit"`
			}
			if err := json.Unmarshal(f.readAll(0)[0].Metadata, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Files == nil {
				t.Fatalf("metadata has no files member")
			}
			if want := paths[:min(n, 100)]; !slices.Equal(*stored.Files, want) {
				t.Errorf("files = %d entries %v, want the first %d", len(*stored.Files), *stored.Files, len(want))
			}
			switch {
			case n > 100 && (stored.Truncated == nil || !*stored.Truncated):
				t.Errorf("truncated = %v, want true", stored.Truncated)
			case n <= 100 && stored.Truncated != nil:
				t.Errorf("truncated = %v, want no truncated member", *stored.Truncated)
			}
			if stored.Commit != "abc" {
				t.Errorf("commit = %q, want the other members kept", stored.Commit)
			}
		})
	}
}

func TestEmit_ScrubsMetadataStrings(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.scrubber.Add("s3cr3t-value")
	in := valid("parley.workspace.create")
	in.Metadata = json.RawMessage(`{"git_url":"https://bob:pw@example.com/r.git",` +
		`"nested":{"note":"token s3cr3t-value here","s3cr3t-value":"member names stay"},` +
		`"list":["https://x:y@h/p",{"deep":["s3cr3t-value"]}],` +
		`"count":12345678901234567890,"ratio":0.5,"ok":true,"none":null,"long":"` + strings.Repeat("x", 1100) + `"}`)
	emitted := f.emit(in)
	stored := f.readAll(0)[0]
	if !bytes.Equal(emitted.Metadata, stored.Metadata) {
		t.Errorf("Emit returned metadata %s, the log holds %s", emitted.Metadata, stored.Metadata)
	}

	var got struct {
		GitURL string `json:"git_url"`
		Nested map[string]string
		List   []json.RawMessage
		Count  json.Number
		Ratio  json.Number
		OK     *bool
		None   json.RawMessage
		Long   string
	}
	dec := json.NewDecoder(bytes.NewReader(stored.Metadata))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.GitURL != "https://example.com/r.git" {
		t.Errorf("git_url = %q, want https://example.com/r.git", got.GitURL)
	}
	if note := got.Nested["note"]; !strings.Contains(note, "[redacted]") || strings.Contains(note, "s3cr3t-value") {
		t.Errorf("nested.note = %q, want the secret replaced by [redacted]", note)
	}
	if got.Nested["s3cr3t-value"] != "member names stay" {
		t.Errorf("nested = %v, want the member name kept", got.Nested)
	}
	if len(got.List) != 2 || string(got.List[0]) != `"https://h/p"` || string(got.List[1]) != `{"deep":["[redacted]"]}` {
		t.Errorf("list = %s, want [\"https://h/p\",{\"deep\":[\"[redacted]\"]}]", got.List)
	}
	if got.Count != "12345678901234567890" || got.Ratio != "0.5" || got.OK == nil || !*got.OK || string(got.None) != "null" {
		t.Errorf("count, ratio, ok, none = %s, %s, %v, %s, want them unchanged", got.Count, got.Ratio, got.OK, got.None)
	}
	if len(got.Long) != 1024 {
		t.Errorf("long = %d bytes, want it cut to 1024", len(got.Long))
	}
}
