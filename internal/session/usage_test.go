package session_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/session"
)

// usageMembers are the members of every usage record.
var usageMembers = []string{
	"cache_read_tokens", "id", "input_tokens", "model", "output_tokens", "reported_at", "session_id", "workspace_slug",
}

// report sends POST /sessions/:id/usage with body as credential.
func (w *world) report(t *testing.T, credential, id string, body any) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodPost, "/api/v1/sessions/"+id+"/usage", credential, body)
}

// reported reports usage with body as credential, fails the test unless it
// is answered 201, and returns the usage record.
func (w *world) reported(t *testing.T, credential, id string, body any) map[string]any {
	t.Helper()
	status, raw := w.report(t, credential, id, body)
	if status != http.StatusCreated {
		t.Fatalf("POST usage %v = %d %s, want 201", body, status, raw)
	}
	return decode(t, raw)
}

// tokenSummary is the token_summary of the session id, read as credential.
func (w *world) tokenSummary(t *testing.T, credential, id string) map[string]any {
	t.Helper()
	status, raw := w.get(t, credential, id)
	if status != http.StatusOK {
		t.Fatalf("GET /sessions/%s = %d %s, want 200", id, status, raw)
	}
	return decode(t, raw)["token_summary"].(map[string]any)
}

// summary is a token_summary or totals object as JSON decodes it.
func summary(in, out, cacheRead float64, models ...any) map[string]any {
	if models == nil {
		models = []any{}
	}
	return map[string]any{
		"total_input_tokens": in, "total_output_tokens": out, "total_cache_read_tokens": cacheRead, "models_used": models,
	}
}

// usageList is the answer of GET /sessions/:id/usage.
type usageList struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
	SessionID  string           `json:"session_id"`
	Totals     map[string]any   `json:"totals"`
}

// listUsage sends GET /sessions/:id/usage?query as credential.
func (w *world) listUsage(t *testing.T, credential, id, query string) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodGet, "/api/v1/sessions/"+id+"/usage?"+query, credential, nil)
}

// usagePages reads the usage list of the session id with query as
// credential, following next_cursor to its end, and returns its pages.
func (w *world) usagePages(t *testing.T, credential, id, query string) []usageList {
	t.Helper()
	var pages []usageList
	q := query
	for {
		status, raw := w.listUsage(t, credential, id, q)
		if status != http.StatusOK {
			t.Fatalf("GET usage?%s = %d %s, want 200", q, status, raw)
		}
		var p usageList
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		pages = append(pages, p)
		if p.NextCursor == nil {
			return pages
		}
		if len(pages) > 10 {
			t.Fatalf("GET usage?%s: more than 10 pages", query)
		}
		q = query + "&cursor=" + *p.NextCursor
	}
}

// idsOf are the ids of the items of pages, in their order.
func idsOf(pages []usageList) []string {
	var ids []string
	for _, p := range pages {
		for _, item := range p.Items {
			ids = append(ids, item["id"].(string))
		}
	}
	return ids
}

// observed is one call of a usage observer.
type observed struct {
	workspace, model         string
	input, output, cacheRead int64
}

// recorder is a usage observer that records its calls.
type recorder struct {
	mu    sync.Mutex
	calls []observed
}

func (r *recorder) observe(workspace, model string, input, output, cacheRead int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, observed{workspace, model, input, output, cacheRead})
}

