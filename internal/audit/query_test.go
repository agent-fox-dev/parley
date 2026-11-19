package audit_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// TestMain runs the package's tests at most hubtest.HubParallel at a time:
// each starts a hub, and hubs contend for one lock past that (see
// hubtest.HubParallel).
func TestMain(m *testing.M) {
	hubtest.LimitParallel(hubtest.HubParallel)
	os.Exit(m.Run())
}

// clock is a settable clock for the hub, which stamps every hub event.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

// key is an item of the audit list as the tests compare it: its source and
// its id.
type key struct{ source, id string }

func (k key) String() string { return k.source + ":" + k.id }

// seeded is a record a test stored, with the time the audit list orders
// it by.
type seeded struct {
	key
	at time.Time
}

// world is a hub on a settable clock and the records a test seeded in it,
// by name: hub events written through the event component, run events
// through the ingestion route.
type world struct {
	*hubtest.Hub
	clock   *clock
	events  *event.Component
	records map[string]seeded
	ids     int
}

// newWorld starts the world's hub, its clock on start.
func newWorld(t *testing.T, start time.Time) *world {
	t.Helper()
	w := &world{clock: &clock{now: start}, records: map[string]seeded{}}
	w.Hub = hubtest.StartWith(t, hubtest.Config{Now: w.clock.Now, Compose: func(c *hub.Composition) {
		w.events = c.Events()
	}})
	return w
}

// hubEvent writes the hub event in at the time at as the record name.
func (w *world) hubEvent(t *testing.T, name string, at time.Time, in event.Input) event.Record {
	t.Helper()
	w.clock.set(at)
	if in.ActorType == "" {
		in.ActorType = event.ActorSystem
	}
	if in.ResourceType == "" {
		in.ResourceType, in.ResourceID = "workspace", "x"
	}
	rec, err := w.events.EmitNow(context.Background(), in)
	if err != nil {
		t.Fatalf("emit %s: %v", name, err)
	}
	w.records[name] = seeded{key{"hub", rec.ID}, rec.Time}
	return rec
}

// runEvent ingests a run event of the run runID into the workspace slug as
// the record name, with the timestamp stamp and the other members of body.
func (w *world) runEvent(t *testing.T, name, slug, runID, stamp string, body map[string]any) {
	t.Helper()
	w.ids++
	id := fmt.Sprintf("0a1b2c3d-0000-4000-8000-%012x", w.ids)
	if body == nil {
		body = map[string]any{}
	}
	body["id"], body["timestamp"] = id, stamp
	if _, ok := body["event_type"]; !ok {
		body["event_type"] = "run.start"
	}
	status, resp := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/runs/"+runID+"/events", w.Token, body)
	if status != http.StatusCreated {
		t.Fatalf("ingest %s = %d %s, want 201", name, status, resp)
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatal(err)
	}
	w.records[name] = seeded{key{"agent", id}, at}
}

// ordered is the records names in the audit list's order: time
// descending, then id descending, then a hub event before a run event.
func (w *world) ordered(t *testing.T, names ...string) []key {
	t.Helper()
	recs := make([]seeded, 0, len(names))
	for _, n := range names {
		rec, ok := w.records[n]
		if !ok {
			t.Fatalf("no record %s was seeded", n)
		}
		recs = append(recs, rec)
	}
	return keysInOrder(recs)
}

// keysInOrder sorts recs into the audit list's order and returns their keys.
func keysInOrder(recs []seeded) []key {
	slices.SortFunc(recs, func(a, b seeded) int {
		return cmp.Or(b.at.Compare(a.at), cmp.Compare(b.id, a.id), cmp.Compare(a.source, b.source)*-1)
	})
	keys := make([]key, len(recs))
	for i, r := range recs {
		keys[i] = r.key
	}
	return keys
}

// page is one answer of GET /api/v1/audit.
type page struct {
	Items      []map[string]json.RawMessage `json:"items"`
	NextCursor *string                      `json:"next_cursor"`
}

