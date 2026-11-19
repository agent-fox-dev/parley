package audit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/audit"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// TestAuditFilters runs the filters of the audit query over one hub that
// holds hub events and run events of a workspace, its fork, a deleted fork
// and a deleted workspace.
func TestAuditFilters(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	stamp := func(n int) string { return sec(n).Format(time.RFC3339) }
	w := newWorld(t, t0)

	alice, bob, carol, dave := w.CreateUser(t, "alice"), w.CreateUser(t, "bob"), w.CreateUser(t, "carol"), w.CreateUser(t, "dave")
	aliceKey, carolKey := w.IssueKey(t, alice), w.IssueKey(t, carol)
	acme := w.CreateOrg(t, "acme")
	repo := func(slug, owner string) access.Record {
		return access.Record{Slug: slug, OwnerID: owner, OrgID: acme, Kind: access.KindRepository, Status: access.StatusReady}
	}
	fork := func(slug string) access.Record {
		return access.Record{Slug: slug, OwnerID: alice, OrgID: acme, Kind: access.KindFork, ParentSlug: "ws1", Status: access.StatusReady}
	}
	for _, rec := range []access.Record{repo("ws1", alice), repo("ws2", dave), repo("own2", alice), repo("ws3", alice), fork("ws1--f1"), fork("ws1--gone")} {
		w.AddWorkspace(t, rec)
	}
	// Bound once ws1 exists, so that the token binding sweep of the hub's
	// first maintenance pass, whenever it runs, finds the binding alive.
	bound := w.IssuePAT(t, identity.PATSpec{UserID: alice, Permissions: []string{"audit:read"}, Workspace: "ws1"})

	// The hub events the hub wrote while it was set up are in every list
	// that selects all hub events.
	setup := w.hubEventsSoFar(t)

	const runA, runB = "20260601_120000_aaaaaa", "20260601_120000_bbbbbb"
	const session = "5e551011-0000-4000-8000-00000000000a"
	ws := func(slug string) *string { return &slug }
	w.hubEvent(t, "hl", sec(10), event.Input{Type: "parley.change.land", Workspace: ws("ws1"), ResourceType: "change", ResourceID: "c1", SessionID: ptr(session)})
	w.hubEvent(t, "ho", sec(11), event.Input{Type: "parley.change.open", Workspace: ws("ws1"), ActorType: event.ActorAPIKey, ActorID: &alice, ResourceType: "change", ResourceID: "c2"})
	w.hubEvent(t, "hs", sec(12), event.Input{Type: "parley.workspace.sync", Workspace: ws("ws1"), ActorType: event.ActorPAT, ActorID: &bob, ResourceType: "workspace", ResourceID: "ws1"})
	w.hubEvent(t, "hf", sec(13), event.Input{Type: "parley.change.land", Workspace: ws("ws1--f1"), ActorType: event.ActorForge, ResourceType: "change", ResourceID: "c3"})
	w.hubEvent(t, "hgone", sec(14), event.Input{Type: "parley.change.open", Workspace: ws("ws1--gone"), ResourceType: "change", ResourceID: "c4"})
	w.hubEvent(t, "h3", sec(15), event.Input{Type: "parley.workspace.sync", Workspace: ws("ws3"), ResourceType: "workspace", ResourceID: "ws3"})
	w.runEvent(t, "at", "ws1", runA, stamp(20), map[string]any{"event_type": "tool.started", "node_id": "n1", "session_id": session})
	w.runEvent(t, "af", "ws1", runA, stamp(21), map[string]any{"event_type": "session.fail", "node_id": "n2"})
	w.runEvent(t, "ab", "ws1", runB, stamp(22), map[string]any{"event_type": "tool.started", "node_id": "n1", "severity": "warning"})
	w.runEvent(t, "aff", "ws1--f1", runA, stamp(23), map[string]any{"event_type": "tool.started", "node_id": "n3", "severity": "critical"})
	w.runEvent(t, "agone", "ws1--gone", runB, stamp(24), nil)
	w.runEvent(t, "a3", "ws3", runB, stamp(25), nil)
	for _, slug := range []string{"ws1--gone", "ws3"} {
		if _, err := w.Store.Main().Exec(`DELETE FROM workspaces WHERE slug = ?`, slug); err != nil {
			t.Fatal(err)
		}
	}
	hubs := []string{"hl", "ho", "hs", "hf", "hgone", "h3"}
	agents := []string{"at", "af", "ab", "aff", "agone", "a3"}

	// check asserts that the audit list for each row's query, as the admin
	// token sees it, holds the row's records, plus the setup's hub events
	// where the row says so.
	type row struct {
		query      string
		want       []string
		withSetup  bool
		sourceOnly string
	}
	check := func(t *testing.T, rows []row) {
		t.Helper()
		for _, r := range rows {
			query, err := url.ParseQuery(r.query)
			if err != nil {
				t.Fatal(err)
			}
			got := keysOf(t, w.follow(t, w.Token, query, 0)...)
			var extra []seeded
			if r.withSetup {
				extra = setup
			}
			if want := w.orderedWith(t, extra, r.want...); !slices.Equal(got, want) {
				t.Errorf("%s: %v,\nwant %v", r.query, got, want)
			}
			for _, k := range got {
				if r.sourceOnly != "" && k.source != r.sourceOnly {
					t.Errorf("%s: an item of source %s, want %s only", r.query, k.source, r.sourceOnly)
					break
				}
			}
		}
	}

	t.Run("source, workspace, types and session_id narrow both sources", func(t *testing.T) {
		check(t, []row{
			{query: "source=hub", want: hubs, withSetup: true},
			{query: "source=agent", want: agents},
			{query: "workspace=ws1", want: []string{"hl", "ho", "hs", "at", "af", "ab"}},
			{query: "types=parley.change.land", want: []string{"hl", "hf"}},
			{query: "types=parley.change.*", want: []string{"hl", "ho", "hf", "hgone"}},
			{query: "types=parley.change.*,tool.started", want: []string{"hl", "ho", "hf", "hgone", "at", "ab", "aff"}},
			{query: "types=*", want: append(slices.Clone(hubs), agents...), withSetup: true},
			{query: "types=parley.nothing.here"},
			{query: "session_id=" + strings.ToUpper(session), want: []string{"hl", "at"}},
			{query: "workspace=ws1&source=agent&types=tool.*", want: []string{"at", "ab"}},
		})
	})

	t.Run("a hub-only filter selects hub events and no run event", func(t *testing.T) {
		rows := []row{
			{query: "actor_id=" + alice, want: []string{"ho"}},
			{query: "actor_type=system", want: []string{"hl", "hgone", "h3"}},
			{query: "resource_type=workspace", want: []string{"hs", "h3"}},
			{query: "resource_id=c2", want: []string{"ho"}},
			{query: "action=land", want: []string{"hl", "hf"}},
			{query: "actor_type=system&action=land", want: []string{"hl"}},
		}
		for _, r := range slices.Clone(rows) {
			r.query += "&source=hub"
			rows = append(rows, r)
		}
		for i := range rows {
			rows[i].sourceOnly = "hub"
		}
		check(t, rows)
	})

	t.Run("an agent-only filter selects run events and no hub event", func(t *testing.T) {
		rows := []row{
			{query: "run_id=" + runA, want: []string{"at", "af", "aff"}},
			{query: "node_id=n1", want: []string{"at", "ab"}},
			{query: "severity=error", want: []string{"af"}},
			{query: "severity=critical&source=agent", want: []string{"aff"}},
		}
		for i := range rows {
			rows[i].sourceOnly = "agent"
		}
		check(t, rows)
		status, body := w.Call(t, http.MethodGet, "/api/v1/audit?run_id="+runA+"&action=land", w.Token, nil)
		if status != http.StatusOK || !jsonEqual(body, json.RawMessage(`{"items": [], "next_cursor": null}`)) {
			t.Errorf("an agent-only and a hub-only filter: %d %s, want 200 {\"items\": [], \"next_cursor\": null}", status, body)
		}
	})

	t.Run("a workspace filter the caller may not read is refused as not found", func(t *testing.T) {
		cases := []struct {
			caller, credential, slug string
			want                     []string // nil: refused
		}{
			{"outsider", carolKey, "ws1", nil},
			{"outsider", carolKey, "nope", nil},
			{"owner of ws1", aliceKey, "ws2", nil},
			{"token bound to ws1", bound, "own2", nil},
			{"owner of ws1", aliceKey, "ws1", []string{"hl", "ho", "hs", "at", "af", "ab"}},
			{"owner of ws1", aliceKey, "ws1--gone", []string{"hgone", "agone"}},
			{"admin", w.Token, "ws3", []string{"h3", "a3"}},
			{"admin", w.Token, "nope", []string{}},
		}
		var refusal []byte
		for _, c := range cases {
			status, body := w.Call(t, http.MethodGet, "/api/v1/audit?workspace="+c.slug, c.credential, nil)
			if c.want == nil {
				if status != http.StatusNotFound || errorOf(t, body) != "not_found" || !strings.Contains(string(body), `"workspace not found"`) {
					t.Errorf("%s, %s: %d %s, want 404 not_found workspace not found", c.caller, c.slug, status, body)
				}
				if refusal != nil && string(body) != string(refusal) {
					t.Errorf("%s, %s: body %s differs from %s", c.caller, c.slug, body, refusal)
				}
				refusal = body
				continue
			}
			if status != http.StatusOK {
				t.Errorf("%s, %s: %d %s, want 200", c.caller, c.slug, status, body)
				continue
			}
			if got, want := keysOf(t, w.audit(t, c.credential, url.Values{"workspace": {c.slug}})), w.ordered(t, c.want...); !slices.Equal(got, want) {
				t.Errorf("%s, %s: %v, want %v", c.caller, c.slug, got, want)
			}
		}
	})
}

