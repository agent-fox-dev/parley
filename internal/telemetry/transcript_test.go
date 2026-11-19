package telemetry_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// transcriptPage is the answer of the transcript route.
type transcriptPage struct {
	RunID      string           `json:"run_id"`
	NodeID     string           `json:"node_id"`
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// TestTranscript reads transcripts on one hub, each subtest on runs of its
// own in demo.
func TestTranscript(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// trace stores a trace record of run and node in demo; a nil sequence or
	// data is left out of the body.
	trace := func(t *testing.T, run, node, eventType, timestamp string, sequence, data any) {
		t.Helper()
		body := map[string]any{"id": w.freshID(), "timestamp": timestamp, "node_id": node, "event_type": eventType}
		if sequence != nil {
			body["sequence"] = sequence
		}
		if data != nil {
			body["data"] = data
		}
		w.seed(t, "/traces", demo, run, body)
	}
	get := func(t *testing.T, route, run string, query url.Values) (int, []byte) {
		t.Helper()
		return w.list(t, w.aliceKey, demo, run, route, query)
	}
	transcript := func(t *testing.T, run string, query url.Values) transcriptPage {
		t.Helper()
		status, raw := get(t, "/transcript", run, query)
		if status != http.StatusOK {
			t.Fatalf("GET /transcript %v = %d %s, want 200", query, status, raw)
		}
		var p transcriptPage
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return p
	}
	item := func(role, content string, toolName any, timestamp string) map[string]any {
		return map[string]any{"role": role, "content": content, "tool_name": toolName, "timestamp": timestamp}
	}

	t.Run("a node's traces become items in order, without session.result", func(t *testing.T) {
		run := w.freshRun()
		trace(t, run, "n1", "session.init", t1, nil, map[string]any{"content": "hi"})
		trace(t, run, "n1", "assistant.message", t2, nil, map[string]any{"content": map[string]any{"a": 1}, "tool_name": "bash"})
		// Stored in this order, so that their ids sort against their sequences.
		trace(t, run, "n1", "tool.error", t3, 2, nil)
		trace(t, run, "n1", "tool.use", t3, 1, map[string]any{"tool_name": 7})
		trace(t, run, "n1", "assistant.message", t4, nil, map[string]any{"content": "done"})
		trace(t, run, "n1", "session.result", t4, 5, map[string]any{"content": "result"})
		trace(t, run, "n2", "assistant.message", t1, nil, map[string]any{"content": "other node"})

		p := transcript(t, run, url.Values{"node_id": {"n1"}})
		want := []map[string]any{
			item("system", "hi", nil, t1),
			item("assistant", "", "bash", t2),
			item("tool_use", "", nil, t3),
			item("tool_error", "", nil, t3),
			item("assistant", "done", nil, t4),
		}
		if p.RunID != run || p.NodeID != "n1" || p.NextCursor != nil {
			t.Errorf("run_id %q, node_id %q, next_cursor %v, want %s, n1 and null", p.RunID, p.NodeID, p.NextCursor, run)
		}
		if !reflect.DeepEqual(p.Items, want) {
			t.Errorf("items %v, want %v", p.Items, want)
		}
	})

	t.Run("pages join into the whole transcript and an unknown run or node is empty", func(t *testing.T) {
		run := w.freshRun()
		trace(t, run, "n1", "session.init", t1, nil, map[string]any{"content": "p1"})
		trace(t, run, "n1", "assistant.message", t2, nil, map[string]any{"content": "p2"})
		trace(t, run, "n1", "session.result", t3, nil, map[string]any{"content": "skipped"})
		// Sharing a timestamp: an absent sequence counts as 0, so the trace
		// without one comes between sequence 0 of a lower id and sequence 2.
		trace(t, run, "n1", "tool.error", t4, 2, map[string]any{"content": "p5"})
		trace(t, run, "n1", "tool.use", t4, 0, map[string]any{"content": "p3"})
		trace(t, run, "n1", "assistant.message", t4, nil, map[string]any{"content": "p4"})
		trace(t, run, "n1", "session.result", t4, 9, map[string]any{"content": "skipped"})

		all := transcript(t, run, url.Values{"node_id": {"n1"}}).Items
		var contents []string
		for _, it := range all {
			contents = append(contents, it["content"].(string))
		}
		if got := strings.Join(contents, " "); got != "p1 p2 p3 p4 p5" {
			t.Fatalf("contents %q, want p1 p2 p3 p4 p5", got)
		}
		// The second page of 2 starts on a session.result, and the last trace
		// is one: every page but the last is full, and none is empty.
		for _, limit := range []int{1, 2, 3, 5} {
			var joined []map[string]any
			q := url.Values{"node_id": {"n1"}, "limit": {strconv.Itoa(limit)}}
			for pages := 1; ; pages++ {
				p := transcript(t, run, q)
				if p.NextCursor != nil && len(p.Items) != limit || len(p.Items) == 0 {
					t.Errorf("limit %d: page %d holds %d items, next_cursor %v", limit, pages, len(p.Items), p.NextCursor)
				}
				joined = append(joined, p.Items...)
				if p.NextCursor == nil || pages > len(all) {
					break
				}
				q.Set("cursor", *p.NextCursor)
			}
			if !reflect.DeepEqual(joined, all) {
				t.Errorf("limit %d: pages joined %v, want %v", limit, joined, all)
			}
		}

		for _, c := range []struct{ run, node string }{{w.freshRun(), "n1"}, {run, "ghost"}} {
			status, raw := get(t, "/transcript", c.run, url.Values{"node_id": {c.node}})
			got := decode(t, raw)
			items, isList := got["items"].([]any)
			if cursor, ok := got["next_cursor"]; status != http.StatusOK || !isList || len(items) != 0 || !ok || cursor != nil {
				t.Errorf("transcript of run %s, node %s = %d %s, want 200 with no items and next_cursor null", c.run, c.node, status, raw)
			}
		}
	})

	t.Run("an invalid parameter is refused, naming it", func(t *testing.T) {
		run := w.freshRun()
		for range 2 {
			trace(t, run, "n2", "session.init", t1, nil, nil)
		}
		p := transcript(t, run, url.Values{"node_id": {"n2"}, "limit": {"1"}})
		if p.NextCursor == nil {
			t.Fatal("the transcript of node n2 by 1 gave no next_cursor")
		}
		cases := []struct {
			route, run string
			query      url.Values
			name       string
		}{
			{"/transcript", run, nil, "node_id"},
			{"/transcript", run, url.Values{"node_id": {""}}, "node_id"},
			{"/transcript", "x", url.Values{"node_id": {"n1"}}, "run_id"},
			{"/postmortem", "20260601-120000-abcdef", nil, "run_id"},
			{"/transcript", run, url.Values{"node_id": {"n1"}, "limit": {"0"}}, "limit"},
			{"/transcript", run, url.Values{"node_id": {"n1"}, "limit": {"201"}}, "limit"},
			{"/transcript", run, url.Values{"node_id": {"n1"}, "cursor": {"%%%"}}, "cursor"},
			{"/transcript", run, url.Values{"node_id": {"n1"}, "cursor": {*p.NextCursor}}, "cursor"},
		}
		for _, c := range cases {
			status, raw := get(t, c.route, c.run, c.query)
			typ, msg := errorOf(t, raw)
			if status != http.StatusBadRequest || typ != "invalid_request" || !strings.Contains(msg, c.name) {
				t.Errorf("%s of run %s with %v = %d %s, want 400 invalid_request naming %s", c.route, c.run, c.query, status, raw, c.name)
			}
		}
	})
}
