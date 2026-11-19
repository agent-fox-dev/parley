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

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
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