// TestAuditQuery_RefusesAnInvalidParameterBeforeAnyRead serves the audit
// query on stores that count their calls and sends it every malformed
// parameter and every filter that contradicts source.
func TestAuditQuery_RefusesAnInvalidParameterBeforeAnyRead(t *testing.T) {
	t.Parallel()
	calls := &callCount{}
	comp := &audit.Component{}
	comp.Bind(audit.Deps{Hub: countingHub{calls}, Agent: countingAgent{calls}})
	srv := server.New(server.Options{RateLimitDisabled: true})
	for _, r := range comp.Routes() {
		srv.Register(r)
	}
	srv.SetAuthenticator(func(*http.Request) (server.Caller, error) {
		return server.Caller{CredentialID: "admin", Admin: true}, nil
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	get := func(query string) (int, string, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/v1/audit?" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var env struct {
			Error struct {
				ErrorType string `json:"error_type"`
				Message   string `json:"message"`
			} `json:"error"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
		if env.NextCursor != nil {
			return resp.StatusCode, "", *env.NextCursor
		}
		return resp.StatusCode, env.Error.ErrorType + ": " + env.Error.Message, ""
	}

	_, _, cursor := get("workspace=ws1&limit=1")
	if cursor == "" {
		t.Fatal("no cursor for workspace=ws1&limit=1")
	}
	const t1, t2 = "2026-06-01T12:00:00Z", "2026-06-01T13:00:00Z"
	types21 := strings.TrimSuffix(strings.Repeat("parley.a.b,", 21), ",")
	cases := []struct{ query, parameter string }{
		{"source=agent&actor_id=u1", "actor_id"},
		{"source=agent&actor_type=system", "actor_type"},
		{"source=agent&resource_type=change", "resource_type"},
		{"source=agent&resource_id=c1", "resource_id"},
		{"source=agent&action=land", "action"},
		{"source=hub&run_id=20260601_120000_aaaaaa", "run_id"},
		{"source=hub&node_id=n1", "node_id"},
		{"source=hub&severity=error", "severity"},
		{"source=both", "source"},
		{"actor_type=user", "actor_type"},
		{"severity=fatal", "severity"},
		{"session_id=123", "session_id"},
		{"run_id=20260601", "run_id"},
		{"since=noon", "since"},
		{"until=2026-13-01T00:00:00Z", "until"},
		{"since=" + t1 + "&until=" + t1, "since"},
		{"since=" + t2 + "&until=" + t1, "since"},
		{"types=" + types21, "types"},
		{"types=a,,b", "types"},
		{"types=parley.*.land", "types"},
		{"types=*x", "types"},
		{"limit=0", "limit"},
		{"limit=201", "limit"},
		{"cursor=!!!", "cursor"},
		{"workspace=ws2&cursor=" + url.QueryEscape(cursor), "cursor"},
	}
	calls.reset()
	for _, c := range cases {
		status, msg, _ := get(c.query)
		if status != http.StatusBadRequest || !strings.HasPrefix(msg, "invalid_request: ") || !strings.Contains(msg, c.parameter) {
			t.Errorf("%s: %d %q, want 400 invalid_request naming %s", c.query, status, msg, c.parameter)
		}
		if n := calls.reset(); n != 0 {
			t.Errorf("%s: the stores saw %d reads, want none", c.query, n)
		}
	}
	for _, query := range []string{"types=" + strings.TrimSuffix(strings.Repeat("parley.a.b,", 20), ","), "foo=1"} {
		if status, msg, _ := get(query); status != http.StatusOK {
			t.Errorf("%s: %d %s, want 200", query, status, msg)
		}
	}
}

// callCount counts the reads of the stores of the audit query.
type callCount struct {
	mu sync.Mutex
	n  int
}

func (c *callCount) add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

// reset returns the count and starts it again from 0.
func (c *callCount) reset() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.n
	c.n = 0
	return n
}

// countingHub is a hub event store that counts its calls and holds two
// events, so that a page of one has a next cursor.
type countingHub struct{ calls *callCount }

func (h countingHub) ReadNewest(_ context.Context, _ event.Caller, q event.NewestQuery) ([]event.Record, error) {
	h.calls.add()
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	recs := []event.Record{
		{Seq: 2, ID: "b", Type: "parley.a.b", Time: at, ActorType: event.ActorSystem, ResourceType: "x", ResourceID: "x", Action: "b"},
		{Seq: 1, ID: "a", Type: "parley.a.b", Time: at, ActorType: event.ActorSystem, ResourceType: "x", ResourceID: "x", Action: "b"},
	}
	return recs[:min(len(recs), q.Limit)], nil
}

func (h countingHub) CheckWorkspace(context.Context, event.Caller, string) error {
	h.calls.add()
	return nil
}

// countingAgent is a run event store that counts its calls and holds none.
type countingAgent struct{ calls *callCount }

func (a countingAgent) NewestRunEvents(context.Context, telemetry.RunEventQuery) ([]telemetry.RunEventRecord, error) {
	a.calls.add()
	return nil, nil
}
