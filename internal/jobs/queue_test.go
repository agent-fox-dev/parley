package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestList_ReadsAWorkspacesJobsOfATypeNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	// first and second share the second their created_at shows, so the id
	// orders them; third is the newest.
	first := q.seed(t, "probe", "w", StatusCompleted)
	q.clock.Advance(400 * time.Millisecond)
	second := q.seed(t, "probe", "w", StatusCancelled)
	q.clock.Advance(2 * time.Second)
	third := q.seed(t, "probe", "w", StatusQueued)
	q.seed(t, "probe", "other", StatusQueued)
	q.seed(t, "other", "w", StatusQueued)
	sameSecond := []string{first.ID, second.ID}
	slices.Sort(sameSecond)
	slices.Reverse(sameSecond)
	all := append([]string{third.ID}, sameSecond...)
	afterThird := third.Position()

	cases := []struct {
		name   string
		status string
		after  *ListPosition
		limit  int
		want   []string
	}{
		{"all", "", nil, 10, all},
		{"one status", StatusCancelled, nil, 10, []string{second.ID}},
		{"limited", "", nil, 2, all[:2]},
		{"after the newest", "", &afterThird, 10, all[1:]},
		{"after one of the same second", "", &ListPosition{Seconds: first.CreatedAt.Unix(), ID: sameSecond[0]}, 10, all[2:]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs, err := q.List(ctx, "probe", "w", tc.status, tc.after, tc.limit)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var got []string
			for _, j := range jobs {
				got = append(got, j.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("List = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPass_ClaimsAQueuedJobAndRunsItsHandler(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	h := newScriptedHandler(true)
	q.Register("probe", h)
	payload := json.RawMessage(`{"strategy":"rebase"}`)
	j := q.enqueue(t, "probe", "w", payload)
	// A result left from an earlier attempt, which the claim empties.
	err := q.st.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET result = '{"earlier":true}' WHERE id = ?`, j.ID)
		return err
	})
	if err != nil {
		t.Fatalf("set a result: %v", err)
	}
	q.clock.Advance(3 * time.Second)
	claimedAt := q.clock.Now()

	q.pass(t)
	got := h.Enter(t)

	if calls := h.Calls(); !slices.Equal(calls, []string{j.ID}) {
		t.Errorf("handler calls = %v, want [%s]", calls, j.ID)
	}
	if got.ID != j.ID || got.Status != StatusRunning || string(got.Payload) != string(payload) || got.Result != nil {
		t.Errorf("handler got %+v, want job %s running with its payload and no result", got, j.ID)
	}
	stored, err := q.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != StatusRunning || stored.Result != nil || !stored.UpdatedAt.Equal(claimedAt) {
		t.Errorf("stored job = %+v, want running, no result, updated at %v", stored, claimedAt)
	}
}

func TestRun_StartsAQueuedJobWithinFiveSeconds(t *testing.T) {
	t.Parallel()
	for _, noWake := range []bool{false, true} {
		name := map[bool]string{false: "woken by the enqueue", true: "on the interval alone"}[noWake]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			st := openStore(t, t.TempDir())
			q := New(Deps{Store: st, Clock: SystemClock(time.Now), NoWake: noWake})
			h := newScriptedHandler(false)
			q.Register("probe", h)
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				q.Run(ctx)
			}()
			defer func() {
				cancel()
				waitClosed(t, stopped, "the dispatcher's stop")
				q.settleAll(t)
			}()

			var j Job
			err := st.Tx(ctx, func(tx *sql.Tx) error {
				var err error
				j, err = q.Enqueue(ctx, tx, "probe", "w", json.RawMessage(`{}`))
				return err
			})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			committed := time.Now()

			got := h.Enter(t)
			if waited := time.Since(committed); got.ID != j.ID || waited > 5*time.Second {
				t.Errorf("handler got job %s after %v, want %s within 5 s", got.ID, waited, j.ID)
			}
		})
	}
}

func TestRun_ClaimsAJobWhoseWakeCameBeforeItsTransactionCommitted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	st := openStore(t, t.TempDir())
	// The clock's timers fire only when the test advances it, which it
	// never does: the wake of the enqueue is the one way to a pass.
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := New(Deps{Store: st, Clock: clock})
	h := newScriptedHandler(false)
	q.Register("probe", h)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		q.Run(ctx)
	}()
	defer func() {
		cancel()
		waitClosed(t, stopped, "the dispatcher's stop")
		q.settleAll(t)
	}()
	clock.WaitTimers(t, 1) // the first pass is over; the dispatcher waits

	var j Job
	err := st.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if j, err = q.Enqueue(ctx, tx, "probe", "w", json.RawMessage(`{}`)); err != nil {
			return err
		}
		// A route enqueues inside its own transaction and commits later:
		// leave the woken dispatcher the time to look before the commit.
		time.Sleep(100 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := h.Enter(t); got.ID != j.ID {
		t.Errorf("handler got job %s, want %s", got.ID, j.ID)
	}
}

func TestPass_RunsAtMostFourJobsAtOnce(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	h := newScriptedHandler(true)
	q.Register("probe", h)
	var ids []string
	for i := 1; i <= 6; i++ {
		ids = append(ids, q.enqueue(t, "probe", fmt.Sprintf("w%d", i), json.RawMessage(`{}`)).ID)
		q.clock.Advance(time.Second)
	}

	q.pass(t)
	for range MaxRunning {
		h.Enter(t)
	}
	q.pass(t)
	h.Idle(t)
	for _, id := range ids[4:] {
		if got := q.status(t, id); got != StatusQueued {
			t.Errorf("status of the job of a fifth or sixth workspace = %q while four run, want queued", got)
		}
	}

	q.finish(t, h, ids[0], nil)
	q.pass(t)
	if got := h.Enter(t); got.ID != ids[4] {
		t.Errorf("after one ended, the queue claimed %s, want the fifth job %s", got.ID, ids[4])
	}
	h.Idle(t)
	if got := q.status(t, ids[4]); got != StatusRunning {
		t.Errorf("status of the fifth job = %q, want running", got)
	}
	if got := q.status(t, ids[5]); got != StatusQueued {
		t.Errorf("status of the sixth job = %q, want queued", got)
	}
	if got := h.MaxActive(); got != MaxRunning {
		t.Errorf("most attempts running at once = %d, want %d", got, MaxRunning)
	}
}

func TestPass_RunsOneJobOfATypeAndWorkspaceAtATime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// failFirst lets the first job fail and fall due before the pass, so
		// that it and the follow-up are both queued when the pass begins.
		failFirst bool
	}{
		{"behind a running job", false},
		{"behind a job the same pass claims", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			q := newTestQueue(t)
			h := newScriptedHandler(true)
			q.Register("probe", h)
			a := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
			q.pass(t)
			h.Enter(t)
			// The follow-up is the younger job, so that a pass that could take
			// either takes the first.
			q.clock.Advance(time.Second)
			var f Job
			err := q.st.Tx(ctx, func(tx *sql.Tx) error {
				var err error
				f, err = q.EnqueueFollowUp(ctx, tx, a.ID, json.RawMessage(`{"follow":true}`))
				return err
			})
			if err != nil {
				t.Fatalf("EnqueueFollowUp: %v", err)
			}
			if tc.failFirst {
				q.finish(t, h, a.ID, errors.New("upstream unreachable"))
				// The first retry is due 4 s after the failure.
				q.clock.Advance(4 * time.Second)
			}

			q.pass(t)
			if tc.failFirst {
				h.Enter(t)
			}
			h.Idle(t)
			if got := q.status(t, f.ID); got != StatusQueued {
				t.Errorf("status of the follow-up = %q, want queued", got)
			}
			want := []string{a.ID}
			if tc.failFirst {
				want = append(want, a.ID)
			}
			if calls := h.Calls(); !slices.Equal(calls, want) {
				t.Errorf("handler calls = %v, want %v", calls, want)
			}

			q.finish(t, h, a.ID, nil)
			q.pass(t)
			if got := h.Enter(t); got.ID != f.ID {
				t.Errorf("after the first job ended, the queue claimed %s, want the follow-up %s", got.ID, f.ID)
			}
			if got := q.status(t, a.ID); got != StatusCompleted {
				t.Errorf("status of the first job = %q, want completed", got)
			}
			if got := q.status(t, f.ID); got != StatusRunning {
				t.Errorf("status of the follow-up = %q, want running", got)
			}
		})
	}
}

func TestPass_ClaimsAJobOnceUnderConcurrentPasses(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 50} {
		t.Run(fmt.Sprintf("%d jobs", n), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			q := newTestQueue(t)
			h := newScriptedHandler(false)
			q.Register("probe", h)
			var ids []string
			for i := range n {
				ids = append(ids, q.enqueue(t, "probe", fmt.Sprintf("w%d", i), json.RawMessage(`{}`)).ID)
			}

			for round := 0; ; round++ {
				if round == 2*n+2 {
					t.Fatalf("not every job completed after %d rounds of passes", round)
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				for range 16 {
					wg.Go(func() {
						<-start
						if err := q.Pass(ctx); err != nil {
							t.Errorf("Pass: %v", err)
						}
					})
				}
				close(start)
				wg.Wait()
				q.settleAll(t)
				var completed int
				err := q.st.Main().QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE status = ?`, StatusCompleted).Scan(&completed)
				if err != nil {
					t.Fatalf("count completed jobs: %v", err)
				}
				if completed == n {
					break
				}
			}

			calls := h.Calls()
			slices.Sort(calls)
			slices.Sort(ids)
			if !slices.Equal(calls, ids) {
				t.Errorf("handler calls = %v, want each of %v once", calls, ids)
			}
			if got := h.MaxActive(); got > MaxRunning {
				t.Errorf("most attempts running at once = %d, want at most %d", got, MaxRunning)
			}
		})
	}
}

