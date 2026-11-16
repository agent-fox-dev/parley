package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"uuid"
)

func TestEnqueue_StoresAQueuedJobInTheCallersTransaction(t *testing.T) {
	t.Parallel()
	for _, commit := range []bool{true, false} {
		name := map[bool]string{true: "commit", false: "rollback"}[commit]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			q := newTestQueue(t)
			payload := json.RawMessage(`{"strategy":"rebase"}`)
			rollback := errors.New("roll back")
			var j Job
			err := q.st.Tx(ctx, func(tx *sql.Tx) error {
				var err error
				if j, err = q.Enqueue(ctx, tx, "probe", "w", payload); err != nil {
					return err
				}
				if _, err := q.Get(ctx, j.ID); !errors.Is(err, ErrNotFound) {
					t.Errorf("Get before commit: err %v, want ErrNotFound", err)
				}
				if !commit {
					return rollback
				}
				return nil
			})
			if commit && err != nil || !commit && !errors.Is(err, rollback) {
				t.Fatalf("transaction: %v", err)
			}

			id, perr := uuid.Parse(j.ID)
			if len(j.ID) != 36 || perr != nil || id[6]>>4 != 4 || id[8]>>6 != 2 {
				t.Errorf("id %q is not a version 4 UUID", j.ID)
			}
			now := clockStart
			if j.Type != "probe" || j.Workspace != "w" || j.Status != StatusQueued || j.RetryCount != 0 ||
				j.NextAttemptAt != nil || j.Error != nil || j.CompletedAt != nil || j.Result != nil ||
				!j.CreatedAt.Equal(now) || !j.UpdatedAt.Equal(now) || string(j.Payload) != string(payload) {
				t.Errorf("enqueued job = %+v, want a queued probe job of w at %v with the payload", j, now)
			}

			got, err := q.Get(ctx, j.ID)
			if !commit {
				if !errors.Is(err, ErrNotFound) {
					t.Errorf("Get after rollback: err %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get after commit: %v", err)
			}
			if string(got.Payload) != string(payload) || got.Status != StatusQueued || got.Result != nil {
				t.Errorf("stored job = %+v, want the queued job with the payload and no result", got)
			}
		})
	}
}

func TestEnqueue_RefusesASecondPendingJobOfTheTypeAndWorkspace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		typ, slug    string
		status       string
		wantDup      bool
		wantCountOfW int
	}{
		{"queued", "probe", "w", StatusQueued, true, 1},
		{"running", "probe", "w", StatusRunning, true, 1},
		{"failed", "probe", "w", StatusFailed, true, 1},
		{"completed", "probe", "w", StatusCompleted, false, 2},
		{"dead letter", "probe", "w", StatusDeadLetter, false, 2},
		{"cancelled", "probe", "w", StatusCancelled, false, 2},
		{"pending in another workspace", "probe", "other", StatusQueued, false, 1},
		{"pending of another type", "other", "w", StatusQueued, false, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			q := newTestQueue(t)
			existing := q.seed(t, tc.typ, tc.slug, tc.status)

			var j Job
			err := q.st.Tx(ctx, func(tx *sql.Tx) error {
				var err error
				j, err = q.Enqueue(ctx, tx, "probe", "w", json.RawMessage(`{}`))
				return err
			})
			var dup *DuplicateError
			switch {
			case tc.wantDup && (!errors.As(err, &dup) || dup.JobID != existing.ID):
				t.Errorf("Enqueue: err %v, want a duplicate naming %s", err, existing.ID)
			case !tc.wantDup && (err != nil || j.ID == existing.ID):
				t.Errorf("Enqueue: job %s, err %v, want a new job", j.ID, err)
			}
			if n := q.count(t, "w"); n != tc.wantCountOfW {
				t.Errorf("jobs of w = %d, want %d", n, tc.wantCountOfW)
			}
		})
	}
}

