package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSettle_CompletesAJobWhoseAttemptSucceeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	h := newScriptedHandler(true)
	q.Register("probe", h)
	j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
	if got := q.attempt(t, h, j.ID, WithDetail(errors.New("upstream unreachable"), json.RawMessage(`{"files":["a"]}`))); got.Status != StatusFailed || got.Detail == nil {
		t.Fatalf("after the first attempt the job is %+v, want failed with a detail", got)
	}

	q.clock.Set(*q.get(t, j.ID).NextAttemptAt)
	q.pass(t)
	h.Enter(t)
	result := json.RawMessage(`{"patches_applied":2}`)
	if err := q.SetResult(ctx, j.ID, result); err != nil {
		t.Fatalf("SetResult: %v", err)
	}
	q.clock.Advance(time.Minute)
	q.finish(t, h, j.ID, nil)

	got := q.get(t, j.ID)
	if got.Status != StatusCompleted || got.Error != nil || got.Detail != nil || got.NextAttemptAt != nil ||
		got.RetryCount != 1 || string(got.Result) != string(result) {
		t.Errorf("completed job = %+v, want completed, no error, detail or next attempt, retry count 1, result %s",
			got, result)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(q.clock.Now()) {
		t.Errorf("completed_at = %v, want %v", got.CompletedAt, q.clock.Now())
	}
}

func TestSettle_RetriesARetryableFailureWithBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		before int
		delay  time.Duration
	}{
		{0, 4 * time.Second},
		{1, 8 * time.Second},
		{2, 16 * time.Second},
		{10, 4096 * time.Second},
		{11, 2 * time.Hour},
		{19, 2 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d retries before", tc.before), func(t *testing.T) {
			t.Parallel()
			q := newTestQueue(t)
			h := newScriptedHandler(true)
			q.Register("probe", h)
			j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
			q.failTimes(t, h, j.ID, tc.before)

			got := q.attempt(t, h, j.ID, errors.New("boom"))
			if got.Status != StatusFailed || got.RetryCount != tc.before+1 || got.Error == nil || *got.Error != "boom" ||
				got.CompletedAt != nil {
				t.Errorf("job after a failure at retry count %d = %+v, want failed, retry count %d, error boom, "+
					"not completed", tc.before, got, tc.before+1)
			}
			if got.NextAttemptAt == nil || got.NextAttemptAt.Sub(q.clock.Now()) != tc.delay {
				t.Errorf("next attempt = %v, want %v after %v", got.NextAttemptAt, tc.delay, q.clock.Now())
			}
		})
	}
}

func TestSettle_ReplacesOrClearsTheDetailOfAFailure(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	h := newScriptedHandler(true)
	q.Register("probe", h)
	j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
	steps := []struct {
		err  error
		want string
	}{
		{WithDetail(errors.New("conflict"), json.RawMessage(`{"report":"C"}`)), `{"report":"C"}`},
		{WithDetail(errors.New("conflict"), json.RawMessage(`{"report":"D"}`)), `{"report":"D"}`},
		{errors.New("unreachable"), ""},
	}
	for i, s := range steps {
		got := q.attempt(t, h, j.ID, s.err)
		if got.Status != StatusFailed || string(got.Detail) != s.want {
			t.Errorf("after failure %d: status %s, detail %s, want failed with detail %q", i+1, got.Status, got.Detail, s.want)
		}
	}
}