func TestPass_ClaimsInCreationOrderThenByID(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	h := newScriptedHandler(true)
	q.Register("probe", h)
	var busy []string
	for i := range MaxRunning {
		busy = append(busy, q.enqueue(t, "probe", fmt.Sprintf("busy%d", i), json.RawMessage(`{}`)).ID)
	}
	q.pass(t)
	for range MaxRunning {
		h.Enter(t)
	}
	q.clock.Advance(time.Second)
	x := q.enqueue(t, "probe", "x", json.RawMessage(`{}`))
	q.clock.Advance(time.Second)
	y := q.enqueue(t, "probe", "y", json.RawMessage(`{}`))
	z := q.enqueue(t, "probe", "z", json.RawMessage(`{}`))
	want := []string{x.ID, min(y.ID, z.ID), max(y.ID, z.ID)}

	var got []string
	for i := range want {
		q.finish(t, h, busy[i], nil)
		q.pass(t)
		got = append(got, h.Enter(t).ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("claim order = %v, want %v", got, want)
	}
}

func TestPass_LeavesAJobWithoutHandlerQueuedUntilOneIsRegistered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.Register("probe", newScriptedHandler(false))
	j := q.enqueue(t, "nohandler", "w", json.RawMessage(`{}`))

	for range 2 {
		q.clock.Advance(time.Second)
		q.pass(t)
	}
	got, err := q.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, j) {
		t.Errorf("job after two passes = %+v, want it unchanged: %+v", got, j)
	}

	h := newScriptedHandler(true)
	q.Register("nohandler", h)
	q.pass(t)
	h.Enter(t)
	if calls := h.Calls(); !slices.Equal(calls, []string{j.ID}) {
		t.Errorf("handler calls = %v, want [%s]", calls, j.ID)
	}
}

