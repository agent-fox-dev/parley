package telemetry_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestRecords runs the behaviours of the session outcome, tool call, tool
// error and trace routes on one hub, alice posting to demo, each request on
// a run of its own.
func TestRecords(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const s1 = "0a1b2c3d-0000-4000-8000-0000000000aa"
	// created posts body to the route path of a new run of demo as alice
	// and returns the record the hub answered 201 with.
	created := func(t *testing.T, path string, body any) map[string]any {
		t.Helper()
		status, raw := w.post(t, w.aliceKey, demo, w.freshRun(), path, body)
		if status != http.StatusCreated {
			t.Fatalf("POST %s %v = %d %s, want 201", path, body, status, raw)
		}
		return decode(t, raw)
	}
	// hasMembers checks that the record got holds exactly the common
	// members and those of its kind.
	hasMembers := func(t *testing.T, got map[string]any, kind ...string) {
		t.Helper()
		want := append([]string{"id", "workspace", "run_id", "timestamp", "ingested_at"}, kind...)
		for _, m := range want {
			if _, ok := got[m]; !ok {
				t.Errorf("answer %v lacks the member %s", got, m)
			}
		}
		if len(got) != len(want) {
			t.Errorf("answer %v has %d members, want %d: %v", got, len(got), len(want), want)
		}
	}
	// storedJSON is the text a JSON member of the record id is stored as.
	storedJSON := func(t *testing.T, table, column, id string) string {
		t.Helper()
		var s string
		if err := w.Store.Analytics().QueryRowContext(t.Context(), `SELECT `+column+` FROM `+table+` WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatalf("%s.%s of %s: %v", table, column, id, err)
		}
		return s
	}

	t.Run("a session outcome is stored without looking up its session, its token usage as sent", func(t *testing.T) {
		got := created(t, "/sessions/outcomes", map[string]any{
			"session_id": s1, "status": "completed", "node_id": "n1", "duration_ms": 0,
			"token_usage": map[string]any{"input": 10, "by_model": []int{1, 2}},
		})
		hasMembers(t, got, "session_id", "status", "node_id", "duration_ms", "token_usage")
		usage, _ := got["token_usage"].(map[string]any)
		byModel, _ := usage["by_model"].([]any)
		if got["status"] != "completed" || got["duration_ms"] != 0.0 || len(byModel) != 2 || byModel[1] != 2.0 {
			t.Errorf("answer = %v, want status completed, duration_ms 0 and token_usage.by_model [1 2]", got)
		}
		if got["session_id"] != s1 || got["node_id"] != "n1" || got["workspace"] != demo {
			t.Errorf("answer = %v, want session_id %s, node_id n1 and workspace demo", got, s1)
		}
		if s := storedJSON(t, "session_outcomes", "token_usage", got["id"].(string)); s != `{"by_model":[1,2],"input":10}` {
			t.Errorf("stored token_usage = %s, want the JSON object sent", s)
		}

		got = created(t, "/sessions/outcomes", map[string]any{"session_id": s1, "status": "anything-goes"})
		hasMembers(t, got, "session_id", "status", "node_id", "duration_ms", "token_usage")
		if got["duration_ms"] != nil || got["token_usage"] != nil || got["node_id"] != nil {
			t.Errorf("answer = %v, want duration_ms, token_usage and node_id null", got)
		}
		for _, usage := range []any{[]any{1.0, "x"}, 42.5, "many", true} {
			got := created(t, "/sessions/outcomes", map[string]any{"session_id": s1, "status": "ok", "token_usage": usage})
			if !reflect.DeepEqual(got["token_usage"], usage) {
				t.Errorf("token_usage %#v answered as %#v", usage, got["token_usage"])
			}
		}
	})

	t.Run("a tool call is stored with its input and output as sent", func(t *testing.T) {
		got := created(t, "/tools/calls", map[string]any{
			"tool_name": "bash", "duration_ms": 12, "input": map[string]any{"cmd": []string{"ls", "-l"}}, "output": "text",
		})
		hasMembers(t, got, "tool_name", "node_id", "session_id", "duration_ms", "input", "output")
		input, _ := got["input"].(map[string]any)
		cmd, _ := input["cmd"].([]any)
		if len(cmd) != 2 || cmd[1] != "-l" || got["output"] != "text" || got["duration_ms"] != 12.0 {
			t.Errorf("answer = %v, want input.cmd [ls -l], output text and duration_ms 12", got)
		}
		if got["session_id"] != nil || got["node_id"] != nil {
			t.Errorf("answer = %v, want session_id and node_id null", got)
		}
		id := got["id"].(string)
		if in, out := storedJSON(t, "tool_calls", "input", id), storedJSON(t, "tool_calls", "output", id); in != `{"cmd":["ls","-l"]}` || out != `"text"` {
			t.Errorf("stored input %s and output %s, want the JSON values sent", in, out)
		}
		got = created(t, "/tools/calls", map[string]any{"tool_name": strings.Repeat("t", 255), "session_id": strings.ToUpper(s1)})
		if got["input"] != nil || got["output"] != nil || got["session_id"] != s1 {
			t.Errorf("answer = %v, want input and output null and session_id in lower case", got)
		}
	})

	t.Run("a tool error is stored with its message and code", func(t *testing.T) {
		got := created(t, "/tools/errors", map[string]any{"tool_name": "bash", "error_msg": "exit 1", "error_code": "E_EXIT"})
		hasMembers(t, got, "tool_name", "error_msg", "error_code", "node_id", "session_id")
		if got["tool_name"] != "bash" || got["error_msg"] != "exit 1" || got["error_code"] != "E_EXIT" {
			t.Errorf("answer = %v, want tool_name bash, error_msg 'exit 1' and error_code E_EXIT", got)
		}
		if got := created(t, "/tools/errors", map[string]any{"tool_name": "bash", "error_msg": "x"}); got["error_code"] != nil {
			t.Errorf("error_code = %v, want null", got["error_code"])
		}
	})

	t.Run("a trace of each event type is stored with its sequence and data as sent", func(t *testing.T) {
		for _, typ := range []string{"session.init", "assistant.message", "tool.use", "tool.error", "session.result"} {
			got := created(t, "/traces", map[string]any{
				"event_type": typ, "node_id": "n1", "sequence": 0, "data": map[string]any{"content": "hello", "tool_name": "bash"},
			})
			hasMembers(t, got, "event_type", "node_id", "session_id", "sequence", "data")
			data, _ := got["data"].(map[string]any)
			if got["event_type"] != typ || got["sequence"] != 0.0 || data["content"] != "hello" {
				t.Errorf("answer = %v, want event_type %s, sequence 0 and data.content hello", got, typ)
			}
			if s := storedJSON(t, "traces", "data", got["id"].(string)); s != `{"content":"hello","tool_name":"bash"}` {
				t.Errorf("stored data = %s, want the JSON object sent", s)
			}
		}
		got := created(t, "/traces", map[string]any{"event_type": "tool.use"})
		if got["sequence"] != nil || got["data"] != nil {
			t.Errorf("answer = %v, want sequence and data null", got)
		}
	})

	t.Run("a body that breaks a member's rule is refused naming the member", func(t *testing.T) {
		long := strings.Repeat("t", 256)
		cases := []struct {
			path, table string
			body        map[string]any
			member      string
		}{
			{"/sessions/outcomes", "session_outcomes", map[string]any{"status": "ok"}, "session_id"},
			{"/sessions/outcomes", "session_outcomes", map[string]any{"session_id": s1}, "status"},
			{"/sessions/outcomes", "session_outcomes", map[string]any{"session_id": "abc", "status": "ok"}, "session_id"},
			{"/sessions/outcomes", "session_outcomes", map[string]any{"session_id": s1, "status": ""}, "status"},
			{"/sessions/outcomes", "session_outcomes", map[string]any{"session_id": s1, "status": "ok", "duration_ms": -1}, "duration_ms"},
			{"/sessions/outcomes", "session_outcomes", map[string]any{"session_id": s1, "status": "ok", "duration_ms": 1.5}, "duration_ms"},
			{"/sessions/outcomes", "session_outcomes", map[string]any{"session_id": s1, "status": "ok", "duration_ms": "3"}, "duration_ms"},
			{"/tools/calls", "tool_calls", map[string]any{}, "tool_name"},
			{"/tools/calls", "tool_calls", map[string]any{"tool_name": long}, "tool_name"},
			{"/tools/calls", "tool_calls", map[string]any{"tool_name": "bash", "duration_ms": -5}, "duration_ms"},
			{"/tools/calls", "tool_calls", map[string]any{"tool_name": "bash", "session_id": "zz"}, "session_id"},
			{"/tools/errors", "tool_errors", map[string]any{"tool_name": "bash"}, "error_msg"},
			{"/tools/errors", "tool_errors", map[string]any{"error_msg": "x"}, "tool_name"},
			{"/tools/errors", "tool_errors", map[string]any{"tool_name": "bash", "error_msg": ""}, "error_msg"},
			{"/tools/errors", "tool_errors", map[string]any{"tool_name": "bash", "error_msg": "x", "error_code": 5}, "error_code"},
			{"/traces", "traces", map[string]any{}, "event_type"},
			{"/traces", "traces", map[string]any{"event_type": "user.message"}, "event_type"},
			{"/traces", "traces", map[string]any{"event_type": "tool.use", "sequence": -1}, "sequence"},
			{"/traces", "traces", map[string]any{"event_type": "tool.use", "sequence": 2.5}, "sequence"},
			{"/traces", "traces", map[string]any{"event_type": "tool.use", "timestamp": "yesterday"}, "timestamp"},
			{"/traces", "traces", map[string]any{"event_type": "tool.use", "id": "x"}, "id"},
		}
		for _, c := range cases {
			before := w.count(t, `SELECT COUNT(*) FROM `+c.table)
			status, raw := w.post(t, w.aliceKey, demo, w.freshRun(), c.path, c.body)
			typ, msg := errorOf(t, raw)
			if status != http.StatusBadRequest || typ != "invalid_request" || !strings.Contains(msg, c.member) {
				t.Errorf("POST %s %v = %d %s, want 400 invalid_request naming %q", c.path, c.body, status, raw, c.member)
			}
			if n := w.count(t, `SELECT COUNT(*) FROM `+c.table); n != before {
				t.Errorf("POST %s %v: %s holds %d records, want %d", c.path, c.body, c.table, n, before)
			}
		}
	})
}
