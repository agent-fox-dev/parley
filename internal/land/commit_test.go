package land_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/land"
)

// landState is what a land leaves behind: where main is, the change's
// state, the attempt's status, how many of its landed commits resolve to
// the change, its parley.change.land events, the parley.git.push events
// of a land, and the entries left in the land journal.
type landState struct {
	Main, Change, Attempt             string
	Mapped, Lands, Pushes, Journalled int
}

func TestCommit_ComesUpLandedOrUntouchedAfterAStopAtEachPoint(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		point  string
		landed bool
	}{
		{"before-journal", false},
		{"before-swap", false},
		{"after-swap", true},
		{"in-bookkeeping", true},
		{"after-bookkeeping", true},
	} {
		t.Run(c.point, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			ctx := context.Background()
			ch := w.open(t, "feature")
			stop := errors.New("the hub stops")
			land.SetSeams(w.land, land.Seams{Fault: func(point string) error {
				if point == c.point {
					return stop
				}
				return nil
			}})
			a, err := w.runLand(t, ch, w.caller(), nil)
			if !errors.Is(err, stop) {
				t.Fatalf("run = %v, want it stopped at %s", err, c.point)
			}

			w.Restart(t)

			state := func() landState {
				stored := w.attempt(t, a.ID)
				s := landState{Main: head(t, w.api, "main"), Change: w.readChange(t, ch.Number).State, Attempt: stored.Status,
					Pushes: len(w.landPushes(t))}
				for _, cm := range stored.Commits {
					if cc, found, err := w.changes.ChangeOfCommit(ctx, nil, "api", cm.LandedSHA); err == nil && found &&
						cc.Number == ch.Number && cc.Landed {
						s.Mapped++
					}
				}
				for _, e := range w.eventsOf(t, land.EventLand) {
					if e.ResourceID == strconv.FormatInt(ch.Number, 10) {
						s.Lands++
					}
				}
				n, err := land.Journalled(ctx, w.Store)
				if err != nil {
					t.Fatal(err)
				}
				s.Journalled = n
				return s
			}
			got := state()
			want := landState{Main: w.base, Change: change.StateOpen, Attempt: land.StatusRunning}
			if c.landed {
				landed := w.attempt(t, a.ID)
				if landed.LandedSHA == nil {
					t.Fatalf("attempt = %s with nothing landed, want landed", landed.Status)
				}
				want = landState{Main: *landed.LandedSHA, Change: change.StateLanded, Attempt: land.StatusLanded,
					Mapped: 1, Lands: 1, Pushes: 1}
			}
			if got != want {
				t.Errorf("after the restart: %+v, want %+v", got, want)
			}

			if err := w.land.Reconcile(ctx); err != nil {
				t.Fatalf("reconcile again: %v", err)
			}
			if again := state(); !reflect.DeepEqual(again, got) {
				t.Errorf("a second reconciliation left %+v, want %+v unchanged", again, got)
			}
		})
	}
}
