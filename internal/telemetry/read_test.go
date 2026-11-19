package telemetry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// listRoutes are the per-run list routes, after
// /workspaces/:slug/runs/:run_id.
var listRoutes = []string{"/events", "/sessions/outcomes", "/tools/calls", "/tools/errors", "/traces"}

// readRoutes are every per-run read route.
var readRoutes = append(slices.Clone(listRoutes), "/postmortem", "/transcript")

// Three instants of the records the lists are read from, a second apart.
const (
	t1 = "2026-06-01T12:00:00Z"
	t2 = "2026-06-01T12:00:01Z"
	t3 = "2026-06-01T12:00:02Z"
	t4 = "2026-06-01T12:00:03Z"
)

// Two sessions of the records, in lower case.
const (
	session1 = "0a1b2c3d-0000-4000-8000-0000000000c1"
	session2 = "0a1b2c3d-0000-4000-8000-0000000000b2"
)

// fullRecord is a body for the single-record route of the list route that
// sets every member of its record: id, timestamp, and each label of
// labels (node_id, session_id, and the route's own: event_type, severity,
// status or tool_name) over the defaults.
func fullRecord(route, id, timestamp string, labels map[string]string) map[string]any {
	body := map[string]any{"id": id, "timestamp": timestamp, "node_id": "n1", "session_id": "0a1b2c3d-0000-4000-8000-0000000000b1"}
	switch route {
	case "/events":
		body["event_type"], body["severity"], body["payload"] = "run.start", "info", map[string]any{"step": 1, "ok": true}
	case "/sessions/outcomes":
		body["status"], body["duration_ms"], body["token_usage"] = "completed", 1200, map[string]any{"input": 3, "output": 4}
	case "/tools/calls":
		body["tool_name"], body["duration_ms"], body["input"], body["output"] = "bash", 15, map[string]any{"cmd": "ls"}, []any{"a", "b"}
	case "/tools/errors":
		body["tool_name"], body["error_msg"], body["error_code"] = "bash", "exit 1", "E1"
	case "/traces":
		body["event_type"], body["sequence"], body["data"] = "session.init", 7, map[string]any{"text": "hello"}
	}
	for k, v := range labels {
		body[k] = v
	}
	return body
}

// seed stores body as a record of the list route's kind under slug and
// run, and returns the stored record as a list item holds it.
func (w *world) seed(t *testing.T, route, slug, run string, body map[string]any) map[string]any {
	t.Helper()
	raw, err := w.telemetry.Seed(context.Background(), route, slug, run, body)
	if err != nil {
		t.Fatalf("seed %s %v: %v", route, body, err)
	}
	return decode(t, raw)
}

// listPage is the answer of a list route.
type listPage struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// list sends GET to the list route of slug and run with query as
// credential.
func (w *world) list(t *testing.T, credential, slug, run, route string, query url.Values) (int, []byte) {
	t.Helper()
	path := "/api/v1/workspaces/" + slug + "/runs/" + run + route
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return w.Call(t, http.MethodGet, path, credential, nil)
}

// page is list answered 200 and decoded.
func (w *world) page(t *testing.T, credential, slug, run, route string, query url.Values) listPage {
	t.Helper()
	status, raw := w.list(t, credential, slug, run, route, query)
	if status != http.StatusOK {
		t.Fatalf("GET %s %v = %d %s, want 200", route, query, status, raw)
	}
	var p listPage
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return p
}

// ids are the ids of items, in their order.
func ids(items []map[string]any) []string {
	out := []string{}
	for _, it := range items {
		id, _ := it["id"].(string)
		out = append(out, id)
	}
	return out
}

// asJSON is v as a JSON value decodes: what a member sent as v is answered
// as.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRunLists reads the per-run lists on one hub. The subtests run in
// order: the last ones count the store's reads.
func TestRunLists(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	auditRead := w.pat(t, w.alice, "audit:read")

	t.Run("every reader gets the run's records in order with every field, on a ready and an archived workspace", func(t *testing.T) { listsReadInOrder(t, w, auditRead) })
	t.Run("order=desc reverses the order and a cursor pages through every record once", func(t *testing.T) { listsPageBothWays(t, w) })
	t.Run("filters and the time range select the records that match exactly", func(t *testing.T) { listsFilter(t, w) })
	t.Run("an invalid parameter is refused, naming it, before any record is read", func(t *testing.T) { listsRefuseInvalidParameters(t, w) })
	t.Run("a caller without a credential, the scope or read access gets no telemetry", func(t *testing.T) { listsRefuseOutsiders(t, w) })
}

