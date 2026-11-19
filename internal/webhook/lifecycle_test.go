package webhook_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRestart_ResumesEachSubscriptionWhereTheStopLeftIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure + "timeout = \"60s\"\n"})
	// held answers nothing until the test releases it: its first attempt is
	// in flight when the hub stops, and its second when the test looks at
	// what the stop left.
	release := make(chan struct{})
	held := newInbox(t, func(_ delivered, stop <-chan struct{}) int {
		select {
		case <-release:
		case <-stop:
		}
		return http.StatusOK
	})
	flaky := answeringWith(http.StatusInternalServerError)
	retried := newInbox(t, flaky.answer)
	off := newInbox(t, answerOK)
	inFlight := w.subscribe(t, demo, map[string]any{"url": held.url(), "types": testTypes})
	scheduled := w.subscribe(t, demo, map[string]any{"url": retried.url(), "types": testTypes})
	disabled := w.subscribe(t, demo, map[string]any{"url": off.url(), "types": testTypes})
	w.patch(t, disabled, map[string]any{"active": false})
	e1 := w.emit(t, demo, "parley.test.one", "")
	e2 := w.emit(t, demo, "parley.test.two", "")
	waitFor(t, "the first attempt to be in flight", func() bool { return len(held.requests()) == 1 })
	pending := w.retrying(t, scheduled, 1)

	stopped := w.Served
	w.Restart(t)

	for line := range strings.SplitSeq(stopped.Log.String(), "\n") {
		if strings.Contains(line, "abandoned at the stop") && strings.Contains(line, "webhook deliveries") {
			t.Errorf("the stop waited past its grace for the delivery loops: %s", line)
		}
	}

	// The stop abandoned the attempt in flight and recorded nothing of it:
	// the restarted loop makes it again at once, under the same id.
	waitFor(t, "the abandoned attempt to be made again", func() bool { return len(held.requests()) == 2 })
	got := held.requests()
	if got[1].seq != e1 || got[1].delivery != got[0].delivery {
		t.Errorf("after the restart the receiver got seq %d of delivery %s, want seq %d of delivery %s",
			got[1].seq, got[1].delivery, e1, got[0].delivery)
	}
	if ds := w.deliveries(t, inFlight); len(ds) != 1 || ds[0].id != got[0].delivery || ds[0].status != "pending" ||
		ds[0].attempts != 0 || ds[0].nextAttemptAt.Valid {
		t.Errorf("while the attempt is made again the delivery records are %+v, want %s pending with no attempt recorded", ds, got[0].delivery)
	}
	if r := w.row(t, inFlight); r.position >= e1 {
		t.Errorf("while the attempt is made again the position is %d, want it before %d", r.position, e1)
	}
	close(release)
	waitFor(t, "the resumed subscription to deliver every event", func() bool { return w.settled(t, inFlight) })
	if seqs := held.seqs(); !slices.Equal(seqs, []int64{e1, e1, e2}) {
		t.Errorf("the receiver got seqs %v, want %v", seqs, []int64{e1, e1, e2})
	}
	if ds := w.deliveries(t, inFlight); len(ds) != 2 || ds[0].id != got[0].delivery || ds[0].attempts != 1 || ds[0].status != "succeeded" {
		t.Errorf("delivery records = %+v, want %s succeeded after one recorded attempt, then the next event's", ds, got[0].delivery)
	}

	// A delivery scheduled before the stop waits for its next attempt.
	due := timeAt(pending.nextAttemptAt.Int64)
	waitFor(t, "the restarted loop to wait for the scheduled attempt", func() bool { return w.clock.armed(due) })
	if n := len(retried.requests()); n != 1 {
		t.Fatalf("before %s the receiver got %d requests, want the 1 before the restart", due, n)
	}
	flaky.set(http.StatusOK)
	w.clock.advance(due.Sub(w.clock.Now()))
	waitFor(t, "the scheduled subscription to deliver every event", func() bool { return w.settled(t, scheduled) })
	if ds := w.deliveries(t, scheduled); len(ds) != 2 || ds[0].id != pending.id || ds[0].attempts != 2 || ds[0].status != "succeeded" {
		t.Errorf("delivery records = %+v, want %s succeeded at its second attempt, then the next event's", ds, pending.id)
	}

	if n := len(off.requests()); n != 0 {
		t.Errorf("the disabled subscription's receiver got %d requests, want none", n)
	}
	if ds := w.deliveries(t, disabled); len(ds) != 0 {
		t.Errorf("the disabled subscription has delivery records %+v, want none", ds)
	}
}