// audit gets /api/v1/audit with query as credential, failing unless it
// answers 200.
func (w *world) audit(t *testing.T, credential string, query url.Values) page {
	t.Helper()
	status, body := w.Call(t, http.MethodGet, "/api/v1/audit?"+query.Encode(), credential, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /audit?%s = %d %s, want 200", query.Encode(), status, body)
	}
	var p page
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if p.Items == nil {
		t.Fatalf("GET /audit?%s: items is not a list: %s", query.Encode(), body)
	}
	return p
}

// follow pages through /api/v1/audit with query as credential, limit items
// at a time (0: the default), and returns every page.
func (w *world) follow(t *testing.T, credential string, query url.Values, limit int) []page {
	t.Helper()
	q := maps.Clone(query)
	if q == nil {
		q = url.Values{}
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var pages []page
	for len(pages) < 100 {
		p := w.audit(t, credential, q)
		pages = append(pages, p)
		if p.NextCursor == nil {
			return pages
		}
		q.Set("cursor", *p.NextCursor)
	}
	t.Fatalf("GET /audit?%s did not end within 100 pages", query.Encode())
	return nil
}

// keysOf are the keys of the items of pages, in order.
func keysOf(t *testing.T, pages ...page) []key {
	t.Helper()
	var keys []key
	for _, p := range pages {
		for _, it := range p.Items {
			var k key
			if json.Unmarshal(it["source"], &k.source) != nil || json.Unmarshal(it["id"], &k.id) != nil {
				t.Fatalf("an item without a source and an id: %v", it)
			}
			keys = append(keys, k)
		}
	}
	return keys
}

// errorOf is the error_type of an error envelope.
func errorOf(t *testing.T, raw []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode error envelope %s: %v", raw, err)
	}
	return env.Error.ErrorType
}

// itemMembers are the members of every item of the audit list.
var itemMembers = []string{
	"source", "id", "type", "time", "workspace", "session_id", "seq", "actor_type", "actor_id", "resource_type",
	"resource_id", "action", "metadata", "run_id", "node_id", "severity", "payload",
}