// listsReadInOrder reads, as every kind of reader, a run's records of each
// kind stored out of order beside those of another run and of another
// workspace, on demo and on the archived workspace.
func listsReadInOrder(t *testing.T, w *world, auditRead string) {
	readers := []struct{ name, credential string }{
		{"owner API key", w.aliceKey}, {"org member API key", w.bobKey}, {"PAT with audit:read", auditRead},
		{"PAT with only audit:write", w.alicePAT}, {"admin token", w.Token},
	}
	for _, slug := range []string{demo, old} {
		for _, route := range listRoutes {
			runA, runB := w.freshRun(), w.freshRun()
			first, second, third := w.freshID(), w.freshID(), w.freshID()
			// Stored out of order; two share a timestamp and differ in id.
			bodies := map[string]map[string]any{
				third:  fullRecord(route, third, t2, nil),
				first:  fullRecord(route, first, t1, nil),
				second: fullRecord(route, second, t2, map[string]string{"node_id": "n2"}),
			}
			stored := map[string]map[string]any{}
			for _, id := range []string{third, first, second} {
				stored[id] = w.seed(t, route, slug, runA, bodies[id])
			}
			otherRun := w.seed(t, route, slug, runB, fullRecord(route, w.freshID(), t1, nil))["id"]
			otherWorkspace := w.seed(t, route, other, runA, fullRecord(route, w.freshID(), t1, nil))["id"]

			for _, r := range readers {
				p := w.page(t, r.credential, slug, runA, route, nil)
				if got, want := ids(p.Items), []string{first, second, third}; !slices.Equal(got, want) {
					t.Errorf("%s on %s as %s: ids %v, want %v (not %v of another run, nor %v of another workspace)",
						route, slug, r.name, got, want, otherRun, otherWorkspace)
					continue
				}
				if p.NextCursor != nil {
					t.Errorf("%s on %s as %s: next_cursor %q, want null", route, slug, r.name, *p.NextCursor)
				}
				for _, item := range p.Items {
					id := item["id"].(string)
					if !reflect.DeepEqual(item, stored[id]) {
						t.Errorf("%s on %s as %s: item %v, want the stored record %v", route, slug, r.name, item, stored[id])
					}
					for member, sent := range bodies[id] {
						if member == "timestamp" {
							continue
						}
						if got := item[member]; !reflect.DeepEqual(got, asJSON(t, sent)) {
							t.Errorf("%s as %s: %s = %#v, want %#v as sent", route, r.name, member, got, sent)
						}
					}
				}
			}
			status, raw := w.list(t, w.aliceKey, slug, w.freshRun(), route, nil)
			if status != http.StatusOK || strings.TrimSpace(string(raw)) != `{"items":[],"next_cursor":null}` {
				t.Errorf("%s on %s for a run without records = %d %s, want 200 with no items", route, slug, status, raw)
			}
		}
	}
}

// listsPageBothWays reads five records of each kind in both orders, whole
// and two at a time.
func listsPageBothWays(t *testing.T, w *world) {
	for _, route := range listRoutes {
		run := w.freshRun()
		var want []string
		for _, ts := range []string{t1, t2, t3, t3, t4} {
			id := w.freshID()
			want = append(want, id)
			w.seed(t, route, demo, run, fullRecord(route, id, ts, nil))
		}
		for _, order := range []string{"asc", "desc"} {
			all := ids(w.page(t, w.aliceKey, demo, run, route, url.Values{"order": {order}}).Items)
			expected := slices.Clone(want)
			if order == "desc" {
				slices.Reverse(expected)
			}
			if !slices.Equal(all, expected) {
				t.Errorf("%s order=%s: %v, want %v", route, order, all, expected)
			}
			var paged []string
			q := url.Values{"order": {order}, "limit": {"2"}}
			pages := 0
			for {
				p := w.page(t, w.aliceKey, demo, run, route, q)
				pages++
				paged = append(paged, ids(p.Items)...)
				if p.NextCursor == nil || pages > 5 {
					break
				}
				q.Set("cursor", *p.NextCursor)
			}
			if pages != 3 || !slices.Equal(paged, expected) {
				t.Errorf("%s order=%s by 2: %d pages %v, want 3 pages %v", route, order, pages, paged, expected)
			}
		}
	}
}