func TestEnqueue_ConcurrentCallsStoreOneJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)

	const callers = 16
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  []string
		dups []string
		errs []error
	)
	for range callers {
		wg.Go(func() {
			var j Job
			err := q.st.Tx(ctx, func(tx *sql.Tx) error {
				var err error
				j, err = q.Enqueue(ctx, tx, "probe", "w", json.RawMessage(`{}`))
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			var dup *DuplicateError
			switch {
			case err == nil:
				won = append(won, j.ID)
			case errors.As(err, &dup):
				dups = append(dups, dup.JobID)
			default:
				errs = append(errs, err)
			}
		})
	}
	wg.Wait()

	if len(won) != 1 || len(dups) != callers-1 || len(errs) != 0 {
		t.Fatalf("%d stored, %d duplicates, errors %v; want 1 stored and %d duplicates", len(won), len(dups), errs, callers-1)
	}
	for _, id := range dups {
		if id != won[0] {
			t.Errorf("duplicate names %s, want the stored job %s", id, won[0])
		}
	}
	if n := q.count(t, "w"); n != 1 {
		t.Errorf("jobs of w = %d, want 1", n)
	}
}

func TestEnqueueFollowUp_QueuesBehindTheRunningJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	running := q.seed(t, "probe", "w", StatusRunning)

	var f Job
	err := q.st.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		f, err = q.EnqueueFollowUp(ctx, tx, running.ID, json.RawMessage(`{"follow_up":true}`))
		return err
	})
	if err != nil {
		t.Fatalf("EnqueueFollowUp: %v", err)
	}
	if f.Status != StatusQueued || f.ID == running.ID || f.Type != "probe" || f.Workspace != "w" ||
		string(f.Payload) != `{"follow_up":true}` {
		t.Errorf("follow-up = %+v, want a new queued probe job of w with its payload", f)
	}
	if got, err := q.Get(ctx, f.ID); err != nil || got.Status != StatusQueued {
		t.Errorf("Get follow-up: %+v, %v; want it stored queued", got, err)
	}
	if n := q.count(t, "w"); n != 2 {
		t.Errorf("jobs of w = %d, want 2", n)
	}
}

func TestEnqueueFollowUp_RefusesUnlessTheNamedJobIsTheOnePendingAndRunning(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// arrange stores the jobs of the case and returns the id the
		// follow-up names and the id the duplicate must name ("" for a
		// not-found error).
		arrange func(t *testing.T, q *testQueue) (named, want string)
	}{
		{"named job queued", func(t *testing.T, q *testQueue) (string, string) {
			j := q.seed(t, "probe", "w", StatusQueued)
			return j.ID, j.ID
		}},
		{"named job failed", func(t *testing.T, q *testQueue) (string, string) {
			j := q.seed(t, "probe", "w", StatusFailed)
			return j.ID, j.ID
		}},
		{"named job completed with another job queued", func(t *testing.T, q *testQueue) (string, string) {
			done := q.seed(t, "probe", "w", StatusCompleted)
			queued := q.seed(t, "probe", "w", StatusQueued)
			return done.ID, queued.ID
		}},
		{"named job running with a follow-up queued", func(t *testing.T, q *testQueue) (string, string) {
			running := q.seed(t, "probe", "w", StatusRunning)
			var f Job
			err := q.st.Tx(context.Background(), func(tx *sql.Tx) error {
				var err error
				f, err = q.EnqueueFollowUp(context.Background(), tx, running.ID, json.RawMessage(`{}`))
				return err
			})
			if err != nil {
				t.Fatalf("first follow-up: %v", err)
			}
			return running.ID, f.ID
		}},
		{"unknown job", func(t *testing.T, q *testQueue) (string, string) {
			q.seed(t, "probe", "w", StatusRunning)
			return uuid.NewV4().String(), ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			q := newTestQueue(t)
			named, want := tc.arrange(t, q)
			before := q.count(t, "w")

			err := q.st.Tx(ctx, func(tx *sql.Tx) error {
				_, err := q.EnqueueFollowUp(ctx, tx, named, json.RawMessage(`{}`))
				return err
			})
			var dup *DuplicateError
			switch {
			case want == "" && !errors.Is(err, ErrNotFound):
				t.Errorf("EnqueueFollowUp: err %v, want ErrNotFound", err)
			case want != "" && (!errors.As(err, &dup) || dup.JobID != want):
				t.Errorf("EnqueueFollowUp: err %v, want a duplicate naming %s", err, want)
			}
			if n := q.count(t, "w"); n != before {
				t.Errorf("jobs of w = %d, want %d", n, before)
			}
		})
	}
}

func TestEnqueue_RefusesAPayloadThatIsNotJSON(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	for _, payload := range []json.RawMessage{nil, json.RawMessage(`{"strategy":`)} {
		err := q.st.Tx(ctx, func(tx *sql.Tx) error {
			_, err := q.Enqueue(ctx, tx, "probe", "w", payload)
			return err
		})
		if !errors.Is(err, errPayload) {
			t.Errorf("Enqueue(%q): err %v, want errPayload", payload, err)
		}
	}
	if n := q.count(t, "w"); n != 0 {
		t.Errorf("jobs of w = %d, want 0", n)
	}
}