func (r *recorder) got() []observed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func TestUsage_StoresARecordAndAddsItToTheSession(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		name, workspace, sender string
	}{
		{"alice on her session on demo", demo, w.aliceKey},
		{"the admin token on alice's session on a fork", fork1, w.Token},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := w.open(t, w.aliceKey, map[string]any{"workspace_slug": c.workspace})["id"].(string)
			w.clock.Advance(time.Minute)
			r1 := w.reported(t, c.sender, id, map[string]any{"model": "m2", "input_tokens": 10, "output_tokens": 5})
			u := freshID()
			r2 := w.reported(t, c.sender, id, map[string]any{"model": "m1", "cache_read_tokens": 3, "id": strings.ToUpper(u)})

			keys := make([]string, 0, len(r1))
			for k := range r1 {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, usageMembers) {
				t.Errorf("members = %v, want %v", keys, usageMembers)
			}
			want := map[string]any{
				"session_id": id, "workspace_slug": c.workspace, "model": "m2", "input_tokens": 10.0, "output_tokens": 5.0,
				"cache_read_tokens": 0.0, "reported_at": w.clock.stamp(),
			}
			for k, v := range want {
				if r1[k] != v {
					t.Errorf("first record: %s = %v, want %v", k, r1[k], v)
				}
			}
			if gen, _ := r1["id"].(string); !v4.MatchString(gen) {
				t.Errorf("generated id = %v, want a version 4 UUID in lower case", r1["id"])
			}
			if r2["id"] != u || r2["input_tokens"] != 0.0 || r2["output_tokens"] != 0.0 || r2["cache_read_tokens"] != 3.0 {
				t.Errorf("second record = %v, want id %s and counts 0, 0, 3", r2, u)
			}
			if got, want := w.tokenSummary(t, w.aliceKey, id), summary(10, 5, 3, "m1", "m2"); !reflect.DeepEqual(got, want) {
				t.Errorf("token_summary = %v, want %v", got, want)
			}
		})
	}
}

func TestUsage_ReplaysAStoredRecord(t *testing.T) {
	w := newWorld(t)
	var rec recorder
	w.sessions.OnUsage(rec.observe)
	id := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	u := freshID()
	first := w.reported(t, w.aliceKey, id, map[string]any{"id": u, "model": "m1", "input_tokens": 10})
	w.clock.Advance(time.Minute)

	replay := func(t *testing.T, sentID string) {
		t.Helper()
		status, raw := w.report(t, w.aliceKey, id, map[string]any{"id": sentID, "model": "m9", "input_tokens": 999})
		if status != http.StatusOK || !reflect.DeepEqual(decode(t, raw), first) {
			t.Errorf("replay = %d %s, want 200 %v", status, raw, first)
		}
		if n := w.usageCount(t, id); n != 1 {
			t.Errorf("usage records = %d, want 1", n)
		}
		if got, want := w.tokenSummary(t, w.aliceKey, id), summary(10, 0, 0, "m1"); !reflect.DeepEqual(got, want) {
			t.Errorf("token_summary = %v, want %v", got, want)
		}
		if n := len(rec.got()); n != 1 {
			t.Errorf("observer calls = %d, want 1", n)
		}
	}
	t.Run("while the session is active", func(t *testing.T) { replay(t, u) })
	t.Run("after the session completed, with the id in upper case", func(t *testing.T) {
		if status, raw := w.complete(t, w.aliceKey, id, map[string]any{"status": "completed"}); status != http.StatusOK {
			t.Fatalf("POST complete = %d %s, want 200", status, raw)
		}
		replay(t, strings.ToUpper(u))
	})
}

func TestUsage_RefusesAnIdOfAnotherSession(t *testing.T) {
	w := newWorld(t)
	a := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	b := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	u := freshID()
	w.reported(t, w.aliceKey, a, map[string]any{"id": u, "model": "m1", "input_tokens": 10})

	status, raw := w.report(t, w.aliceKey, b, map[string]any{"id": u, "model": "m1", "input_tokens": 10})
	if errorType, _ := errorOf(t, raw); status != http.StatusConflict || errorType != "conflict" {
		t.Errorf("POST usage on B with A's usage id = %d %s, want 409 conflict", status, raw)
	}
	if got, want := w.tokenSummary(t, w.aliceKey, b), summary(0, 0, 0); !reflect.DeepEqual(got, want) {
		t.Errorf("B's token_summary = %v, want %v", got, want)
	}
	if got, want := w.tokenSummary(t, w.aliceKey, a), summary(10, 0, 0, "m1"); !reflect.DeepEqual(got, want) {
		t.Errorf("A's token_summary = %v, want %v", got, want)
	}
}

