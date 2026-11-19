package sandbox_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/sandbox"
)

// ended are the states in which a sandbox has ended.
var ended = []string{sandbox.StateFailed, sandbox.StateCompleted, sandbox.StateStopped, sandbox.StateDeleted}

// transitionEvent is the event a test writes with a transition to state.
func transitionEvent(to string) *event.Input {
	return &event.Input{Type: "parley.sandbox." + to, ActorType: event.ActorSystem}
}

func TestTransition_AppliesAllowed(t *testing.T) {
	t.Parallel()
	h := startRecordHub(t)
	cases := []struct{ from, to string }{
		{sandbox.StateProvisioning, sandbox.StateReady},
		{sandbox.StateProvisioning, sandbox.StateFailed},
		{sandbox.StateReady, sandbox.StateCompleted},
		{sandbox.StateReady, sandbox.StateFailed},
		{sandbox.StateReady, sandbox.StateStopped},
		{sandbox.StateReady, sandbox.StateProvisioning},
		{sandbox.StateStopped, sandbox.StateProvisioning},
		{sandbox.StateCompleted, sandbox.StateProvisioning},
		{sandbox.StateFailed, sandbox.StateProvisioning},
		{sandbox.StateProvisioning, sandbox.StateDeleted},
		{sandbox.StateReady, sandbox.StateDeleted},
		{sandbox.StateStopped, sandbox.StateDeleted},
		{sandbox.StateCompleted, sandbox.StateDeleted},
		{sandbox.StateFailed, sandbox.StateDeleted},
	}
	for _, tc := range cases {
		t.Run(tc.from+" to "+tc.to, func(t *testing.T) {
			id := h.seed(t, tc.from)
			// The runtime's pause is written without an event.
			ev := transitionEvent(tc.to)
			if tc.from == sandbox.StateReady && tc.to == sandbox.StateProvisioning {
				ev = nil
			}
			applied, err := h.svc.Transition(context.Background(), nil, id, tc.from, tc.to, "because", ev)
			if !applied || err != nil {
				t.Fatalf("Transition = %v, %v; want applied", applied, err)
			}
			rec := storedRecord(t, h.Hub, id)
			if rec.State != tc.to || rec.StateReason != "because" || !rec.UpdatedAt.Equal(recordStart) {
				t.Errorf("stored state %q, reason %q, updated_at %v; want %q, %q, %v",
					rec.State, rec.StateReason, rec.UpdatedAt, tc.to, "because", recordStart)
			}
			endedNow := rec.EndedAt != nil && rec.EndedAt.Equal(recordStart)
			if want := slices.Contains(ended, tc.to); endedNow != want {
				t.Errorf("ended_at = %v; ended now %v, want %v", rec.EndedAt, endedNow, want)
			}
			events := h.events(t, id)
			if ev == nil {
				if len(events) != 0 {
					t.Errorf("events = %+v; want none", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("events = %+v; want one", events)
			}
			e := events[0]
			if e.typ != ev.Type || e.resourceType != "sandbox" || e.workspace == nil || *e.workspace != "ws" ||
				e.session == nil || *e.session != recordSession {
				t.Errorf("event %s of resource %s, workspace %v, session %v; want %s of sandbox, ws, %s",
					e.typ, e.resourceType, e.workspace, e.session, ev.Type, recordSession)
			}
		})
	}
}

func TestTransition_SkipsOnStateMismatch(t *testing.T) {
	t.Parallel()
	h := startRecordHub(t)
	for _, stored := range []string{sandbox.StateDeleted, sandbox.StateFailed} {
		t.Run(stored, func(t *testing.T) {
			id := h.seed(t, stored)
			before := storedRecord(t, h.Hub, id)
			h.clock.Advance(time.Minute)
			applied, err := h.svc.Transition(context.Background(), nil, id, sandbox.StateProvisioning, sandbox.StateReady, "", transitionEvent(sandbox.StateReady))
			if applied || err != nil {
				t.Fatalf("Transition = %v, %v; want not applied and no error", applied, err)
			}
			if after := storedRecord(t, h.Hub, id); !reflect.DeepEqual(after, before) {
				t.Errorf("record changed:\n got %+v\nwant %+v", after, before)
			}
			if events := h.events(t, id); len(events) != 0 {
				t.Errorf("events = %+v; want none", events)
			}
		})
	}
}

func TestTransition_RefusesUnknownPairs(t *testing.T) {
	t.Parallel()
	h := startRecordHub(t)
	cases := []struct{ from, to string }{
		{sandbox.StateProvisioning, sandbox.StateStopped},
		{sandbox.StateProvisioning, sandbox.StateCompleted},
		{sandbox.StateStopped, sandbox.StateReady},
		{sandbox.StateCompleted, sandbox.StateFailed},
		{sandbox.StateDeleted, sandbox.StateProvisioning},
		{sandbox.StateDeleted, sandbox.StateDeleted},
		{sandbox.StateReady, sandbox.StateReady},
	}
	for _, tc := range cases {
		t.Run(tc.from+" to "+tc.to, func(t *testing.T) {
			id := h.seed(t, tc.from)
			before := storedRecord(t, h.Hub, id)
			applied, err := h.svc.Transition(context.Background(), nil, id, tc.from, tc.to, "", transitionEvent(tc.to))
			if applied || !errors.Is(err, sandbox.ErrInvalidTransition) {
				t.Fatalf("Transition = %v, %v; want not applied and the invalid-transition error", applied, err)
			}
			if after := storedRecord(t, h.Hub, id); !reflect.DeepEqual(after, before) {
				t.Errorf("record changed:\n got %+v\nwant %+v", after, before)
			}
			if events := h.events(t, id); len(events) != 0 {
				t.Errorf("events = %+v; want none", events)
			}
		})
	}
	t.Run("unknown id", func(t *testing.T) {
		unknown := "sbx_00000000000000000000000000000000"
		applied, err := h.svc.Transition(context.Background(), nil, unknown, sandbox.StateProvisioning, sandbox.StateReady, "", transitionEvent(sandbox.StateReady))
		if applied || !errors.Is(err, sandbox.ErrNotFound) {
			t.Fatalf("Transition = %v, %v; want not applied and the not-found error", applied, err)
		}
		if events := h.events(t, unknown); len(events) != 0 {
			t.Errorf("events = %+v; want none", events)
		}
	})
}

func TestTransition_RaceYieldsOne(t *testing.T) {
	t.Parallel()
	h := startRecordHub(t)
	targets := []string{sandbox.StateReady, sandbox.StateDeleted}
	for round := range 50 {
		id := h.seed(t, sandbox.StateProvisioning)
		var applied [2]bool
		var errs [2]error
		var wg sync.WaitGroup
		for i, to := range targets {
			wg.Go(func() {
				applied[i], errs[i] = h.svc.Transition(context.Background(), nil, id, sandbox.StateProvisioning, to, "", transitionEvent(to))
			})
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || applied[0] == applied[1] {
			t.Fatalf("round %d: applied %v, errors %v; want exactly one applied and no error", round, applied, errs)
		}
		won := targets[0]
		if applied[1] {
			won = targets[1]
		}
		events := h.events(t, id)
		if len(events) != 1 || events[0].typ != transitionEvent(won).Type {
			t.Fatalf("round %d: events %+v; want one of the transition to %s", round, events, won)
		}
		if got := storedRecord(t, h.Hub, id).State; got != won {
			t.Fatalf("round %d: stored state %s; want %s", round, got, won)
		}
	}
	// A call that expects the state now stored applies as usual.
	id := h.seed(t, sandbox.StateProvisioning)
	if applied, err := h.svc.Transition(context.Background(), nil, id, sandbox.StateProvisioning, sandbox.StateReady, "", nil); !applied || err != nil {
		t.Fatalf("Transition to ready = %v, %v", applied, err)
	}
	if applied, err := h.svc.Transition(context.Background(), nil, id, sandbox.StateReady, sandbox.StateDeleted, "", nil); !applied || err != nil {
		t.Fatalf("Transition from the stored state = %v, %v; want applied", applied, err)
	}
}

func TestTransition_ObservesSeconds(t *testing.T) {
	t.Parallel()
	h := startRecordHub(t)
	const metric = "parley_sandbox_seconds"
	ctx := context.Background()
	// step is one transition after the clock moved on by after.
	type step struct {
		after    time.Duration
		from, to string
	}
	ready := step{0, sandbox.StateProvisioning, sandbox.StateReady}
	cases := []struct {
		name  string
		steps []step
		// skipped is a last call that expects ready while the record is no
		// longer ready, so that it does not apply.
		skipped bool
		want    []float64
	}{
		{"to stopped", []step{ready, {90 * time.Second, sandbox.StateReady, sandbox.StateStopped}}, false, []float64{90}},
		{"to completed", []step{ready, {90 * time.Second, sandbox.StateReady, sandbox.StateCompleted}}, false, []float64{90}},
		{"to failed", []step{ready, {90 * time.Second, sandbox.StateReady, sandbox.StateFailed}}, false, []float64{90}},
		{"to deleted", []step{ready, {90 * time.Second, sandbox.StateReady, sandbox.StateDeleted}}, false, []float64{90}},
		{"through the runtime's pause", []step{
			ready,
			{10 * time.Second, sandbox.StateReady, sandbox.StateProvisioning},
			{10 * time.Second, sandbox.StateProvisioning, sandbox.StateReady},
			{40 * time.Second, sandbox.StateReady, sandbox.StateStopped},
		}, false, []float64{60}},
		{"a call that does not apply", []step{ready, {90 * time.Second, sandbox.StateReady, sandbox.StateStopped}}, true, []float64{90}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := h.seed(t, sandbox.StateProvisioning)
			seen := len(h.rec.observations(metric))
			for _, s := range tc.steps {
				h.clock.Advance(s.after)
				if applied, err := h.svc.Transition(ctx, nil, id, s.from, s.to, "", nil); !applied || err != nil {
					t.Fatalf("Transition %s to %s = %v, %v", s.from, s.to, applied, err)
				}
			}
			if tc.skipped {
				h.clock.Advance(30 * time.Second)
				if applied, err := h.svc.Transition(ctx, nil, id, sandbox.StateReady, sandbox.StateStopped, "", nil); applied || err != nil {
					t.Fatalf("Transition from ready of a stopped sandbox = %v, %v; want not applied", applied, err)
				}
			}
			if got := h.rec.observations(metric)[seen:]; !slices.Equal(got, tc.want) {
				t.Errorf("observations = %v; want %v", got, tc.want)
			}
		})
	}

	// In the caller's transaction the observation is the caller's to make
	// once it has committed: one that rolls back observes nothing.
	for _, commit := range []bool{true, false} {
		t.Run(fmt.Sprintf("in the caller's transaction, committed %t", commit), func(t *testing.T) {
			id := h.seed(t, sandbox.StateProvisioning)
			if applied, err := h.svc.Transition(ctx, nil, id, sandbox.StateProvisioning, sandbox.StateReady, "", nil); !applied || err != nil {
				t.Fatalf("Transition to ready = %v, %v", applied, err)
			}
			seen := len(h.rec.observations(metric))
			h.clock.Advance(90 * time.Second)
			var report func()
			err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
				applied, r, err := h.svc.TransitionTx(ctx, tx, id, sandbox.StateReady, sandbox.StateStopped, "", nil)
				if !applied || err != nil {
					t.Fatalf("TransitionTx to stopped = %v, %v", applied, err)
				}
				report = r
				if got := h.rec.observations(metric)[seen:]; len(got) != 0 {
					t.Errorf("observations %v before the commit; want none", got)
				}
				if !commit {
					return errRollback
				}
				return nil
			})
			want := []float64{}
			if commit {
				if err != nil {
					t.Fatal(err)
				}
				report()
				want = []float64{90}
			}
			if got := h.rec.observations(metric)[seen:]; !slices.Equal(got, want) {
				t.Errorf("observations = %v; want %v", got, want)
			}
		})
	}
}