func TestSettle_DeadLettersTheRetryableFailureAfterTheLastRetry(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	h := newScriptedHandler(true)
	q.Register("probe", h)
	j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
	q.failTimes(t, h, j.ID, MaxRetries)

	got := q.attempt(t, h, j.ID, errors.New("boom"))
	if got.Status != StatusDeadLetter || got.RetryCount != MaxRetries || got.NextAttemptAt != nil ||
		got.Error == nil || *got.Error != "boom" {
		t.Errorf("job after failure %d = %+v, want dead_letter, retry count %d, error boom, no next attempt",
			MaxRetries+1, got, MaxRetries)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(q.clock.Now()) {
		t.Errorf("completed_at = %v, want %v", got.CompletedAt, q.clock.Now())
	}
	q.clock.Advance(3 * time.Hour)
	q.pass(t)
	h.Idle(t)
	if n := len(h.Calls()); n != MaxRetries+1 {
		t.Errorf("handler calls = %d, want %d", n, MaxRetries+1)
	}
}

func TestSettle_DeadLettersAPermanentFailureAtOnce(t *testing.T) {
	t.Parallel()
	for _, before := range []int{0, 3} {
		t.Run(fmt.Sprintf("%d retries before", before), func(t *testing.T) {
			t.Parallel()
			q := newTestQueue(t)
			h := newScriptedHandler(true)
			q.Register("probe", h)
			j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
			q.failTimes(t, h, j.ID, before)

			detail := `{"files":["README.md"]}`
			got := q.attempt(t, h, j.ID, Permanent(WithDetail(errors.New("conflict"), json.RawMessage(detail))))
			if got.Status != StatusDeadLetter || got.RetryCount != before || got.Error == nil || *got.Error != "conflict" ||
				string(got.Detail) != detail || got.NextAttemptAt != nil {
				t.Errorf("job after a permanent failure = %+v, want dead_letter, retry count %d, error conflict, "+
					"detail %s, no next attempt", got, before, detail)
			}
			if got.CompletedAt == nil || !got.CompletedAt.Equal(q.clock.Now()) {
				t.Errorf("completed_at = %v, want %v", got.CompletedAt, q.clock.Now())
			}
			q.clock.Advance(3 * time.Hour)
			q.pass(t)
			h.Idle(t)
			if n := len(h.Calls()); n != before+1 {
				t.Errorf("handler calls = %d, want %d", n, before+1)
			}
		})
	}
}

func TestSettle_FailsAnAttemptWhoseHandlerPanics(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	h := newScriptedHandler(false)
	h.run = func(_ context.Context, j Job) error {
		if j.Workspace == "w" {
			panic("kaboom")
		}
		return nil
	}
	q.Register("probe", h)
	j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
	q.pass(t)
	q.settleAll(t)
	got := q.get(t, j.ID)
	if got.Status != StatusFailed || got.RetryCount != 1 || got.Error == nil || !strings.Contains(*got.Error, "kaboom") {
		t.Errorf("job whose handler panicked = %+v, want failed, retry count 1, an error naming kaboom", got)
	}

	other := q.enqueue(t, "probe", "w2", json.RawMessage(`{}`))
	q.pass(t)
	q.settleAll(t)
	if got := q.status(t, other.ID); got != StatusCompleted {
		t.Errorf("status of a job queued after the panic = %q, want completed", got)
	}
}

// createMarkers adds a table a settlement hook writes to, so that a test
// sees which of its writes were committed.
func createMarkers(t *testing.T, q *testQueue) {
	t.Helper()
	err := q.st.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `CREATE TABLE markers (job_id TEXT NOT NULL, status TEXT NOT NULL)`)
		return err
	})
	if err != nil {
		t.Fatalf("create markers: %v", err)
	}
}

// markersOf are the statuses the markers of the job id record.
func markersOf(t *testing.T, q *testQueue, id string) []string {
	t.Helper()
	rows, err := q.st.Main().QueryContext(context.Background(), `SELECT status FROM markers WHERE job_id = ?`, id)
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("read markers: %v", err)
		}
		got = append(got, s)
	}
	return got
}

