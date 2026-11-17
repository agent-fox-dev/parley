package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/jobs"
)

// rebuildHub is a carry-patch hub whose workspace carry holds one active
// patch, and whose job queue runs rebuild jobs with the handler the test
// gives it, on the test's clock.
type rebuildHub struct {
	*workspaceHub
	queue *jobs.Queue
	clock *fakeNow
}

// startRebuildHub starts a rebuild hub whose rebuild jobs handler runs. Its
// job queue passes only when the test calls pass: its timers never fire
// and nothing wakes it.
func startRebuildHub(t *testing.T, handler jobs.Handler) *rebuildHub {
	t.Helper()
	h := &rebuildHub{clock: &fakeNow{now: time.Now().UTC()}}
	clock := &stillJobsClock{now: h.clock.Now, armed: make(chan struct{})}
	h.workspaceHub, _, _ = startPatchHubWith(t, h.clock.Now, func(c *hub.Composition) {
		h.queue = c.Jobs()
		h.queue.Register(carrypatch.RebuildJobType, handler)
		c.AdjustJobs(func(d *jobs.Deps) { d.Clock, d.NoWake = clock, true })
	}, "patch/a")
	// The dispatcher's own pass at its start could claim a job of the test;
	// from here on only the test's passes do.
	waitClosed(t, clock.armed, "the job queue's first pass")
	return h
}

// pass runs one pass of the job queue.
func (h *rebuildHub) pass(t *testing.T) {
	t.Helper()
	if err := h.queue.Pass(context.Background()); err != nil {
		t.Fatalf("pass the job queue: %v", err)
	}
}

// rebuildAnswer is an answer of a rebuild route: a job's record, a list of
// them or an error.
type rebuildAnswer struct {
	status int
	raw    []byte
	record map[string]any
	Items  []map[string]any `json:"items"`
	Error  struct {
		ErrorType string `json:"error_type"`
	} `json:"error"`
}

// rebuildCall sends method /api/v1/workspaces/carry+path with the
// developer's key and decodes the answer.
func (h *rebuildHub) rebuildCall(t *testing.T, method, path string) rebuildAnswer {
	t.Helper()
	status, raw := h.Call(t, method, "/api/v1/workspaces/carry"+path, h.key, nil)
	a := rebuildAnswer{status: status, raw: raw}
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(raw, &a.record)
	return a
}

// stillJobsClock is a job queue's clock whose time is now and whose timers
// never fire. armed is closed once the dispatcher has asked for its first
// timer, which it does after the pass it makes as it starts.
type stillJobsClock struct {
	now   func() time.Time
	once  sync.Once
	armed chan struct{}
}

func (c *stillJobsClock) Now() time.Time { return c.now() }

func (c *stillJobsClock) NewTimer(time.Duration) jobs.Timer {
	c.once.Do(func() { close(c.armed) })
	return stillTimer{}
}

type stillTimer struct{}

func (stillTimer) C() <-chan time.Time { return nil }

func (stillTimer) Stop() bool { return true }

// waitClosed waits up to 10 s for ch to be closed, and fails the test then.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("waited 10 s for %s", what)
	}
}

// failsForGoodOnce is a rebuild handler whose first attempt fails for good
// with a conflict report as its detail, and whose later attempts run until
// they are cancelled. settled is closed once the first attempt's outcome
// is being settled.
type failsForGoodOnce struct {
	calls   atomic.Int32
	once    sync.Once
	settled chan struct{}
}

