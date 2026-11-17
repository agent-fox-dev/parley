package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/cli"
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
	// jobsClock is the job queue's clock, on clock.
	jobsClock *jobsClock
}

// startRebuildHub starts a rebuild hub whose rebuild jobs handler runs. Its
// job queue passes only when the test calls pass, unless adjust, called on
// the queue's dependencies at every start of the hub, turns the wake on;
// the queue's timers fire only when the test advances jobsClock.
func startRebuildHub(t *testing.T, handler jobs.Handler, adjust func(*jobs.Deps)) *rebuildHub {
	t.Helper()
	h := &rebuildHub{clock: &fakeNow{now: time.Now().UTC()}}
	h.jobsClock = &jobsClock{fakeNow: h.clock, timers: map[*jobsTimer]bool{}, armed: map[time.Duration]chan struct{}{}}
	h.workspaceHub, _, _ = startPatchHubWith(t, h.clock.Now, func(c *hub.Composition) {
		h.queue = c.Jobs()
		h.queue.Register(carrypatch.RebuildJobType, handler)
		c.AdjustJobs(func(d *jobs.Deps) {
			d.Clock, d.NoWake = h.jobsClock, true
			if adjust != nil {
				adjust(d)
			}
		})
	}, "patch/a")
	// The dispatcher's own pass at its start could claim a job of the test;
	// from here on only the test's passes, the wake and the clock do.
	waitClosed(t, h.jobsClock.armedFor(jobs.DefaultInterval), "the job queue's first pass")
	return h
}

// pass runs one pass of the job queue.
func (h *rebuildHub) pass(t *testing.T) {
	t.Helper()
	if err := h.queue.Pass(context.Background()); err != nil {
		t.Fatalf("pass the job queue: %v", err)
	}
}

// waitRebuild reads GET /api/v1/workspaces/carry+path, as rebuildCall
// does, until fn holds for the answer, or fails the test after 10 s.
func (h *rebuildHub) waitRebuild(t *testing.T, path string, fn func(rebuildAnswer) bool) rebuildAnswer {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := h.rebuildCall(t, http.MethodGet, path)
		if fn(r) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s is %d %s after 10 s", path, r.status, r.raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stepWhile is a condition of waitRebuild that holds once the first job
// the answer holds, the job or the first of a list, has a status other
// than status, and that moves the job queue's clock on by a second each
// time it does not. Each step fires the dispatcher's timer for its next
// pass, whether or not the dispatcher had armed it when the step before
// moved the clock. A job is not left to the wake its enqueue gives: that
// wake can come while the enqueuing request's transaction is still open,
// and the pass it starts then finds no job.
func (h *rebuildHub) stepWhile(status string) func(rebuildAnswer) bool {
	return func(r rebuildAnswer) bool {
		job := r.record
		if r.Items != nil {
			if len(r.Items) == 0 {
				return false
			}
			job = r.Items[0]
		}
		if job["status"] == status {
			h.jobsClock.advance(time.Second)
			return false
		}
		return true
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

// jobsClock is a job queue's clock on the test's clock whose timers fire
// when the test advances it with advance.
type jobsClock struct {
	*fakeNow
	mu     sync.Mutex
	timers map[*jobsTimer]bool
	// armed holds, for each duration, a channel closed once a timer of it
	// has been asked for.
	armed map[time.Duration]chan struct{}
}

type jobsTimer struct {
	clock *jobsClock
	at    time.Time
	c     chan time.Time
}

func (c *jobsClock) NewTimer(d time.Duration) jobs.Timer {
	t := &jobsTimer{clock: c, at: c.Now().Add(d), c: make(chan time.Time, 1)}
	armed := c.armedFor(d)
	c.mu.Lock()
	c.timers[t] = true
	select {
	case <-armed:
	default:
		close(armed)
	}
	c.mu.Unlock()
	return t
}

// armedFor is a channel closed once a timer of d has been asked for.
func (c *jobsClock) armedFor(d time.Duration) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.armed[d]
	if !ok {
		ch = make(chan struct{})
		c.armed[d] = ch
	}
	return ch
}

// advance moves the clock on by d and fires the timers that are due.
func (c *jobsClock) advance(d time.Duration) {
	c.fakeNow.advance(d)
	now := c.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for t := range c.timers {
		if !t.at.After(now) {
			delete(c.timers, t)
			t.c <- now
		}
	}
}

func (t *jobsTimer) C() <-chan time.Time { return t.c }

func (t *jobsTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	pending := t.clock.timers[t]
	delete(t.clock.timers, t)
	return pending
}

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
	h := startRebuildHub(t, handler, nil)

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

// scriptedRebuild is a rebuild handler whose n-th attempt does script[n-1]
// and whose attempts succeed once the script has run out. It records the
// job of every attempt.
type scriptedRebuild struct {
	script []func() error
	mu     sync.Mutex
	jobs   []jobs.Job
}

func (s *scriptedRebuild) Run(_ context.Context, j jobs.Job) error {
	s.mu.Lock()
	s.jobs = append(s.jobs, j)
	n := len(s.jobs)
	s.mu.Unlock()
	if n <= len(s.script) {
		return s.script[n-1]()
	}
	return nil
}

func (*scriptedRebuild) Settled(context.Context, *sql.Tx, jobs.Job, bool) error { return nil }

// attempts are the jobs of the attempts so far, in order.
func (s *scriptedRebuild) attempts() []jobs.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.jobs)
}