func TestPass_RequeuesAFailedJobWhenItsNextAttemptIsDue(t *testing.T) {
	t.Parallel()
	for _, busy := range []bool{false, true} {
		name := map[bool]string{false: "a worker is free", true: "every worker is busy"}[busy]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			q := newTestQueue(t)
			h := newScriptedHandler(true)
			q.Register("probe", h)
			j := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
			q.pass(t)
			h.Enter(t)
			failedAt := q.clock.Now()
			q.finish(t, h, j.ID, errors.New("upstream unreachable"))
			if busy {
				for i := range MaxRunning {
					q.enqueue(t, "probe", fmt.Sprintf("busy%d", i), json.RawMessage(`{}`))
				}
				q.pass(t)
				for range MaxRunning {
					h.Enter(t)
				}
			}

			q.clock.Set(failedAt.Add(3 * time.Second))
			q.pass(t)
			h.Idle(t)
			if got := q.status(t, j.ID); got != StatusFailed {
				t.Errorf("status a second before the next attempt = %q, want failed", got)
			}

			q.clock.Set(failedAt.Add(4 * time.Second))
			q.pass(t)
			if busy {
				h.Idle(t)
				got, err := q.Get(ctx, j.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if got.Status != StatusQueued || got.NextAttemptAt != nil || got.RetryCount != 1 ||
					got.Error == nil || *got.Error != "upstream unreachable" {
					t.Errorf("job at its next attempt with every worker busy = %+v, want queued, no next attempt, "+
						"retry count 1 and its error", got)
				}
				return
			}
			got := h.Enter(t)
			if calls := h.Calls(); !slices.Equal(calls, []string{j.ID, j.ID}) {
				t.Errorf("handler calls = %v, want [%s %s]", calls, j.ID, j.ID)
			}
			if got.Status != StatusRunning || got.RetryCount != 1 || got.NextAttemptAt != nil ||
				got.Error == nil || *got.Error != "upstream unreachable" {
				t.Errorf("second attempt got %+v, want running, retry count 1, no next attempt and the first error", got)
			}
		})
	}
}

func TestDepth_CountsTheJobsOfEveryRegisteredTypeByStatus(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	q.Register("probe", newScriptedHandler(true))
	q.Register("rebuild", newScriptedHandler(true))
	want := map[string]int{
		StatusQueued: 2, StatusRunning: 1, StatusFailed: 1, StatusCompleted: 3, StatusDeadLetter: 1, StatusCancelled: 1,
	}
	n := 0
	for _, status := range Statuses {
		for range want[status] {
			// One workspace each, so that the pending jobs do not refuse one
			// another.
			q.seed(t, "probe", fmt.Sprintf("w%d", n), status)
			n++
		}
	}

	got, err := q.Depth(context.Background())
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	zero := map[string]int{}
	for _, status := range Statuses {
		zero[status] = 0
	}
	if wantAll := map[string]map[string]int{"probe": want, "rebuild": zero}; !reflect.DeepEqual(got, wantAll) {
		t.Errorf("Depth = %v, want %v", got, wantAll)
	}
}