func TestRemoveWorkspace_DeletesItsSubscriptionsInTheRemovalTransaction(t *testing.T) {
	t.Parallel()
	errRollback := errors.New("the removal failed")
	for _, tc := range []struct {
		name     string
		rollback bool
	}{
		{"committed", false},
		{"rolled back", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, options{toml: insecure})
			delivering, failing, elsewhere := newInbox(t, answerOK), failingInbox(t), newInbox(t, answerOK)
			settled := w.subscribe(t, demo, map[string]any{"url": delivering.url(), "types": testTypes})
			retrying := w.subscribe(t, demo, map[string]any{"url": failing.url(), "types": testTypes})
			kept := w.subscribe(t, other, map[string]any{"url": elsewhere.url(), "types": testTypes})
			w.emit(t, demo, "parley.test.one", "")
			w.emit(t, other, "parley.test.one", "")
			w.retrying(t, retrying, 1)
			waitFor(t, "the other subscriptions to deliver", func() bool { return w.settled(t, settled) && w.settled(t, kept) })
			before, events := w.webhookRows(t), w.webhookEvents(t)

			ctx := context.Background()
			err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
				if err := w.hooks.RemoveWorkspace(ctx, tx, demo); err != nil {
					return err
				}
				if tc.rollback {
					return errRollback
				}
				return nil
			})
			if tc.rollback {
				if !errors.Is(err, errRollback) {
					t.Fatalf("the removal transaction = %v, want it rolled back", err)
				}
				if after := w.webhookRows(t); !reflect.DeepEqual(after, before) {
					t.Errorf("after the rollback the rows are\n%v\nwant\n%v", after, before)
				}
				// The loops go on.
				seq := w.emit(t, demo, "parley.test.two", "")
				waitFor(t, "the kept subscription to deliver the next event", func() bool { return slices.Contains(delivering.seqs(), seq) })
				return
			}
			if err != nil {
				t.Fatalf("the removal transaction: %v", err)
			}
			if n := w.count(t, demo); n != 0 {
				t.Errorf("demo has %d subscriptions after the removal, want none", n)
			}
			for _, id := range []string{settled, retrying} {
				if ds := w.deliveries(t, id); len(ds) != 0 {
					t.Errorf("removed subscription %s has delivery records %+v, want none", id, ds)
				}
			}
			after := w.webhookRows(t)
			for name, rows := range map[string]map[string]string{"before": before, "after": after} {
				for key := range rows {
					if !strings.Contains(key, kept) {
						delete(rows, key)
					}
				}
				if len(rows) == 0 {
					t.Fatalf("%s the removal the subscription of other has no rows", name)
				}
			}
			if !reflect.DeepEqual(after, before) {
				t.Errorf("the subscription of other is\n%v\nafter the removal, want\n%v", after, before)
			}
			if n := w.webhookEvents(t); n != events {
				t.Errorf("the log holds %d parley.webhook.* events after the removal, want %d as before", n, events)
			}

			// Nothing more is sent for demo: not its next event, nor the
			// retry of the removed pending delivery.
			sent := len(delivering.requests()) + len(failing.requests())
			w.emit(t, demo, "parley.test.two", "")
			w.clock.advance(time.Hour)
			seq := w.emit(t, other, "parley.test.two", "")
			waitFor(t, "the subscription of other to deliver its next event", func() bool { return slices.Contains(elsewhere.seqs(), seq) })
			time.Sleep(20 * time.Millisecond)
			if n := len(delivering.requests()) + len(failing.requests()); n != sent {
				t.Errorf("the receivers of demo got %d requests after the removal, want none", n-sent)
			}
		})
	}
}

// webhookRows is every row of the webhook tables, every column, keyed by
// the table and the row's id with its subscription's id.
func (w *world) webhookRows(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, q := range []struct{ table, query string }{
		{"subscription", `SELECT id, id, * FROM webhook_subscriptions`},
		{"delivery", `SELECT id, subscription_id, * FROM webhook_deliveries`},
	} {
		rows, err := w.Store.Main().Query(q.query)
		if err != nil {
			t.Fatalf("read the %s rows: %v", q.table, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(vals[2:])
			if err != nil {
				t.Fatal(err)
			}
			out[fmt.Sprintf("%s %s of %s", q.table, vals[0], vals[1])] = string(raw)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	return out
}
