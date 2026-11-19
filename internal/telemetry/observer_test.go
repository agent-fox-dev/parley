package telemetry_test

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// observer is a run event observer that keeps the event types it is told,
// and, for each, whether the analytical store db already held a committed
// run event of that type when it was told.
type observer struct {
	db        *sql.DB
	mu        sync.Mutex
	types     []string
	unstored  []string
	readFails []error
}

// observe is the observer's function, as OnRunEvent takes it.
func (o *observer) observe(eventType string) {
	var n int
	err := o.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM run_events WHERE event_type = ?`, eventType).Scan(&n)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.types = append(o.types, eventType)
	switch {
	case err != nil:
		o.readFails = append(o.readFails, err)
	case n == 0:
		o.unstored = append(o.unstored, eventType)
	}
}

// told is the number of calls so far, and since(n) the types told after
// the first n calls, sorted.
func (o *observer) told() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.types)
}

func (o *observer) since(n int) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	got := slices.Clone(o.types[n:])
	slices.Sort(got)
	return got
}

// early are the types the observer was told before a run event of the type
// was stored, and the failures of its reads of the store.
func (o *observer) early() (unstored []string, readFails []error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.unstored), slices.Clone(o.readFails)
}

// TestRunEventObservers registers two observers on the hub's telemetry
// component, whose session lookup knows s1 (a session of someRunID) and s2
// (a session of no run), with a write limit short enough for a blocked write
// to end quickly.
func TestRunEventObservers(t *testing.T) {
	t.Parallel()
	w := newWorld(t, func(d *telemetry.Deps) {
		d.Sessions = knownSessions
		d.WriteTimeout = 250 * time.Millisecond
	})
	first, second := &observer{db: w.Store.Analytics()}, &observer{db: w.Store.Analytics()}
	w.telemetry.OnRunEvent(first.observe)
	w.telemetry.OnRunEvent(second.observe)

	t.Run("each observer is told the type of every newly stored run event once, after it is stored", func(t *testing.T) {
		n1, n2 := first.told(), second.told()
		run := w.freshRun()
		if status, body := w.post(t, w.alicePAT, demo, run, "/events", map[string]any{"event_type": "run.start"}); status != http.StatusCreated {
			t.Fatalf("single run event: status = %d %s, want 201", status, body)
		}
		items := []any{map[string]any{"event_type": "a"}, map[string]any{"event_type": "b"}, map[string]any{"event_type": "a"}}
		if status, body := w.post(t, w.alicePAT, demo, run, "/events/batch", items); status != http.StatusOK {
			t.Fatalf("batch: status = %d %s, want 200", status, body)
		}
		w.telemetry.RecordSandboxUse(context.Background(), telemetry.SandboxExec{SandboxUse: useOf(s1), Command: []string{"go", "test"}, DurationMS: 10})

		want := []string{"a", "a", "b", "run.start", "sandbox.exec"}
		for _, o := range []struct {
			name string
			obs  *observer
			from int
		}{{"first", first, n1}, {"second", second, n2}} {
			if got := o.obs.since(o.from); !reflect.DeepEqual(got, want) {
				t.Errorf("%s observer was told %v, want %v", o.name, got, want)
			}
			if unstored, fails := o.obs.early(); len(unstored) != 0 || len(fails) != 0 {
				t.Errorf("%s observer was told %v before they were stored (store reads failed: %v)", o.name, unstored, fails)
			}
		}
	})

	t.Run("no observer is told of a request that stores no new run event, or of a record of another kind", func(t *testing.T) {
		run := w.freshRun()
		u1 := w.freshID()
		if status, body := w.post(t, w.alicePAT, demo, run, "/events", map[string]any{"id": u1, "event_type": "x"}); status != http.StatusCreated {
			t.Fatalf("store u1: status = %d %s, want 201", status, body)
		}
		scopeless := w.pat(t, w.alice)
		cases := []struct {
			name       string
			credential string
			slug, path string
			body       any
			want       int
			fault      func(on bool)
		}{
			{"a replay", w.alicePAT, demo, "/events", map[string]any{"id": u1, "event_type": "x"}, http.StatusOK, nil},
			{"an invalid run event", w.alicePAT, demo, "/events", map[string]any{}, http.StatusBadRequest, nil},
			{"a batch of a duplicate and an invalid item", w.alicePAT, demo, "/events/batch",
				[]any{map[string]any{"id": u1, "event_type": "x"}, map[string]any{}}, http.StatusOK, nil},
			{"a run event of a caller without the scope", scopeless, demo, "/events", map[string]any{"event_type": "x"}, http.StatusForbidden, nil},
			{"a run event of a caller without access", w.carolKey, demo, "/events", map[string]any{"event_type": "x"}, http.StatusNotFound, nil},
			{"a run event to an archived workspace", w.alicePAT, old, "/events", map[string]any{"event_type": "x"}, http.StatusConflict, nil},
			{"a trace record", w.alicePAT, demo, "/traces", map[string]any{"event_type": "tool.use"}, http.StatusCreated, nil},
			{"a batch of trace records", w.alicePAT, demo, "/traces/batch", []any{map[string]any{"event_type": "tool.use"}}, http.StatusOK, nil},
			{"a tool call", w.alicePAT, demo, "/tools/calls", map[string]any{"tool_name": "bash"}, http.StatusCreated, nil},
			{"a postmortem", w.alicePAT, demo, "/postmortem", validPostmortem(), http.StatusCreated, nil},
			{"a run event whose write fails", w.alicePAT, demo, "/events", map[string]any{"event_type": "x"},
				http.StatusInternalServerError, func(on bool) { w.failWrites.Store(on) }},
			{"a batch whose write fails", w.alicePAT, demo, "/events/batch", []any{map[string]any{"event_type": "x"}},
				http.StatusInternalServerError, func(on bool) { w.failWrites.Store(on) }},
			{"a run event whose write outlasts the limit", w.alicePAT, demo, "/events", map[string]any{"event_type": "x"},
				http.StatusServiceUnavailable, func(on bool) { w.blockWrites.Store(on) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				n := first.told()
				if tc.fault != nil {
					tc.fault(true)
					defer tc.fault(false)
				}
				if status, body := w.post(t, tc.credential, tc.slug, run, tc.path, tc.body); status != tc.want {
					t.Fatalf("status = %d %s, want %d", status, body, tc.want)
				}
				if got := first.since(n); len(got) != 0 {
					t.Errorf("observer was told %v, want nothing", got)
				}
			})
		}

		t.Run("a sandbox use the recorder stores nothing for", func(t *testing.T) {
			n := first.told()
			w.telemetry.RecordSandboxUse(context.Background(), telemetry.SandboxExec{SandboxUse: useOf(s2)})
			w.failWrites.Store(true)
			w.telemetry.RecordSandboxUse(context.Background(), telemetry.SandboxExec{SandboxUse: useOf(s1)})
			w.failWrites.Store(false)
			if got := first.since(n); len(got) != 0 {
				t.Errorf("observer was told %v, want nothing", got)
			}
		})
	})
}
