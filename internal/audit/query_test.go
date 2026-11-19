package audit_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// itemMembers are the members of every item of the audit list.
var itemMembers = []string{
	"source", "id", "type", "time", "workspace", "session_id", "seq", "actor_type", "actor_id", "resource_type",
	"resource_id", "action", "metadata", "run_id", "node_id", "severity", "payload",
}

// queryWorld is the hub of TestAuditQuery and what it seeded.
type queryWorld struct {
	*world
	// sec is the time n seconds after the hub's start.
	sec func(n int) time.Time
	// stamp is the text of sec(n).
	stamp func(n int) string

	alice, erin, runA                             string
	aliceKey, bobKey, carolKey, erinKey, frankKey string
	// shapeEvent is the hub event whose item's members are checked.
	shapeEvent event.Record
	// merge are the records of ws1 whose times interleave, in time order.
	merge []string
	// all is every record as the admin lists them.
	all []key
}

// newQueryWorld starts the hub of TestAuditQuery and seeds it.
func newQueryWorld(t *testing.T) *queryWorld {
	t.Helper()
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

	all := queryAdminView(t, w, ping)

	return &queryWorld{
		world: w, sec: sec, stamp: stamp, alice: alice, erin: erin,
		aliceKey: aliceKey, bobKey: bobKey, carolKey: carolKey, erinKey: erinKey, frankKey: frankKey,
		runA: runA, shapeEvent: shapeEvent, merge: merge, all: all,
	}
}

// queryAdminView is every stored record but the ping, in the list's order:
// the hub events of the log, those the hub wrote itself included, and the
// run events seeded.
func queryAdminView(t *testing.T, w *world, ping event.Record) []key {
	t.Helper()
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
	return keysInOrder(adminView)
}

// TestAuditQuery runs the audit query over one hub that holds hub events
// and run events of several workspaces, deleted ones included, and callers
// who see different parts of them.
func TestAuditQuery(t *testing.T) {
	t.Parallel()
	q := newQueryWorld(t)

	t.Run("the list merges both sources newest first, then by id", func(t *testing.T) { queryMergesBothSources(t, q) })
	t.Run("every item has the seventeen members, those of the other source null", func(t *testing.T) { queryItemMembers(t, q) })
	t.Run("pages join into the whole list at any limit", func(t *testing.T) { queryPagesJoin(t, q) })
	t.Run("a caller sees only the records the visibility rule and the readable set allow", func(t *testing.T) { queryVisibility(t, q) })
	t.Run("since and until select a half-open range of both sources", func(t *testing.T) { queryTimeRange(t, q) })
	t.Run("a caller without a credential or the scope is refused", func(t *testing.T) { queryRefusals(t, q) })
}

// queryMergesBothSources lists the records as the admin, an owner, a
// caller with nothing to see and a PAT holding audit:write.
func queryMergesBothSources(t *testing.T, q *queryWorld) {
	w, aliceKey, frankKey, erin, merge, all := q.world, q.aliceKey, q.frankKey, q.erin, q.merge, q.all
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
}

// queryItemMembers reads the members of a hub item and of an agent item.
func queryItemMembers(t *testing.T, q *queryWorld) {
	w, shapeEvent, runA := q.world, q.shapeEvent, q.runA
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
}

// queryPagesJoin pages through the whole list at several limits.
func queryPagesJoin(t *testing.T, q *queryWorld) {
	w, all := q.world, q.all
	for _, limit := range []int{1, 2, 3, 6, len(all)} {
		pages := w.follow(t, w.Token, nil, limit)
		if got := keysOf(t, pages...); !slices.Equal(got, all) {
			t.Errorf("limit %d: %v,\nwant %v", limit, got, all)
		}
		if want := (len(all) + limit - 1) / limit; len(pages) != want {
			t.Errorf("limit %d: %d pages, want %d with next_cursor null on the last", limit, len(pages), want)
		}
	}
}

// queryVisibility lists the records as callers who see different parts
// of them.
func queryVisibility(t *testing.T, q *queryWorld) {
	w, alice, aliceKey, bobKey, carolKey, merge, all := q.world, q.alice, q.aliceKey, q.bobKey, q.carolKey, q.merge, q.all
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
}

// queryTimeRange lists the records of ws4 through since and until.
func queryTimeRange(t *testing.T, q *queryWorld) {
	w, sec, stamp, erinKey := q.world, q.sec, q.stamp, q.erinKey
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
}

// queryRefusals asks for the list without a credential and without the
// scope.
func queryRefusals(t *testing.T, q *queryWorld) {
	w, alice := q.world, q.alice
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
}