// watchedBuffer is a buffer a running command writes to; seen is closed
// once what it holds contains want.
type watchedBuffer struct {
	want string
	seen chan struct{}
	mu   sync.Mutex
	buf  bytes.Buffer
	once sync.Once
}

func (b *watchedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if strings.Contains(b.buf.String(), b.want) {
		b.once.Do(func() { close(b.seen) })
	}
	return len(p), nil
}

func (b *watchedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// timeIn is the time the member name of record holds, the zero time when it
// holds none.
func timeIn(record map[string]any, name string) time.Time {
	text, _ := record[name].(string)
	at, _ := time.Parse(time.RFC3339, text)
	return at
}

// A developer submits a rebuild from the shell and waits for it; the first
// attempt fails, the job waits 4 s to retry, and the command ends with the
// job completed by the second attempt.
func TestRebuildJobs_SubmitAndWaitThroughARetry(t *testing.T) {
	parallel(t)
	handler := &scriptedRebuild{script: []func() error{func() error { return errors.New("upstream unreachable") }}}
	h := startRebuildHub(t, handler, func(d *jobs.Deps) { d.NoWake = false })

	stderr := &watchedBuffer{want: "waiting: failed", seen: make(chan struct{})}
	var stdout bytes.Buffer
	exited := make(chan int, 1)
	go func() {
		args := []string{"--endpoint-url", h.URL(""), "--api-key", h.key, "rebuild", "submit", "carry", "--wait", "--poll-interval", "1s"}
		exited <- cli.Run(context.Background(), args, &stdout, stderr)
	}()
	// The queued job is claimed on a pass of the dispatcher, which the
	// queue's clock times.
	claimed := h.stepWhile("queued")
	h.waitRebuild(t, "/rebuilds", func(r rebuildAnswer) bool {
		select {
		case code := <-exited:
			t.Fatalf("parley rebuild submit exited %d before the job was claimed; stderr %q", code, stderr.String())
		default:
		}
		return claimed(r)
	})
	select {
	case <-stderr.seen:
	case code := <-exited:
		t.Fatalf("parley rebuild submit exited %d before it saw the job failed; stderr %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("parley rebuild submit did not see the job failed within 10 s; stderr %q", stderr.String())
	}
	list := h.rebuildCall(t, http.MethodGet, "/rebuilds")
	if len(list.Items) != 1 {
		t.Fatalf("GET rebuilds = %d %s, want one job", list.status, list.raw)
	}
	id, _ := list.Items[0]["id"].(string)
	failed := h.rebuildCall(t, http.MethodGet, "/rebuilds/"+id).record
	if failed["status"] != "failed" || failed["retry_count"] != 1.0 || failed["error"] != "upstream unreachable" ||
		!timeIn(failed, "next_attempt_at").Equal(timeIn(failed, "updated_at").Add(4*time.Second)) {
		t.Fatalf("the job waiting to retry = %v, want failed with retry_count 1 and its next attempt 4 s after the failure", failed)
	}

	h.jobsClock.advance(4 * time.Second)
	h.waitRebuild(t, "/rebuilds/"+id, h.stepWhile("failed"))
	var code int
	select {
	case code = <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("parley rebuild submit did not end within 10 s; stderr %q", stderr.String())
	}

	var final map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &final); code != 0 || err != nil || final["id"] != id ||
		final["status"] != "completed" || final["retry_count"] != 1.0 {
		t.Errorf("parley rebuild submit --wait exited %d with stdout %q, stderr %q; want 0 and the job completed with retry_count 1",
			code, stdout.String(), stderr.String())
	}
	var attempted []string
	for _, j := range handler.attempts() {
		attempted = append(attempted, j.ID)
	}
	if !slices.Equal(attempted, []string{id, id}) {
		t.Errorf("the handler ran the jobs %v, want %s twice", attempted, id)
	}
	enqueued := 0
	for _, e := range h.patchEvents(t, "carry", carrypatch.EventRebuildEnqueue) {
		if e.ResourceID == id {
			enqueued++
		}
	}
	if enqueued != 1 {
		t.Errorf("the event log holds %d %s for the job, want 1", enqueued, carrypatch.EventRebuildEnqueue)
	}
	if list := h.rebuildCall(t, http.MethodGet, "/rebuilds"); len(list.Items) != 1 || list.Items[0]["id"] != id {
		t.Errorf("GET rebuilds = %d %s, want the job once", list.status, list.raw)
	}
}

// A rebuild is running when the hub stops, and its handler does not end
// within the queue's grace; the hub started again on the same data runs the
// job again from the start and completes it.
func TestRebuildJobs_ARunningRebuildSurvivesARestart(t *testing.T) {
	parallel(t)
	started, release := make(chan struct{}), make(chan struct{})
	handler := &scriptedRebuild{script: []func() error{func() error {
		close(started)
		<-release
		return nil
	}}}
	// found holds, for each start of the hub, the jobs the store held
	// before the queue recovered them, as id:status.
	var mu sync.Mutex
	var found [][]string
	h := startRebuildHub(t, handler, func(d *jobs.Deps) {
		d.NoWake = false
		held := []string{}
		rows, err := d.Store.Main().Query(`SELECT id, status FROM jobs ORDER BY id`)
		if err != nil {
			held = append(held, err.Error())
		} else {
			defer rows.Close()
			for rows.Next() {
				var id, status string
				_ = rows.Scan(&id, &status)
				held = append(held, id+":"+status)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		found = append(found, held)
	})
	t.Cleanup(func() { close(release) })

	post := h.rebuildCall(t, http.MethodPost, "/rebuild")
	id, _ := post.record["id"].(string)
	if post.status != http.StatusAccepted || id == "" {
		t.Fatalf("POST rebuild = %d %s, want 202 with a job", post.status, post.raw)
	}
	h.waitRebuild(t, "/rebuilds/"+id, h.stepWhile("queued"))
	waitClosed(t, started, "the first attempt")
	if r := h.rebuildCall(t, http.MethodGet, "/rebuilds/"+id); r.record["status"] != "running" {
		t.Fatalf("GET the job = %d %s, want running", r.status, r.raw)
	}
	// The stop waits for the attempt for the queue's grace, on its clock.
	go func() {
		select {
		case <-h.jobsClock.armedFor(jobs.ShutdownGrace):
			h.jobsClock.advance(jobs.ShutdownGrace)
		case <-time.After(10 * time.Second):
		}
	}()
	h.Restart(t)

	done := h.waitRebuild(t, "/rebuilds/"+id, func(r rebuildAnswer) bool { return r.record["status"] == "completed" })
	if done.record["retry_count"] != 0.0 {
		t.Errorf("the job after the restart = %s, want completed with retry_count 0", done.raw)
	}
	mu.Lock()
	if want := []string{id + ":running"}; len(found) != 2 || !slices.Equal(found[1], want) {
		t.Errorf("the starts found the jobs %v before the recovery, want the second to find %v", found, want)
	}
	mu.Unlock()
	attempts := handler.attempts()
	if len(attempts) != 2 || attempts[1].ID != id || string(attempts[1].Payload) != string(attempts[0].Payload) {
		t.Fatalf("the handler ran %v, want the job %s once in each hub, with one payload", attempts, id)
	}
	var payload map[string]any
	_ = json.Unmarshal(attempts[1].Payload, &payload)
	for _, member := range []string{"strategy", "fail_mode", "integration_branch", "submitted_by"} {
		if payload[member] != post.record[member] {
			t.Errorf("the second hub's attempt ran with %s %v, want %v as queued", member, payload[member], post.record[member])
		}
	}
	for _, job := range h.rebuildCall(t, http.MethodGet, "/rebuilds").Items {
		if job["status"] == "running" {
			t.Errorf("the job %v is running after the restart", job["id"])
		}
	}
}