func TestUsage_RefusesASessionThatIsNotActive(t *testing.T) {
	w := newWorld(t)
	for _, ended := range []string{"completed", "failed", "timeout", "terminated"} {
		for _, withID := range []bool{true, false} {
			name := ended + "/without an id"
			if withID {
				name = ended + "/with a new id"
			}
			t.Run(name, func(t *testing.T) {
				id := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
				if ended == session.StatusTerminated {
					w.terminate(t, id)
				} else if status, raw := w.complete(t, w.aliceKey, id, map[string]any{"status": ended}); status != http.StatusOK {
					t.Fatalf("POST complete = %d %s, want 200", status, raw)
				}
				body := map[string]any{"model": "m1", "input_tokens": 1}
				if withID {
					body["id"] = freshID()
				}
				status, raw := w.report(t, w.aliceKey, id, body)
				if errorType, _ := errorOf(t, raw); status != http.StatusConflict || errorType != "session_not_active" {
					t.Errorf("POST usage = %d %s, want 409 session_not_active", status, raw)
				}
				if n := w.usageCount(t, id); n != 0 {
					t.Errorf("usage records = %d, want 0", n)
				}
			})
		}
	}
}

func TestUsage_RefusesAnInvalidBody(t *testing.T) {
	w := newWorld(t)
	id := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	cases := []struct {
		name, member string
		body         map[string]any
	}{
		{"an empty body", "model", map[string]any{}},
		{"model empty", "model", map[string]any{"model": ""}},
		{"model blank", "model", map[string]any{"model": "   "}},
		{"model 3", "model", map[string]any{"model": 3}},
		{"model of 256 characters", "model", map[string]any{"model": strings.Repeat("é", 256)}},
		{"id x", "id", map[string]any{"model": "m1", "id": "x"}},
		{"id of 32 hexadecimal digits", "id", map[string]any{"model": "m1", "id": strings.ReplaceAll(freshID(), "-", "")}},
		{"input_tokens -1", "input_tokens", map[string]any{"model": "m1", "input_tokens": -1}},
		{"output_tokens 1.5", "output_tokens", map[string]any{"model": "m1", "output_tokens": 1.5}},
		{"cache_read_tokens '2'", "cache_read_tokens", map[string]any{"model": "m1", "cache_read_tokens": "2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := w.report(t, w.aliceKey, id, c.body)
			errorType, message := errorOf(t, raw)
			if status != http.StatusBadRequest || errorType != "invalid_request" || !strings.HasPrefix(message, c.member+" ") {
				t.Errorf("POST usage %v = %d %s, want 400 invalid_request with a message naming %s", c.body, status, raw, c.member)
			}
			if n := w.usageCount(t, id); n != 0 {
				t.Errorf("usage records = %d, want 0", n)
			}
		})
	}
	t.Run("a model of 255 characters is accepted", func(t *testing.T) {
		model := strings.Repeat("é", 255)
		if r := w.reported(t, w.aliceKey, id, map[string]any{"model": model}); r["model"] != model {
			t.Errorf("model = %v, want the 255 characters sent", r["model"])
		}
	})
}