func (f *failsForGoodOnce) Run(ctx context.Context, _ jobs.Job) error {
	if f.calls.Add(1) == 1 {
		return jobs.WithDetail(jobs.Permanent(errors.New("patch/a conflicts with upstream")), json.RawMessage(`{"files":["a.txt"]}`))
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *failsForGoodOnce) Settled(context.Context, *sql.Tx, jobs.Job, bool) error {
	f.once.Do(func() { close(f.settled) })
	return nil
}

// An agent's rebuild fails for good; while a second rebuild is queued the
// first cannot be requeued, so the agent cancels the second and requeues
// the first.
func TestRebuildJobs_CancelAQueuedRebuildAndRequeueADeadLetteredOne(t *testing.T) {
	parallel(t)
	handler := &failsForGoodOnce{settled: make(chan struct{})}
	h := startRebuildHub(t, handler)

	first := h.rebuildCall(t, http.MethodPost, "/rebuild")
	a, _ := first.record["id"].(string)
	if first.status != http.StatusAccepted || a == "" {
		t.Fatalf("POST rebuild = %d %s, want 202 with a job", first.status, first.raw)
	}
	h.pass(t)
	waitClosed(t, handler.settled, "the first attempt to be settled")
	// The store runs its write transactions one at a time, in the order
	// they are asked for: an empty one asked for now ends after the
	// settlement's.
	if err := h.Store.Tx(context.Background(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if r := h.rebuildCall(t, http.MethodGet, "/rebuilds/"+a); r.record["status"] != "dead_letter" || r.record["conflict"] == nil {
		t.Fatalf("GET the first job = %d %s, want dead_letter with a conflict", r.status, r.raw)
	}

	// The second job is made a second later, so that the list puts it first.
	h.clock.advance(time.Second)
	second := h.rebuildCall(t, http.MethodPost, "/rebuild")
	b, _ := second.record["id"].(string)
	if second.status != http.StatusAccepted || b == "" {
		t.Fatalf("POST rebuild again = %d %s, want 202 with a second job", second.status, second.raw)
	}
	for _, step := range []struct {
		method, path string
		status       int
		errorType    string
	}{
		{http.MethodPost, "/rebuilds/" + a + "/requeue", http.StatusConflict, "concurrent_rebuild"},
		{http.MethodDelete, "/rebuilds/" + b, http.StatusOK, ""},
		{http.MethodDelete, "/rebuilds/" + b, http.StatusConflict, "job_not_cancellable"},
		{http.MethodPost, "/rebuilds/" + a + "/requeue", http.StatusOK, ""},
	} {
		if r := h.rebuildCall(t, step.method, step.path); r.status != step.status || r.Error.ErrorType != step.errorType {
			t.Fatalf("%s %s = %d %s, want %d %s", step.method, step.path, r.status, r.raw, step.status, step.errorType)
		}
	}

	if r := h.rebuildCall(t, http.MethodGet, "/rebuilds/"+a); r.record["status"] != "queued" || r.record["retry_count"] != 0.0 ||
		r.record["error"] != nil || r.record["conflict"] != nil {
		t.Errorf("GET the first job = %d %s, want queued with retry_count 0, no error and no conflict", r.status, r.raw)
	}
	if r := h.rebuildCall(t, http.MethodGet, "/rebuilds/"+b); r.record["status"] != "cancelled" || r.record["completed_at"] == nil {
		t.Errorf("GET the second job = %d %s, want cancelled with completed_at", r.status, r.raw)
	}
	counts := map[string]int{}
	for _, e := range h.patchEvents(t, "carry", carrypatch.EventRebuildEnqueue, carrypatch.EventRebuildCancel, carrypatch.EventRebuildRequeue) {
		counts[e.Type]++
	}
	if want := map[string]int{carrypatch.EventRebuildEnqueue: 2, carrypatch.EventRebuildCancel: 1, carrypatch.EventRebuildRequeue: 1}; !maps.Equal(counts, want) {
		t.Errorf("the event log holds %v for carry, want %v", counts, want)
	}
	list := h.rebuildCall(t, http.MethodGet, "/rebuilds")
	var ids []string
	for _, it := range list.Items {
		id, _ := it["id"].(string)
		ids = append(ids, id)
	}
	if !slices.Equal(ids, []string{b, a}) {
		t.Errorf("GET rebuilds = %d %s, want the second job, then the first", list.status, list.raw)
	}
}