// listsFilter reads three records of each kind through each filter and
// time range.
func listsFilter(t *testing.T, w *world) {
	// Per route: r1 at t1, r2 at t2, r3 at t3; own is the route's own
	// label and its values for r1, r2 and r3.
	type ownLabel struct{ name, v1, v2, v3 string }
	own := map[string]ownLabel{
		"/events":            {"event_type", "run.start", "session.fail", "run.start"},
		"/sessions/outcomes": {"status", "completed", "failed", "completed"},
		"/tools/calls":       {"tool_name", "bash", "grep", "bash"},
		"/tools/errors":      {"tool_name", "bash", "grep", "bash"},
		"/traces":            {"event_type", "session.init", "tool.use", "session.init"},
	}
	for _, route := range listRoutes {
		run := w.freshRun()
		o := own[route]
		r1, r2, r3 := w.freshID(), w.freshID(), w.freshID()
		w.seed(t, route, demo, run, fullRecord(route, r1, t1, map[string]string{"node_id": "n1", "session_id": session1, o.name: o.v1, "severity": "info"}))
		w.seed(t, route, demo, run, fullRecord(route, r2, t2, map[string]string{"node_id": "n2", "session_id": session2, o.name: o.v2, "severity": "error"}))
		w.seed(t, route, demo, run, fullRecord(route, r3, t3, map[string]string{"node_id": "n1", "session_id": session2, o.name: o.v3, "severity": "error"}))
		// A filter of another route is ignored like any unknown one, so
		// severity selects on /events alone.
		bySeverity := []string{r1, r2, r3}
		if route == "/events" {
			bySeverity = []string{r2, r3}
		}
		cases := []struct {
			query string
			want  []string
		}{
			{"node_id=n1", []string{r1, r3}},
			{"session_id=" + strings.ToUpper(session2), []string{r2, r3}},
			{o.name + "=" + o.v2, []string{r2}},
			{"node_id=n1&session_id=" + session2, []string{r3}},
			{"node_id=n", nil},
			{"since=" + t2, []string{r2, r3}},
			{"until=" + t2, []string{r1}},
			{"since=" + t1 + "&until=" + t3, []string{r1, r2}},
			{"since=" + url.QueryEscape("2026-06-01T14:00:01+02:00"), []string{r2, r3}},
			{"no_such_filter=x", []string{r1, r2, r3}},
			{"severity=error", bySeverity},
		}
		for _, c := range cases {
			q, err := url.ParseQuery(c.query)
			if err != nil {
				t.Fatal(err)
			}
			got := ids(w.page(t, w.aliceKey, demo, run, route, q).Items)
			if want := append([]string{}, c.want...); !slices.Equal(got, want) {
				t.Errorf("%s?%s: %v, want %v", route, c.query, got, want)
			}
		}
	}
}