// TestAuditQuery runs the audit query over one hub that holds hub events
// and run events of several workspaces, deleted ones included, and callers
// who see different parts of them.
func TestAuditQuery(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	stamp := func(n int) string { return sec(n).Format(time.RFC3339) }
	w := newWorld(t, t0)

	alice, bob, carol := w.CreateUser(t, "alice"), w.CreateUser(t, "bob"), w.CreateUser(t, "carol")
	dave, erin, frank := w.CreateUser(t, "dave"), w.CreateUser(t, "erin"), w.CreateUser(t, "frank")
	aliceKey, bobKey, carolKey := w.IssueKey(t, alice), w.IssueKey(t, bob), w.IssueKey(t, carol)
	erinKey, frankKey := w.IssueKey(t, erin), w.IssueKey(t, frank)
	acme, beta := w.CreateOrg(t, "acme"), w.CreateOrg(t, "beta")
	w.AddMember(t, acme, alice)
	w.AddMember(t, acme, bob)
	repo := func(slug, owner, org string) access.Record {
		return access.Record{Slug: slug, OwnerID: owner, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady}
	}
	w.AddWorkspace(t, repo("ws1", alice, acme))
	w.AddWorkspace(t, repo("ws2", dave, beta))
	w.AddWorkspace(t, repo("ws3", alice, acme))
	w.AddWorkspace(t, repo("ws4", erin, beta))
	w.AddWorkspace(t, access.Record{Slug: "ws1--gone", OwnerID: alice, OrgID: acme, Kind: access.KindFork, ParentSlug: "ws1", Status: access.StatusReady})
	ws := func(slug string) *string { return &slug }
	const runA, runB = "20260601_120000_aaaaaa", "20260601_120000_bbbbbb"
	const session = "5e551011-0000-4000-8000-000000000001"

	// The merge: hub events and run events of ws1 whose times interleave;
	// h2, a3 and a4 share a second, and so do h3 and h4.
	shapeEvent := w.hubEvent(t, "h1", sec(0), event.Input{
		Type: "parley.workspace.sync", Workspace: ws("ws1"), ActorType: event.ActorAPIKey, ActorID: &alice,
		ResourceType: "workspace", ResourceID: "ws1", SessionID: ptr(session), Metadata: map[string]any{"commit": "abc"},
	})
	w.runEvent(t, "a1", "ws1", runA, "2026-06-01T13:59:58.123456+02:00",
		map[string]any{"event_type": "tool.started", "payload": map[string]any{"k": "v"}})
	w.runEvent(t, "a2", "ws1", runA, "2026-06-01T12:00:01.5Z", nil)
	w.hubEvent(t, "h2", sec(2), event.Input{Type: "parley.change.open", Workspace: ws("ws1")})
	w.runEvent(t, "a3", "ws1", runA, stamp(2), nil)
	w.runEvent(t, "a4", "ws1", runA, stamp(2), nil)
	w.hubEvent(t, "h3", sec(3), event.Input{Type: "parley.change.land", Workspace: ws("ws1")})
	w.hubEvent(t, "h4", sec(3), event.Input{Type: "parley.change.land", Workspace: ws("ws1")})
	merge := []string{"h1", "a1", "a2", "h2", "a3", "a4", "h3", "h4"}

	// What callers see: an owner-only event of ws1, records of ws2, events
	// of no workspace in the scope of a user or of an org, records of the
	// fork ws1--gone and of ws3, both deleted after, and a ping.
	w.hubEvent(t, "secret", sec(60), event.Input{Type: "parley.secret.set", Workspace: ws("ws1")})
	w.hubEvent(t, "ws2", sec(61), event.Input{Type: "parley.change.open", Workspace: ws("ws2")})
	w.hubEvent(t, "user-alice", sec(62), event.Input{Type: "parley.token.created", Metadata: map[string]any{"scope": "user", "scope_id": alice}})
	w.hubEvent(t, "user-carol", sec(63), event.Input{Type: "parley.token.created", Metadata: map[string]any{"scope": "user", "scope_id": carol}})
	w.hubEvent(t, "org-acme", sec(64), event.Input{Type: "parley.org.updated", Metadata: map[string]any{"scope": "org", "scope_id": acme}})
	w.hubEvent(t, "gone", sec(65), event.Input{Type: "parley.change.open", Workspace: ws("ws1--gone")})
	ping := w.hubEvent(t, "ping", sec(66), event.Input{Type: "parley.webhook.ping", Workspace: ws("ws1")})
	w.runEvent(t, "run-ws2", "ws2", runB, stamp(67), nil)
	w.runEvent(t, "run-gone", "ws1--gone", runB, stamp(68), nil)
	w.runEvent(t, "run-ws3", "ws3", runB, stamp(69), nil)
	for _, slug := range []string{"ws1--gone", "ws3"} {
		if _, err := w.Store.Main().Exec(`DELETE FROM workspaces WHERE slug = ?`, slug); err != nil {
			t.Fatal(err)
		}
	}

	// The time range: one hub event and one run event at each of T1 < T2 <
	// T3, in ws4, which erin alone reads.
	for i, n := range []int{120, 130, 140} {
		w.hubEvent(t, fmt.Sprintf("rh%d", i+1), sec(n), event.Input{Type: "parley.change.open", Workspace: ws("ws4")})
		w.runEvent(t, fmt.Sprintf("ra%d", i+1), "ws4", runA, stamp(n), nil)
	}

	// adminView is every stored record but the ping: the hub events of the
	// log, those the hub wrote itself included, and the run events seeded.
	var adminView []seeded
	var log struct {
		Items []struct {
			ID   string `json:"id"`
			Time string `json:"time"`
		} `json:"items"`
	}
	if status, body := w.Call(t, http.MethodGet, "/api/v1/events/log?limit=200", w.Token, nil); status != http.StatusOK || json.Unmarshal(body, &log) != nil {
		t.Fatalf("GET /events/log = %d %s", status, body)
	}
	for _, it := range log.Items {
		if it.ID == ping.ID {
			continue
		}
		at, err := time.Parse(time.RFC3339, it.Time)
		if err != nil {
			t.Fatal(err)
		}
		adminView = append(adminView, seeded{key{"hub", it.ID}, at})
	}
	for _, rec := range w.records {
		if rec.source == "agent" {
			adminView = append(adminView, rec)
		}
	}
	all := keysInOrder(adminView)

	t.Run("the list merges both sources newest first, then by id", func(t *testing.T) {
		if got := keysOf(t, w.audit(t, w.Token, url.Values{"limit": {"200"}})); !slices.Equal(got, all) {
			t.Errorf("admin: %v,\nwant %v", got, all)
		}
		if got := keysOf(t, w.follow(t, aliceKey, nil, 0)...); !slices.Equal(got[len(got)-len(merge):], w.ordered(t, merge...)) {
			t.Errorf("alice: %v ends otherwise than %v", got, w.ordered(t, merge...))
		}
		empty := w.audit(t, frankKey, nil)
		if len(empty.Items) != 0 || empty.NextCursor != nil {
			t.Errorf("a caller with nothing to see: %+v, want {items: [], next_cursor: null}", empty)
		}
		writer := w.IssuePAT(t, identity.PATSpec{UserID: erin, Permissions: []string{"audit:write"}})
		if got, want := keysOf(t, w.audit(t, writer, nil)), w.ordered(t, "rh1", "ra1", "rh2", "ra2", "rh3", "ra3"); !slices.Equal(got, want) {
			t.Errorf("a PAT holding audit:write: %v, want %v", got, want)
		}
	})

	t.Run("every item has the seventeen members, those of the other source null", func(t *testing.T) {
		items := map[key]map[string]json.RawMessage{}
		for _, it := range w.audit(t, w.Token, url.Values{"limit": {"200"}}).Items {
			if got := slices.Sorted(maps.Keys(it)); !slices.Equal(got, slices.Sorted(slices.Values(itemMembers))) {
				t.Errorf("item members %v, want %v", got, itemMembers)
			}
			items[keysOf(t, page{Items: []map[string]json.RawMessage{it}})[0]] = it
		}
		hubItem, agentItem := items[w.records["h1"].key], items[w.records["a1"].key]
		if hubItem == nil || agentItem == nil {
			t.Fatalf("the seeded hub event or run event is missing: %v", items)
		}
		var logged map[string]json.RawMessage
		if raw, err := json.Marshal(shapeEvent); err != nil || json.Unmarshal(raw, &logged) != nil {
			t.Fatalf("encode the event record: %v", err)
		}
		for _, m := range []string{"type", "time", "workspace", "session_id", "seq", "actor_type", "actor_id", "resource_type", "resource_id", "action", "metadata"} {
			if !jsonEqual(hubItem[m], logged[m]) {
				t.Errorf("hub item %s = %s, want %s as the event log has it", m, hubItem[m], logged[m])
			}
		}
		for m, want := range map[string]string{
			"source": `"hub"`, "run_id": "null", "node_id": "null", "severity": "null", "payload": "null",
		} {
			if !jsonEqual(hubItem[m], json.RawMessage(want)) {
				t.Errorf("hub item %s = %s, want %s", m, hubItem[m], want)
			}
		}
		for m, want := range map[string]string{
			"source": `"agent"`, "type": `"tool.started"`, "time": `"2026-06-01T11:59:58.123456Z"`, "workspace": `"ws1"`,
			"session_id": "null", "payload": `{"k":"v"}`, "severity": `"info"`, "run_id": `"` + runA + `"`, "node_id": "null",
			"seq": "null", "actor_type": "null", "actor_id": "null", "resource_type": "null", "resource_id": "null",
			"action": "null", "metadata": "null",
		} {
			if !jsonEqual(agentItem[m], json.RawMessage(want)) {
				t.Errorf("agent item %s = %s, want %s", m, agentItem[m], want)
			}
		}
	})

	t.Run("pages join into the whole list at any limit", func(t *testing.T) {
		for _, limit := range []int{1, 2, 3, 6, len(all)} {
			pages := w.follow(t, w.Token, nil, limit)
			if got := keysOf(t, pages...); !slices.Equal(got, all) {
				t.Errorf("limit %d: %v,\nwant %v", limit, got, all)
			}
			if want := (len(all) + limit - 1) / limit; len(pages) != want {
				t.Errorf("limit %d: %d pages, want %d with next_cursor null on the last", limit, len(pages), want)
			}
		}
	})

	t.Run("a caller sees only the records the visibility rule and the readable set allow", func(t *testing.T) {
		ws1 := append([]string{"secret"}, merge...)
		bound := w.IssuePAT(t, identity.PATSpec{UserID: alice, Permissions: []string{"audit:read"}, Workspace: "ws1"})
		cases := []struct {
			caller, credential string
			want               []key
		}{
			{"admin", w.Token, all},
			{"owner of ws1", aliceKey, w.ordered(t, append(ws1, "user-alice", "org-acme", "gone", "run-gone")...)},
			{"member of ws1's org", bobKey, w.ordered(t, append(merge, "org-acme", "gone", "run-gone")...)},
			{"outsider", carolKey, w.ordered(t, "user-carol")},
			{"token bound to ws1", bound, w.ordered(t, ws1...)},
		}
		for _, c := range cases {
			got := keysOf(t, w.follow(t, c.credential, nil, 4)...)
			if !slices.Equal(got, c.want) {
				t.Errorf("%s: %v,\nwant %v", c.caller, got, c.want)
			}
			if slices.Contains(got, w.records["ping"].key) {
				t.Errorf("%s sees parley.webhook.ping", c.caller)
			}
		}
	})

	t.Run("since and until select a half-open range of both sources", func(t *testing.T) {
		t1, t2, t3 := stamp(120), stamp(130), stamp(140)
		cases := []struct {
			query url.Values
			want  []string
		}{
			{url.Values{"since": {t2}}, []string{"rh2", "ra2", "rh3", "ra3"}},
			{url.Values{"until": {t2}}, []string{"rh1", "ra1"}},
			{url.Values{"since": {t1}, "until": {t3}}, []string{"rh1", "ra1", "rh2", "ra2"}},
			{url.Values{"since": {sec(130).In(time.FixedZone("", -5*3600)).Format(time.RFC3339)}}, []string{"rh2", "ra2", "rh3", "ra3"}},
		}
		for _, c := range cases {
			if got, want := keysOf(t, w.audit(t, erinKey, c.query)), w.ordered(t, c.want...); !slices.Equal(got, want) {
				t.Errorf("%s: %v, want %v", c.query.Encode(), got, want)
			}
		}
	})

	t.Run("a caller without a credential or the scope is refused", func(t *testing.T) {
		noScope := w.IssuePAT(t, identity.PATSpec{UserID: alice, Permissions: []string{"sessions:read"}})
		cases := []struct {
			caller, credential string
			status             int
			errorType          string
		}{
			{"no credential", "", http.StatusUnauthorized, "unauthenticated"},
			{"a PAT holding sessions:read only", noScope, http.StatusForbidden, "insufficient_scope"},
		}
		for _, c := range cases {
			status, body := w.Call(t, http.MethodGet, "/api/v1/audit", c.credential, nil)
			if status != c.status || errorOf(t, body) != c.errorType {
				t.Errorf("%s: %d %s, want %d %s", c.caller, status, body, c.status, c.errorType)
			}
		}
	})
}

func ptr[T any](v T) *T { return &v }

// jsonEqual reports whether a and b are the same JSON value.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(ax) == string(by)
}
