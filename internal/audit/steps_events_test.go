package audit_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/audit"
	"github.com/agent-fox-dev/parley/internal/event"
)

func TestEventSteps_MoveTheLogStartAndDeleteOldEventsButKeepTheNewest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// The hub starts long before the events, so that what it writes at its
	// start is older than every one of them.
	w := newWorld(t, retentionNow.Add(-300*day))
	w.waitForRetention(t)
	steps := audit.EventSteps(w.events, defaults, func() time.Time { return retentionNow })
	run := func(t *testing.T, names ...string) {
		t.Helper()
		for _, name := range names {
			if err := steps[name](ctx); err != nil {
				t.Fatalf("%s = %v", name, err)
			}
		}
	}
	bounds := func(t *testing.T) event.Bounds {
		t.Helper()
		b, err := w.events.Bounds(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	audited := func(t *testing.T) []key {
		t.Helper()
		return keysOf(t, w.follow(t, w.Token, url.Values{"source": {"hub"}}, 0)...)
	}
	named := func(names ...string) []key {
		keys := make([]key, len(names))
		for i, n := range names {
			keys[i] = w.records[n].key
		}
		return keys
	}

	t.Run("a lone event older than every limit stays the newest", func(t *testing.T) {
		e0 := w.hubEvent(t, "e0", retentionNow.Add(-200*day), event.Input{Type: "parley.test.lone"})
		w.clock.set(retentionNow)
		run(t, "event_log", "hub_events")
		if b := bounds(t); b.OldestSeq != e0.Seq || b.LastSeq != e0.Seq {
			t.Errorf("bounds = %+v, want oldest_seq = last_seq = %d", b, e0.Seq)
		}
		if got, want := audited(t), named("e0"); !slices.Equal(got, want) {
			t.Errorf("audited hub events = %v, want %v", got, want)
		}
	})

	t.Run("event_log moves the log start and hub_events deletes what is older than the maximum age", func(t *testing.T) {
		seq := map[string]int64{}
		for _, e := range []struct {
			name string
			age  time.Duration
		}{{"e1", 100 * day}, {"e2", 40 * day}, {"e3", 20 * day}, {"e4", day}} {
			seq[e.name] = w.hubEvent(t, e.name, retentionNow.Add(-e.age), event.Input{Type: "parley.test.aged"}).Seq
		}
		w.clock.set(retentionNow)
		s1, s3, s4 := seq["e1"], seq["e3"], seq["e4"]

		run(t, "event_log")
		if b := bounds(t); b.OldestSeq != s3 || b.LastSeq != s4 {
			t.Errorf("bounds after event_log = %+v, want oldest_seq %d, last_seq %d", b, s3, s4)
		}
		status, body := w.Call(t, http.MethodGet, fmt.Sprintf("/api/v1/events/log?after=%d", s1), w.Token, nil)
		if status != http.StatusGone || errorOf(t, body) != "cursor_expired" {
			t.Errorf("GET /events/log?after=%d = %d %s, want 410 cursor_expired", s1, status, body)
		}
		if got, want := audited(t), named("e4", "e3", "e2", "e1", "e0"); !slices.Equal(got, want) {
			t.Errorf("audited hub events after event_log = %v, want %v", got, want)
		}

		run(t, "hub_events")
		if got, want := audited(t), named("e4", "e3", "e2"); !slices.Equal(got, want) {
			t.Errorf("audited hub events after hub_events = %v, want %v", got, want)
		}
		if b := bounds(t); b.LastSeq != s4 {
			t.Errorf("last_seq after hub_events = %d, want %d", b.LastSeq, s4)
		}
	})
}

// delivery is a webhook delivery record the fake pruner holds.
type delivery struct {
	name, status string
	lastAttempt  time.Time
}

// fakePruner prunes its records as the delivery store does, and records
// the cutoffs it was called with.
type fakePruner struct {
	records []delivery
	calls   []time.Time
	err     error
}

func (p *fakePruner) PruneDeliveries(_ context.Context, before time.Time) (int64, error) {
	p.calls = append(p.calls, before)
	if p.err != nil {
		return 0, p.err
	}
	n := len(p.records)
	p.records = slices.DeleteFunc(p.records, func(d delivery) bool {
		return d.status != "pending" && d.lastAttempt.Before(before)
	})
	return int64(n - len(p.records)), nil
}

func TestWebhookDeliveriesStep_PrunesTheEndedDeliveriesOlderThanSevenDays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := func() time.Time { return retentionNow }
	at := func(age time.Duration) time.Time { return retentionNow.Add(-age) }

	t.Run("a registered pruner", func(t *testing.T) {
		p := &fakePruner{records: []delivery{
			{"succeeded-8d", "succeeded", at(8 * day)},
			{"failed-8d", "failed", at(8 * day)},
			{"pending-30d", "pending", at(30 * day)},
			{"succeeded-exactly-7d", "succeeded", at(7 * day)},
			{"succeeded-1d", "succeeded", at(day)},
		}}
		if err := audit.DeliverySteps(p, clock)["webhook_deliveries"](ctx); err != nil {
			t.Fatalf("webhook_deliveries = %v", err)
		}
		if want := []time.Time{retentionNow.Add(-168 * time.Hour)}; !slices.Equal(p.calls, want) {
			t.Errorf("pruner calls = %v, want %v", p.calls, want)
		}
		var kept []string
		for _, d := range p.records {
			kept = append(kept, d.name)
		}
		if want := []string{"pending-30d", "succeeded-exactly-7d", "succeeded-1d"}; !slices.Equal(kept, want) {
			t.Errorf("deliveries kept = %v, want %v", kept, want)
		}
	})

	t.Run("no pruner", func(t *testing.T) {
		if err := audit.DeliverySteps(nil, clock)["webhook_deliveries"](ctx); err != nil {
			t.Errorf("webhook_deliveries without a pruner = %v, want nil", err)
		}
	})

	t.Run("a failing pruner", func(t *testing.T) {
		boom := errors.New("the delivery store is gone")
		if err := audit.DeliverySteps(&fakePruner{err: boom}, clock)["webhook_deliveries"](ctx); !errors.Is(err, boom) {
			t.Errorf("webhook_deliveries = %v, want the pruner's error", err)
		}
	})
}