// listsRefuseInvalidParameters sends every list route invalid parameters
// and counts the store's reads meanwhile.
func listsRefuseInvalidParameters(t *testing.T, w *world) {
	run := w.freshRun()
	for range 2 {
		w.seed(t, "/events", demo, run, fullRecord("/events", w.freshID(), t1, map[string]string{"node_id": "a"}))
	}
	cursorOf := func(query url.Values) string {
		t.Helper()
		p := w.page(t, w.aliceKey, demo, run, "/events", query)
		if p.NextCursor == nil {
			t.Fatalf("GET /events %v gave no next_cursor", query)
		}
		return *p.NextCursor
	}
	eventsCursor := cursorOf(url.Values{"limit": {"1"}})
	nodeACursor := cursorOf(url.Values{"limit": {"1"}, "node_id": {"a"}})

	type row struct {
		routes []string
		run    string
		query  url.Values
		param  string
	}
	rows := []row{
		{listRoutes, "2026", nil, "run_id"},
		{listRoutes, "20260601_120000_ABCDEF", nil, "run_id"},
		{listRoutes, "20260601_120000_abcde", nil, "run_id"},
		{[]string{"/events"}, run, url.Values{"severity": {"fatal"}}, "severity"},
		{listRoutes, run, url.Values{"order": {"up"}}, "order"},
		{[]string{"/traces"}, run, url.Values{"event_type": {"tool.call"}}, "event_type"},
		{listRoutes, run, url.Values{"session_id": {"abc"}}, "session_id"},
		{listRoutes, run, url.Values{"since": {"yesterday"}}, "since"},
		{listRoutes, run, url.Values{"until": {"2026-06-01"}}, "until"},
		{listRoutes, run, url.Values{"since": {t2}, "until": {t2}}, "since"},
		{listRoutes, run, url.Values{"since": {t3}, "until": {t2}}, "since"},
		{listRoutes, run, url.Values{"limit": {"0"}}, "limit"},
		{listRoutes, run, url.Values{"limit": {"201"}}, "limit"},
		{listRoutes, run, url.Values{"limit": {"x"}}, "limit"},
		{listRoutes, run, url.Values{"cursor": {"%%%"}}, "cursor"},
		{[]string{"/traces"}, run, url.Values{"cursor": {eventsCursor}}, "cursor"},
		{[]string{"/events"}, run, url.Values{"cursor": {nodeACursor}, "node_id": {"b"}}, "cursor"},
	}
	before := w.reads.Load()
	for _, r := range rows {
		for _, route := range r.routes {
			status, raw := w.list(t, w.aliceKey, demo, r.run, route, r.query)
			typ, msg := errorOf(t, raw)
			if status != http.StatusBadRequest || typ != "invalid_request" || !strings.Contains(msg, r.param) {
				t.Errorf("%s on run %s with %v = %d %s, want 400 invalid_request naming %s", route, r.run, r.query, status, raw, r.param)
			}
		}
	}
	if n := w.reads.Load(); n != before {
		t.Errorf("the store saw %d reads for refused requests, want none", n-before)
	}
	w.page(t, w.aliceKey, demo, run, "/events", nil)
	if w.reads.Load() == before {
		t.Error("a valid request was not counted as a read: the count proves nothing")
	}
}

// listsRefuseOutsiders reads every per-run route without a credential,
// without the scope and as an outsider.
func listsRefuseOutsiders(t *testing.T, w *world) {
	sessionsRead := w.pat(t, w.alice, "sessions:read")
	w.seed(t, "/events", demo, someRunID, fullRecord("/events", w.freshID(), t1, nil))
	for _, route := range readRoutes {
		status, raw := w.list(t, "", demo, someRunID, route, nil)
		if typ, _ := errorOf(t, raw); status != http.StatusUnauthorized || typ != "unauthenticated" {
			t.Errorf("%s without a credential = %d %s, want 401 unauthenticated", route, status, raw)
		}
		for _, slug := range []string{demo, nowhere} {
			status, raw := w.list(t, sessionsRead, slug, someRunID, route, nil)
			if typ, _ := errorOf(t, raw); status != http.StatusForbidden || typ != "insufficient_scope" {
				t.Errorf("%s on %s with a PAT without audit scope = %d %s, want 403 insufficient_scope", route, slug, status, raw)
			}
		}
		status, onDemo := w.list(t, w.carolKey, demo, someRunID, route, nil)
		if typ, _ := errorOf(t, onDemo); status != http.StatusNotFound || typ != "not_found" {
			t.Errorf("%s on demo as an outsider = %d %s, want 404 not_found", route, status, onDemo)
		}
		if _, onNowhere := w.list(t, w.carolKey, nowhere, someRunID, route, nil); string(onDemo) != string(onNowhere) {
			t.Errorf("%s: an outsider's answers differ: %s on demo, %s on a missing slug", route, onDemo, onNowhere)
		}
	}
}
