package session_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
)

// spent is a usage record a cost test writes straight into the store, so
// that it can be reported at any time: session is the id of a stored
// session, at an RFC 3339 timestamp in UTC with whole seconds.
type spent struct {
	session, workspace, model, at string
	in, out, cacheRead            int64
}

// spend stores the usage records records.
func (w *world) spend(t *testing.T, records ...spent) {
	t.Helper()
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, r := range records {
			_, err := tx.ExecContext(ctx, `
INSERT INTO session_usage (id, session_id, workspace_slug, model, input_tokens, output_tokens, cache_read_tokens, reported_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, freshID(), r.session, r.workspace, r.model, r.in, r.out, r.cacheRead, r.at)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// costMembers are the members of every cost report, and costItemMembers
// those of each of its items.
var (
	costMembers     = []string{"group_by", "items", "next_cursor", "period", "totals", "workspace"}
	costItemMembers = []string{"cache_read_tokens", "input_tokens", "key", "output_tokens", "sessions"}
)

// costReport is the answer of GET /workspaces/:slug/cost.
type costReport struct {
	Workspace string `json:"workspace"`
	Period    struct {
		Since string `json:"since"`
		Until string `json:"until"`
	} `json:"period"`
	GroupBy    string           `json:"group_by"`
	Totals     map[string]any   `json:"totals"`
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// cost sends GET /workspaces/:slug/cost?query as credential.
func (w *world) cost(t *testing.T, credential, slug, query string) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodGet, "/api/v1/workspaces/"+slug+"/cost?"+query, credential, nil)
}

// costOf reads the cost report of demo with query as credential, fails the
// test unless it is answered 200 with exactly the report's members, and
// returns it.
func (w *world) costOf(t *testing.T, credential, query string) costReport {
	t.Helper()
	status, raw := w.cost(t, credential, demo, query)
	if status != http.StatusOK {
		t.Fatalf("GET cost?%s = %d %s, want 200", query, status, raw)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if got := slices.Sorted(maps.Keys(members)); !reflect.DeepEqual(got, costMembers) {
		t.Errorf("GET cost?%s: members %v, want %v", query, got, costMembers)
	}
	var r costReport
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if r.Items == nil {
		t.Errorf("GET cost?%s: items = %s, want an array", query, members["items"])
	}
	for _, item := range r.Items {
		if got := slices.Sorted(maps.Keys(item)); !reflect.DeepEqual(got, costItemMembers) {
			t.Errorf("GET cost?%s: item members %v, want %v", query, got, costItemMembers)
		}
	}
	return r
}

// costPages reads the cost report of demo with query as credential,
// following next_cursor to its end, and returns its pages.
func (w *world) costPages(t *testing.T, credential, query string) []costReport {
	t.Helper()
	var pages []costReport
	q := query
	for {
		p := w.costOf(t, credential, q)
		pages = append(pages, p)
		if p.NextCursor == nil {
			return pages
		}
		if len(pages) > 10 {
			t.Fatalf("GET cost?%s: more than 10 pages", query)
		}
		q = query + "&cursor=" + url.QueryEscape(*p.NextCursor)
	}
}

// sums are the four numbers of a cost report's totals or item as JSON
// decodes them.
func sums(in, out, cacheRead, sessions float64) map[string]any {
	return map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_tokens": cacheRead, "sessions": sessions}
}

// keysOf are the keys of the items of pages, in their order.
func keysOf(pages ...costReport) []string {
	keys := []string{}
	for _, p := range pages {
		for _, item := range p.Items {
			keys = append(keys, item["key"].(string))
		}
	}
	return keys
}

// window is the query of a cost report over [since, until).
func window(since, until time.Time) string {
	return "since=" + since.Format(time.RFC3339) + "&until=" + until.Format(time.RFC3339)
}

func TestCost_SumsTheWorkspacesOwnRecordsInTheWindow(t *testing.T) {
	w := newWorld(t)
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }
	a := w.store(t, stored{workspace: demo, user: w.alice})
	b := w.store(t, stored{workspace: demo, user: w.bob})
	f := w.store(t, stored{workspace: fork1, parent: demo, user: w.alice})
	w.spend(t,
		spent{session: a, workspace: demo, model: "m1", at: at(0), in: 10, out: 1},
		spent{session: a, workspace: demo, model: "m2", at: at(time.Hour), in: 5, cacheRead: 2},
		spent{session: b, workspace: demo, model: "m1", at: at(2 * time.Hour), in: 7, out: 7, cacheRead: 7},
		spent{session: f, workspace: fork1, model: "m1", at: at(time.Hour), in: 100, out: 100, cacheRead: 100},
	)
	cases := []struct {
		name         string
		since, until time.Duration
		want         map[string]any
		wantKeys     []string
	}{
		{"the record at since is counted and the one at until is not", 0, 2 * time.Hour, sums(15, 1, 2, 1), []string{"2026-03-01"}},
		{"a wider window counts every session", 0, 3 * time.Hour, sums(22, 8, 9, 2), []string{"2026-03-01"}},
		{"a window that holds no record answers zero totals and no item", -time.Hour, 0, sums(0, 0, 0, 0), []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := window(t0.Add(tc.since), t0.Add(tc.until))
			r := w.costOf(t, w.aliceKey, query)
			if r.Workspace != demo {
				t.Errorf("workspace = %q, want %q", r.Workspace, demo)
			}
			if !reflect.DeepEqual(r.Totals, tc.want) {
				t.Errorf("totals = %v, want %v", r.Totals, tc.want)
			}
			if got := keysOf(r); !reflect.DeepEqual(got, tc.wantKeys) {
				t.Errorf("keys = %v, want %v", got, tc.wantKeys)
			}
			if len(r.Items) == 1 {
				item := sumsOf(r.Items[0])
				if !reflect.DeepEqual(item, tc.want) {
					t.Errorf("item = %v, want the totals %v", item, tc.want)
				}
			}
			if r.NextCursor != nil {
				t.Errorf("next_cursor = %q, want null", *r.NextCursor)
			}
		})
	}
}

// sumsOf is an item of a cost report without its key.
func sumsOf(item map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range item {
		if k != "key" {
			out[k] = v
		}
	}
	return out
}

func TestCost_GroupsByDaySessionOrModel(t *testing.T) {
	w := newWorld(t)
	a := w.store(t, stored{workspace: demo, user: w.alice})
	b := w.store(t, stored{workspace: demo, user: w.alice})
	w.spend(t,
		spent{session: a, workspace: demo, model: "m1", at: "2026-09-01T23:59:59Z", in: 10},
		spent{session: a, workspace: demo, model: "m2", at: "2026-09-02T00:00:00Z", in: 5},
		spent{session: b, workspace: demo, model: "m1", at: "2026-09-02T10:00:00Z", in: 7},
	)
	ids := slices.Sorted(slices.Values([]string{a, b}))
	byID := map[string]map[string]any{a: sums(15, 0, 0, 1), b: sums(7, 0, 0, 1)}
	cases := []struct {
		name, groupBy, wantGroupBy string
		wantKeys                   []string
		wantItems                  []map[string]any
	}{
		{"absent groups by day", "", "day", []string{"2026-09-01", "2026-09-02"}, []map[string]any{sums(10, 0, 0, 1), sums(12, 0, 0, 2)}},
		{"day", "day", "day", []string{"2026-09-01", "2026-09-02"}, []map[string]any{sums(10, 0, 0, 1), sums(12, 0, 0, 2)}},
		{"session", "session", "session", ids, []map[string]any{byID[ids[0]], byID[ids[1]]}},
		{"model", "model", "model", []string{"m1", "m2"}, []map[string]any{sums(17, 0, 0, 2), sums(5, 0, 0, 1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := "since=2026-09-01T00:00:00Z&until=2026-09-03T00:00:00Z"
			if tc.groupBy != "" {
				query += "&group_by=" + tc.groupBy
			}
			r := w.costOf(t, w.bobKey, query)
			if r.GroupBy != tc.wantGroupBy {
				t.Errorf("group_by = %q, want %q", r.GroupBy, tc.wantGroupBy)
			}
			if got := keysOf(r); !reflect.DeepEqual(got, tc.wantKeys) {
				t.Errorf("keys = %v, want %v", got, tc.wantKeys)
			}
			var items []map[string]any
			var inputs float64
			for _, item := range r.Items {
				items = append(items, sumsOf(item))
				inputs += item["input_tokens"].(float64)
			}
			if !reflect.DeepEqual(items, tc.wantItems) {
				t.Errorf("items = %v, want %v", items, tc.wantItems)
			}
			if want := sums(22, 0, 0, 2); !reflect.DeepEqual(r.Totals, want) {
				t.Errorf("totals = %v, want %v", r.Totals, want)
			}
			if inputs != r.Totals["input_tokens"] {
				t.Errorf("the items' input tokens add up to %v, totals hold %v", inputs, r.Totals["input_tokens"])
			}
		})
	}
}

func TestCost_DefaultsTheWindowToTheLastDayAndNormalisesItToUTC(t *testing.T) {
	w := newWorld(t)
	now := time.Date(2026, 9, 2, 12, 0, 0, 500_000_000, time.UTC)
	w.clock.Advance(now.Sub(w.clock.Now()))
	a := w.store(t, stored{workspace: demo, user: w.alice})
	w.spend(t,
		spent{session: a, workspace: demo, model: "m1", at: "2026-09-01T11:59:59Z", in: 1},
		spent{session: a, workspace: demo, model: "m1", at: "2026-09-01T12:00:00Z", in: 10},
		spent{session: a, workspace: demo, model: "m1", at: "2026-09-02T12:00:00Z", in: 100},
	)
	cases := []struct {
		name, query, wantSince, wantUntil string
		wantInput                         float64
	}{
		{"no bounds: the 24 hours before the clock, up to its next whole second", "", "2026-09-01T12:00:00Z", "2026-09-02T12:00:01Z", 110},
		{"no until: up to the clock's next whole second", "since=2026-09-01T11:59:59Z", "2026-09-01T11:59:59Z", "2026-09-02T12:00:01Z", 111},
		{"no since: 24 hours before the clock", "until=2026-09-02T12:00:01Z", "2026-09-01T12:00:00Z", "2026-09-02T12:00:01Z", 110},
		{"offsets are moved to UTC and fractions cut", "since=2026-09-01T02:00:00%2B02:00&until=2026-09-01T03:00:00.9%2B02:00", "2026-09-01T00:00:00Z", "2026-09-01T01:00:00Z", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := w.costOf(t, w.aliceKey, tc.query)
			if r.Period.Since != tc.wantSince || r.Period.Until != tc.wantUntil {
				t.Errorf("period = %+v, want {%s %s}", r.Period, tc.wantSince, tc.wantUntil)
			}
			if r.Totals["input_tokens"] != tc.wantInput {
				t.Errorf("totals = %v, want input_tokens %v", r.Totals, tc.wantInput)
			}
		})
	}
}

func TestCost_PagesTheGroupsWithTheSameTotalsOnEveryPage(t *testing.T) {
	w := newWorld(t)
	a := w.store(t, stored{workspace: demo, user: w.alice})
	b := w.store(t, stored{workspace: demo, user: w.bob})
	models := []string{"m4", "m2", "m5", "m1", "m3"}
	for i, m := range models {
		s := a
		if i%2 == 1 {
			s = b
		}
		w.spend(t, spent{session: s, workspace: demo, model: m, at: fmt.Sprintf("2026-03-01T%02d:00:00Z", i), in: int64(i + 1), out: 2})
	}
	query := "since=2026-03-01T00:00:00Z&until=2026-03-02T00:00:00Z&group_by=model&limit=2"
	pages := w.costPages(t, w.aliceKey, query)
	if len(pages) != 3 {
		t.Errorf("pages = %d, want 3", len(pages))
	}
	if got, want := keysOf(pages...), []string{"m1", "m2", "m3", "m4", "m5"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	for i, p := range pages {
		if want := sums(15, 10, 0, 2); !reflect.DeepEqual(p.Totals, want) {
			t.Errorf("page %d: totals = %v, want %v", i+1, p.Totals, want)
		}
	}
}

func TestCost_RefusesAnInvalidParameter(t *testing.T) {
	w := newWorld(t)
	w.clock.Advance(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC).Sub(w.clock.Now()))
	a := w.store(t, stored{workspace: demo, user: w.alice})
	w.spend(t,
		spent{session: a, workspace: demo, model: "m1", at: "2026-09-01T13:00:00Z", in: 1},
		spent{session: a, workspace: demo, model: "m1", at: "2026-09-02T01:00:00Z", in: 1},
	)
	day := w.costOf(t, w.aliceKey, "limit=1")
	if day.NextCursor == nil {
		t.Fatal("the day grouping with limit=1 answered no next_cursor")
	}
	cases := []struct{ query, param string }{
		{"group_by=week", "group_by"},
		{"since=yesterday", "since"},
		{"until=1725148800", "until"},
		{"since=2026-09-01T00:00:00Z&until=2026-09-01T00:00:00Z", "since"},
		{"since=2026-09-01T00:00:01Z&until=2026-09-01T00:00:00Z", "since"},
		{"since=2026-09-02T12:00:01Z", "since"},
		{"limit=0", "limit"},
		{"limit=201", "limit"},
		{"group_by=model&limit=1&cursor=" + url.QueryEscape(*day.NextCursor), "cursor"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			status, raw := w.cost(t, w.aliceKey, demo, tc.query)
			if status != http.StatusBadRequest {
				t.Fatalf("GET cost?%s = %d %s, want 400", tc.query, status, raw)
			}
			errorType, message := errorOf(t, raw)
			if errorType != "invalid_request" || !strings.Contains(message, tc.param) {
				t.Errorf("GET cost?%s = %s %q, want invalid_request naming %s", tc.query, errorType, message, tc.param)
			}
		})
	}
}

func TestCost_AnswersEveryReaderOfTheWorkspaceInAnyStatusAndNobodyElse(t *testing.T) {
	w := newWorld(t)
	w.clock.Advance(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC).Sub(w.clock.Now()))
	a := w.store(t, stored{workspace: demo, user: w.alice})
	w.spend(t, spent{session: a, workspace: demo, model: "m1", at: "2026-09-02T01:00:00Z", in: 3, out: 2, cacheRead: 1})
	cases := []struct {
		name, credential, slug string
		wantStatus             int
		wantError, wantMessage string
	}{
		{"alice", w.aliceKey, demo, http.StatusOK, "", ""},
		{"bob, a member of the org", w.bobKey, demo, http.StatusOK, "", ""},
		{"the admin token", w.Token, demo, http.StatusOK, "", ""},
		{"a PAT with sessions:write", w.alicePAT, demo, http.StatusOK, "", ""},
		{"a PAT with sessions:read", w.pat(t, w.alice, "", "sessions:read"), demo, http.StatusOK, "", ""},
		{"a PAT with neither sessions scope", w.pat(t, w.alice, "", "workspaces:read"), demo, http.StatusForbidden, "insufficient_scope", ""},
		{"carol, without access", w.carolKey, demo, http.StatusNotFound, "not_found", "workspace not found"},
		{"a slug that names no workspace", w.aliceKey, "nope", http.StatusNotFound, "not_found", "workspace not found"},
	}
	for _, status := range []string{access.StatusReady, access.StatusArchived} {
		w.setStatus(t, demo, status)
		for _, tc := range cases {
			t.Run(status+"/"+tc.name, func(t *testing.T) {
				if tc.wantStatus == http.StatusOK {
					if r := w.costOf(t, tc.credential, ""); !reflect.DeepEqual(r.Totals, sums(3, 2, 1, 1)) {
						t.Errorf("totals = %v, want %v", r.Totals, sums(3, 2, 1, 1))
					}
					return
				}
				code, raw := w.cost(t, tc.credential, tc.slug, "")
				errorType, message := errorOf(t, raw)
				if code != tc.wantStatus || errorType != tc.wantError || tc.wantMessage != "" && message != tc.wantMessage {
					t.Errorf("GET cost = %d %s %q, want %d %s %q", code, errorType, message, tc.wantStatus, tc.wantError, tc.wantMessage)
				}
			})
		}
	}
}