func TestUsage_NotifiesEveryObserverOnceAfterTheRecordIsStored(t *testing.T) {
	w := newWorld(t)
	id := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	t.Run("with no observer registered a report is stored", func(t *testing.T) {
		w.reported(t, w.aliceKey, id, map[string]any{"model": "m0"})
	})

	var first, second recorder
	var mu sync.Mutex
	var storedAtCall []int
	w.sessions.OnUsage(first.observe)
	w.sessions.OnUsage(func(workspace, model string, input, output, cacheRead int64) {
		var n int
		// The observer runs in the hub; it reads what was committed.
		_ = w.Store.Main().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM session_usage WHERE session_id = ? AND model = ?`, id, model).Scan(&n)
		mu.Lock()
		storedAtCall = append(storedAtCall, n)
		mu.Unlock()
		second.observe(workspace, model, input, output, cacheRead)
	})
	u := freshID()
	w.reported(t, w.aliceKey, id, map[string]any{"id": u, "model": "m1", "input_tokens": 10, "output_tokens": 5, "cache_read_tokens": 3})
	want := []observed{{demo, "m1", 10, 5, 3}}
	for name, r := range map[string]*recorder{"first": &first, "second": &second} {
		if got := r.got(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s observer recorded %v, want %v", name, got, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(storedAtCall, []int{1}) {
		t.Errorf("records of the report stored when the observer was called = %v, want [1]", storedAtCall)
	}

	if status, raw := w.report(t, w.aliceKey, id, map[string]any{"model": ""}); status != http.StatusBadRequest {
		t.Errorf("POST usage with model '' = %d %s, want 400", status, raw)
	}
	if status, raw := w.report(t, w.aliceKey, id, map[string]any{"id": u, "model": "m1", "input_tokens": 10}); status != http.StatusOK {
		t.Errorf("replay = %d %s, want 200", status, raw)
	}
	ended := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	if status, raw := w.complete(t, w.aliceKey, ended, map[string]any{"status": "completed"}); status != http.StatusOK {
		t.Fatalf("POST complete = %d %s, want 200", status, raw)
	}
	if status, raw := w.report(t, w.aliceKey, ended, map[string]any{"model": "m1", "input_tokens": 1}); status != http.StatusConflict {
		t.Errorf("POST usage on a completed session = %d %s, want 409", status, raw)
	}
	for name, r := range map[string]*recorder{"first": &first, "second": &second} {
		if got := r.got(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s observer after the refusals and the replay recorded %v, want %v", name, got, want)
		}
	}
}

func TestUsage_ListsTheRecordsWithTotals(t *testing.T) {
	w := newWorld(t)
	id := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	// Two records in one second, the one with the greater id first, so that
	// the order by id is not the order they were stored in.
	const late, early = "ffffffff-0000-4000-8000-000000000000", "00000000-0000-4000-8000-000000000000"
	reports := []struct {
		advance time.Duration
		body    map[string]any
	}{
		{time.Second, map[string]any{"model": "m2", "input_tokens": 1, "output_tokens": 10, "cache_read_tokens": 100}},
		{time.Second, map[string]any{"id": late, "model": "m1", "input_tokens": 2, "output_tokens": 20, "cache_read_tokens": 200}},
		{0, map[string]any{"id": early, "model": "m2", "input_tokens": 3, "output_tokens": 30, "cache_read_tokens": 300}},
		{time.Second, map[string]any{"model": "m3", "input_tokens": 4, "output_tokens": 40, "cache_read_tokens": 400}},
		{time.Second, map[string]any{"model": "m1", "input_tokens": 5, "output_tokens": 50, "cache_read_tokens": 500}},
	}
	var stored []map[string]any
	for _, r := range reports {
		w.clock.Advance(r.advance)
		stored = append(stored, w.reported(t, w.aliceKey, id, r.body))
	}
	var desc []string
	for i := len(stored) - 1; i >= 0; i-- {
		desc = append(desc, stored[i]["id"].(string))
	}
	desc[2], desc[3] = late, early
	asc := slices.Clone(desc)
	slices.Reverse(asc)
	totals := summary(15, 150, 1500, "m1", "m2", "m3")

	t.Run("pages of two, descending by default", func(t *testing.T) {
		pages := w.usagePages(t, w.aliceKey, id, "limit=2")
		if len(pages[0].Items) != 2 || pages[0].NextCursor == nil {
			t.Errorf("first page holds %d items and next_cursor %v, want 2 and a cursor", len(pages[0].Items), pages[0].NextCursor)
		}
		if !reflect.DeepEqual(pages[0].Items[0], stored[4]) {
			t.Errorf("first item = %v, want the record POST answered, %v", pages[0].Items[0], stored[4])
		}
		for i, p := range pages {
			if p.SessionID != id || !reflect.DeepEqual(p.Totals, totals) {
				t.Errorf("page %d: session_id %s, totals %v; want %s and %v", i, p.SessionID, p.Totals, id, totals)
			}
		}
		if got := idsOf(pages); !slices.Equal(got, desc) {
			t.Errorf("ids = %v, want %v", got, desc)
		}
	})
	orders := []struct {
		query string
		want  []string
	}{
		{"order=desc", desc},
		{"order=asc", asc},
		{"order=asc&limit=2", asc},
	}
	for _, o := range orders {
		t.Run(o.query, func(t *testing.T) {
			if got := idsOf(w.usagePages(t, w.aliceKey, id, o.query)); !slices.Equal(got, o.want) {
				t.Errorf("ids = %v, want %v", got, o.want)
			}
		})
	}
	t.Run("order=up is refused", func(t *testing.T) {
		status, raw := w.listUsage(t, w.aliceKey, id, "order=up")
		if errorType, _ := errorOf(t, raw); status != http.StatusBadRequest || errorType != "invalid_request" {
			t.Errorf("GET usage?order=up = %d %s, want 400 invalid_request", status, raw)
		}
	})
	t.Run("a session without records", func(t *testing.T) {
		empty := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
		status, raw := w.listUsage(t, w.aliceKey, empty, "")
		if status != http.StatusOK || !strings.Contains(string(raw), `"items":[]`) {
			t.Fatalf("GET usage = %d %s, want 200 with an empty items array", status, raw)
		}
		var p usageList
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if p.NextCursor != nil || p.SessionID != empty || !reflect.DeepEqual(p.Totals, summary(0, 0, 0)) {
			t.Errorf("GET usage = %s, want next_cursor null, session_id %s and totals of 0 with models_used []", raw, empty)
		}
	})
	t.Run("carol, who may not read the session, is refused", func(t *testing.T) {
		status, raw := w.listUsage(t, w.carolKey, id, "")
		if errorType, message := errorOf(t, raw); status != http.StatusNotFound || errorType != "not_found" || message != "session not found" {
			t.Errorf("GET usage = %d %s, want 404 not_found with message %q", status, raw, "session not found")
		}
	})
	t.Run("bob, a member of demo's org, reads it", func(t *testing.T) {
		if got := idsOf(w.usagePages(t, w.bobKey, id, "")); !slices.Equal(got, desc) {
			t.Errorf("ids = %v, want %v", got, desc)
		}
	})
}

func TestActiveCounts_CountsActiveSessionsPerWorkspace(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if got, err := w.sessions.ActiveCounts(ctx); err != nil || len(got) != 0 {
		t.Fatalf("ActiveCounts with no session = %v, %v; want none", got, err)
	}
	for _, s := range []struct{ credential, workspace string }{
		{w.aliceKey, demo}, {w.Token, demo}, {w.aliceKey, fork1}, {w.daveKey, other},
	} {
		w.open(t, s.credential, map[string]any{"workspace_slug": s.workspace})
	}
	done := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	if status, raw := w.complete(t, w.aliceKey, done, map[string]any{"status": "completed"}); status != http.StatusOK {
		t.Fatalf("POST complete = %d %s, want 200", status, raw)
	}
	w.terminate(t, w.open(t, w.aliceKey, map[string]any{"workspace_slug": fork2})["id"].(string))

	got, err := w.sessions.ActiveCounts(ctx)
	if want := map[string]int64{demo: 2, fork1: 1, other: 1}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ActiveCounts = %v, %v; want %v", got, err, want)
	}
}
