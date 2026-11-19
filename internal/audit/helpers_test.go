package audit_test

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/session"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/telemetry"
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

// orderedWith is the records names and extra in the audit list's order.
func (w *world) orderedWith(t *testing.T, extra []seeded, names ...string) []key {
	t.Helper()
	recs := slices.Clone(extra)
	for _, n := range names {
		rec, ok := w.records[n]
		if !ok {
			t.Fatalf("no record %s was seeded", n)
		}
		recs = append(recs, rec)
	}
	return keysInOrder(recs)
}

// hubEventsSoFar are the hub events stored so far, as the event log has
// them.
func (w *world) hubEventsSoFar(t *testing.T) []seeded {
	t.Helper()
	var log struct {
		Items []struct {
			ID   string `json:"id"`
			Time string `json:"time"`
		} `json:"items"`
	}
	status, body := w.Call(t, http.MethodGet, "/api/v1/events/log?limit=200", w.Token, nil)
	if status != http.StatusOK || json.Unmarshal(body, &log) != nil {
		t.Fatalf("GET /events/log = %d %s", status, body)
	}
	recs := make([]seeded, 0, len(log.Items))
	for _, it := range log.Items {
		at, err := time.Parse(time.RFC3339, it.Time)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, seeded{key{"hub", it.ID}, at})
	}
	return recs
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

// retentionNow is the time the retention steps run at in the store-level
// tests.
var retentionNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// stores are the hub's two stores, opened in a temporary directory with
// the schemas of the sessions, the run telemetry and the hub events, and
// the components the retention steps act through, bound to them on a
// clock fixed at retentionNow. The session component writes its events to
// events, so that an event a step should not write shows in its bounds.
type stores struct {
	st        *store.Store
	events    *event.Component
	sessions  *session.Component
	telemetry *telemetry.Component
}

func openStores(t *testing.T) *stores {
	t.Helper()
	dir := t.TempDir()
	steps := append([]store.Step{event.SchemaStep, session.SchemaStep}, telemetry.SchemaSteps...)
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, steps)
	if err != nil {
		t.Fatalf("open stores: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ev, err := event.New(event.Deps{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return retentionNow }
	s := &stores{st: st, events: ev, sessions: &session.Component{}, telemetry: &telemetry.Component{}}
	s.sessions.Bind(session.Deps{Store: st, Now: now, Events: ev})
	s.telemetry.Bind(telemetry.Deps{DB: telemetry.SQL(st.Analytics()), Now: now})
	return s
}

// clock is the fixed clock the steps read.
func (s *stores) clock() time.Time { return retentionNow }

// exec runs query on db, failing the test on an error.
func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// session stores a session of the workspace slug with the status, started
// at started.
func (s *stores) session(t *testing.T, id, slug, status string, started time.Time) {
	t.Helper()
	exec(t, s.st.Main(), `
INSERT INTO sessions (id, workspace_slug, status, credential_type, started_at) VALUES (?, ?, ?, 'admin_token', ?)`,
		id, slug, status, started.UTC().Format(time.RFC3339))
}

// usage stores a usage record of the session sessionID, reported at
// reported.
func (s *stores) usage(t *testing.T, id, sessionID, slug string, reported time.Time) {
	t.Helper()
	exec(t, s.st.Main(), `
INSERT INTO session_usage (id, session_id, workspace_slug, model, reported_at) VALUES (?, ?, ?, 'm', ?)`,
		id, sessionID, slug, reported.UTC().Format(time.RFC3339))
}

// runKinds are the tables of the run telemetry kinds other than the
// postmortems, each with the columns a record needs beyond the head.
var runKinds = []struct{ table, columns, values string }{
	{"run_events", "event_type, severity", "'run.start', 'info'"},
	{"session_outcomes", "session_id, status", "'s', 'completed'"},
	{"tool_calls", "tool_name", "'read'"},
	{"tool_errors", "tool_name, error_msg", "'read', 'no such file'"},
	{"traces", "event_type", "'tool.use'"},
}

// telemetryLayout is the form the telemetry store keeps a timestamp in.
const telemetryLayout = "2006-01-02T15:04:05.000000Z"

// record stores a record of the run kind table (one of runKinds) for the
// run runID of the workspace slug, with the timestamp at.
func (s *stores) record(t *testing.T, table, id, slug, runID string, at time.Time) {
	t.Helper()
	for _, k := range runKinds {
		if k.table == table {
			exec(t, s.st.Analytics(), `INSERT INTO `+table+` (id, workspace, run_id, timestamp, ingested_at, `+k.columns+`)
VALUES (?, ?, ?, ?, ?, `+k.values+`)`, id, slug, runID, at.UTC().Format(telemetryLayout), at.UTC().Format(time.RFC3339))
			return
		}
	}
	t.Fatalf("no run kind %s", table)
}

// postmortem stores the postmortem of the run runID of the workspace slug,
// with the timestamp at, ingested at ingested.
func (s *stores) postmortem(t *testing.T, id, slug, runID string, at, ingested time.Time) {
	t.Helper()
	stamp := at.UTC().Format(telemetryLayout)
	exec(t, s.st.Analytics(), `
INSERT INTO postmortems (id, workspace, run_id, timestamp, ingested_at, schema_version, run_status, started_at, completed_at,
	task_summary, cost_summary, blocked_tasks, session_history)
VALUES (?, ?, ?, ?, ?, 1, 'stalled', ?, ?, '{}', '{}', '[]', '[]')`,
		id, slug, runID, stamp, ingested.UTC().Format(time.RFC3339), stamp, stamp)
}

// ids are the ids of table in db, ascending.
func ids(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT id FROM `+table+` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// dump is every row query reads from db with args, each as the text of its columns,
// so that two dumps compare equal only when the rows are the same.
func dump(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%q", vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// snapshot is every row of the sessions, their usage and the run
// telemetry, by table.
func (s *stores) snapshot(t *testing.T) map[string][]string {
	t.Helper()
	snap := map[string][]string{}
	for _, table := range []string{"sessions", "session_usage"} {
		snap[table] = dump(t, s.st.Main(), `SELECT * FROM `+table+` ORDER BY id`)
	}
	for _, k := range runKinds {
		snap[k.table] = dump(t, s.st.Analytics(), `SELECT * FROM `+k.table+` ORDER BY id`)
	}
	snap["postmortems"] = dump(t, s.st.Analytics(), `SELECT * FROM postmortems ORDER BY id`)
	return snap
}