// writeMarker writes a marker of j through tx, recording the job's status
// as tx reads it.
func writeMarker(ctx context.Context, tx *sql.Tx, j Job) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO markers (job_id, status) SELECT id, status FROM jobs WHERE id = ?`, j.ID)
	return err
}

func TestSettle_CallsTheSettlementHookInTheTransaction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		before    int
		err       error
		status    string
		willRetry bool
	}{
		{"success", 0, nil, StatusCompleted, false},
		{"retryable failure", 0, errors.New("boom"), StatusFailed, true},
		{"permanent failure", 0, Permanent(errors.New("conflict")), StatusDeadLetter, false},
		{"retryable failure after the last retry", MaxRetries, errors.New("boom"), StatusDeadLetter, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := newTestQueue(t)
			createMarkers(t, q)
			h := newScriptedHandler(true)
			q.Register("probe", h)
			j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
			q.failTimes(t, h, j.ID, tc.before)

			type call struct {
				job       Job
				willRetry bool
			}
			var calls []call
			h.settled = func(ctx context.Context, tx *sql.Tx, j Job, willRetry bool) error {
				calls = append(calls, call{j, willRetry})
				return writeMarker(ctx, tx, j)
			}
			got := q.attempt(t, h, j.ID, tc.err)
			if got.Status != tc.status {
				t.Fatalf("status = %s, want %s", got.Status, tc.status)
			}
			if len(calls) != 1 {
				t.Fatalf("hook calls = %d, want 1", len(calls))
			}
			if c := calls[0]; c.job.Status != tc.status || c.willRetry != tc.willRetry || c.job.RetryCount != got.RetryCount ||
				!c.job.UpdatedAt.Equal(got.UpdatedAt) {
				t.Errorf("hook got %+v, will retry %v; want the job as settled (%+v), will retry %v",
					c.job, c.willRetry, got, tc.willRetry)
			}
			if m := markersOf(t, q, j.ID); len(m) != 1 || m[0] != tc.status {
				t.Errorf("markers = %v, want one written in the settlement's transaction, at status %s", m, tc.status)
			}
		})
	}
}

func TestSettle_CommitsTheSettlementWithoutTheWritesOfAFailingHook(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fail func() error
	}{
		{"hook returns an error", func() error { return errors.New("hook broke") }},
		{"hook panics", func() error { panic("hook broke") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := newTestQueue(t)
			var log bytes.Buffer
			q.Bind(Deps{Store: q.st, Clock: q.clock, NoWake: true, Logger: slog.New(slog.NewJSONHandler(&log, nil))})
			createMarkers(t, q)
			h := newScriptedHandler(true)
			h.settled = func(ctx context.Context, tx *sql.Tx, j Job, _ bool) error {
				if err := writeMarker(ctx, tx, j); err != nil {
					return err
				}
				return tc.fail()
			}
			q.Register("probe", h)
			j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))

			if got := q.attempt(t, h, j.ID, nil); got.Status != StatusCompleted {
				t.Errorf("status = %s, want completed", got.Status)
			}
			if m := markersOf(t, q, j.ID); len(m) != 0 {
				t.Errorf("markers = %v, want the hook's write undone", m)
			}
			var entries int
			for line := range strings.Lines(log.String()) {
				var e map[string]any
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Fatalf("log line %q: %v", line, err)
				}
				if e["job_id"] == j.ID {
					entries++
				}
			}
			if entries != 1 {
				t.Errorf("log entries naming the job = %d, want 1; log:\n%s", entries, log.String())
			}

			other := q.enqueue(t, "probe", "w2", json.RawMessage(`{}`))
			q.pass(t)
			if got := h.Enter(t); got.ID != other.ID {
				t.Errorf("next pass ran %s, want %s", got.ID, other.ID)
			}
		})
	}
}

func TestSetResult_ReplacesTheResultOfARunningJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	h := newScriptedHandler(true)
	q.Register("probe", h)
	j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
	q.pass(t)
	h.Enter(t)
	for _, r := range []string{`{"step":1}`, `{"step":2}`} {
		if err := q.SetResult(ctx, j.ID, json.RawMessage(r)); err != nil {
			t.Fatalf("SetResult %s: %v", r, err)
		}
		if got := q.get(t, j.ID).Result; string(got) != r {
			t.Errorf("result while running = %s, want %s", got, r)
		}
	}
	q.finish(t, h, j.ID, nil)

	for _, id := range []string{j.ID, "00000000-0000-4000-8000-000000000000"} {
		if err := q.SetResult(ctx, id, json.RawMessage(`{"step":3}`)); !errors.Is(err, ErrNotRunning) {
			t.Errorf("SetResult on %s that is not running: %v, want ErrNotRunning", id, err)
		}
	}
	if got := q.get(t, j.ID).Result; string(got) != `{"step":2}` {
		t.Errorf("result after completion = %s, want {\"step\":2}", got)
	}

	// The result of a failed attempt stays until the next claim empties it.
	f := q.enqueue(t, "probe", "w2", json.RawMessage(`{}`))
	q.pass(t)
	h.Enter(t)
	if err := q.SetResult(ctx, f.ID, json.RawMessage(`{"step":1}`)); err != nil {
		t.Fatalf("SetResult: %v", err)
	}
	q.finish(t, h, f.ID, errors.New("boom"))
	if got := q.get(t, f.ID); string(got.Result) != `{"step":1}` {
		t.Errorf("result of the failed job = %s, want {\"step\":1}", got.Result)
	}
	q.clock.Set(*q.get(t, f.ID).NextAttemptAt)
	q.pass(t)
	if got := h.Enter(t); got.Result != nil {
		t.Errorf("the retry was given result %s, want none", got.Result)
	}
	if got := q.get(t, f.ID); got.Result != nil {
		t.Errorf("result of the retried job = %s, want null", got.Result)
	}
}
