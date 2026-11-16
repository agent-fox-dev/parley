package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRecover_ReturnsRunningJobsToQueuedAndLeavesTheOthers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q1 := newTestQueue(t)
	h := newScriptedHandler(true)
	q1.Register("probe", h)

	// A job that failed three times and is running its fourth attempt.
	retried := q1.enqueue(t, "probe", "retried", json.RawMessage(`{"n":3}`))
	q1.failTimes(t, h, retried.ID, 3)
	q1.clock.Set(*q1.get(t, retried.ID).NextAttemptAt)
	q1.pass(t)
	h.Enter(t)
	// A job running its first attempt.
	first := q1.enqueue(t, "probe", "first", json.RawMessage(`{"n":0}`))
	q1.pass(t)
	h.Enter(t)

	others := []string{
		q1.attempt(t, h, q1.enqueue(t, "probe", "completed", json.RawMessage(`{}`)).ID, nil).ID,
		q1.attempt(t, h, q1.enqueue(t, "probe", "dead", json.RawMessage(`{}`)).ID, Permanent(errors.New("bad patch"))).ID,
		q1.attempt(t, h, q1.enqueue(t, "probe", "failed", json.RawMessage(`{}`)).ID, errors.New("upstream unreachable")).ID,
		q1.seed(t, "probe", "cancelled", StatusCancelled).ID,
		q1.enqueue(t, "probe", "queued", json.RawMessage(`{}`)).ID,
	}
	before := map[string]Job{}
	for _, id := range append(others, first.ID, retried.ID) {
		before[id] = q1.get(t, id)
	}
	if got := before[others[2]]; got.Status != StatusFailed || got.NextAttemptAt == nil {
		t.Fatalf("the failed job is %s with next attempt %v, want failed with one", got.Status, got.NextAttemptAt)
	}

	q2 := q1.reopen(t)
	if err := q2.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	for _, id := range []string{first.ID, retried.ID} {
		was, got := before[id], q2.get(t, id)
		if was.Status != StatusRunning {
			t.Fatalf("job %s was %s before the stop, want running", id, was.Status)
		}
		if got.Status != StatusQueued || got.RetryCount != was.RetryCount || !reflect.DeepEqual(got.Error, was.Error) ||
			string(got.Payload) != string(was.Payload) || !got.CreatedAt.Equal(was.CreatedAt) {
			t.Errorf("recovered job = %+v, want queued with the retry count, error, payload and creation of %+v", got, was)
		}
	}
	if got := q2.get(t, retried.ID); got.RetryCount != 3 || got.Error == nil || *got.Error != "upstream unreachable" {
		t.Errorf("the job running its fourth attempt was recovered with retry count %d and error %v, want 3 and its error",
			got.RetryCount, got.Error)
	}
	for _, id := range others {
		if got := q2.get(t, id); !reflect.DeepEqual(got, before[id]) {
			t.Errorf("job in %s after the recovery = %+v, want it unchanged: %+v", before[id].Status, got, before[id])
		}
	}
	var running int
	if err := q2.st.Main().QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE status = ?`, StatusRunning).Scan(&running); err != nil {
		t.Fatalf("count running jobs: %v", err)
	}
	if running != 0 {
		t.Errorf("running jobs after the recovery = %d, want 0", running)
	}
}

func TestRecover_RunsARecoveredJobAgainFromTheStart(t *testing.T) {
	t.Parallel()
	for _, retries := range []int{0, 2} {
		t.Run(fmt.Sprintf("%d retries before", retries), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			q1 := newTestQueue(t)
			h1 := newScriptedHandler(true)
			q1.Register("probe", h1)
			payload := json.RawMessage(`{"strategy":"rebase"}`)
			j := q1.enqueue(t, "probe", "w", payload)
			q1.failTimes(t, h1, j.ID, retries)

			// The attempt that the stop interrupts has stored a result.
			stored := make(chan struct{})
			interrupted := newScriptedHandler(false)
			interrupted.run = func(ctx context.Context, j Job) error {
				if err := q1.SetResult(ctx, j.ID, json.RawMessage(`{"patches":["a"]}`)); err != nil {
					t.Errorf("SetResult: %v", err)
				}
				close(stored)
				<-ctx.Done()
				return ctx.Err()
			}
			q1.Register("probe", interrupted)
			if next := q1.get(t, j.ID).NextAttemptAt; next != nil {
				q1.clock.Set(*next)
			}
			q1.pass(t)
			interrupted.Enter(t)
			waitClosed(t, stored, "the interrupted attempt's result")

			q2 := q1.reopen(t)
			h2 := newScriptedHandler(false)
			q2.Register("probe", h2)
			if err := q2.Recover(ctx); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			q2.pass(t)
			got := h2.Enter(t)
			q2.settleAll(t)

			if calls := h2.Calls(); len(calls) != 1 || calls[0] != j.ID {
				t.Errorf("attempts after the recovery = %v, want one of %s", calls, j.ID)
			}
			if string(got.Payload) != string(payload) || got.Result != nil || got.RetryCount != retries {
				t.Errorf("the attempt was given %+v, want payload %s, no result and retry count %d", got, payload, retries)
			}
			if done := q2.get(t, j.ID); done.Status != StatusCompleted || done.RetryCount != retries {
				t.Errorf("job after the attempt = %s with retry count %d, want completed with %d", done.Status, done.RetryCount, retries)
			}
		})
	}
}

func TestRun_StopsClaimingAndSettlesTheAttemptsThatEndWithinTheGrace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		running int
		// during queues the waiting job once the stop has begun, rather than
		// before it behind a full limit.
		during bool
	}{
		{name: "queued before the stop behind a full limit", running: MaxRunning},
		{name: "queued during the stop", running: 1, during: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := newTestQueue(t)
			h := newScriptedHandler(true)
			q.Register("probe", h)
			var runningIDs []string
			for i := range tc.running {
				runningIDs = append(runningIDs, q.enqueue(t, "probe", fmt.Sprintf("run-%d", i), json.RawMessage(`{}`)).ID)
			}
			var waiting Job
			if !tc.during {
				q.clock.Advance(time.Second) // claimed after the others
				waiting = q.enqueue(t, "probe", "waiting", json.RawMessage(`{}`))
			}
			stop, stopped := q.run(t)
			for range tc.running {
				h.Enter(t)
			}

			stop()
			q.clock.WaitTimerAt(t, q.clock.Now().Add(ShutdownGrace))
			if tc.during {
				waiting = q.enqueue(t, "probe", "waiting", json.RawMessage(`{}`))
			}
			q.pass(t)
			h.Idle(t)
			for i, id := range runningIDs {
				if returned(stopped) {
					t.Fatalf("Run returned with %d attempts still running", len(runningIDs)-i)
				}
				q.finish(t, h, id, nil)
			}
			waitClosed(t, stopped, "Run's return after the last attempt ended")

			for _, id := range runningIDs {
				if s := q.status(t, id); s != StatusCompleted {
					t.Errorf("job whose attempt ended within the grace is %s, want completed", s)
				}
			}
			if s := q.status(t, waiting.ID); s != StatusQueued {
				t.Errorf("waiting job is %s, want queued", s)
			}
			for _, id := range h.Calls() {
				if id == waiting.ID {
					t.Errorf("the waiting job's handler was called")
				}
			}
		})
	}
}

func TestRun_AbandonsTheAttemptsStillRunningAtTheEndOfTheGrace(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	attempts := make(chan context.Context, 1)
	proceed := make(chan struct{})
	h := newScriptedHandler(false)
	h.run = func(ctx context.Context, _ Job) error {
		attempts <- ctx
		<-ctx.Done()
		<-proceed
		return errors.New("upstream unreachable")
	}
	q.Register("probe", h)
	j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
	stop, stopped := q.run(t)
	release := sync.OnceFunc(func() { close(proceed) })
	t.Cleanup(release) // before the queue's cleanup waits for the attempt
	h.Enter(t)
	attemptCtx := <-attempts
	done := q.doneOf(j.ID)

	stop()
	stoppedAt := q.clock.Now()
	q.clock.WaitTimerAt(t, stoppedAt.Add(ShutdownGrace))
	q.clock.Set(stoppedAt.Add(ShutdownGrace - time.Second))
	if returned(stopped) || attemptCtx.Err() != nil {
		t.Fatalf("a second before the end of the grace: Run returned %v, attempt cancelled %v; want neither",
			returned(stopped), attemptCtx.Err() != nil)
	}
	q.clock.Set(stoppedAt.Add(ShutdownGrace))
	waitClosed(t, attemptCtx.Done(), "the cancellation of the attempt")
	waitClosed(t, stopped, "Run's return at the end of the grace")

	release()
	waitClosed(t, done, "the end of the abandoned attempt")
	if got := q.get(t, j.ID); got.Status != StatusRunning || got.RetryCount != 0 || got.Error != nil {
		t.Errorf("abandoned job = %+v, want running with retry count 0 and no error", got)
	}
}
