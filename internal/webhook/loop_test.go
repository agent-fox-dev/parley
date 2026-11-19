package webhook_test

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
)

// get reads the subscription id of slug through the route, pending
// included.
func (w *world) get(t *testing.T, slug, id string) map[string]any {
	t.Helper()
	status, raw := w.Call(t, http.MethodGet, hookPath(slug, id), w.ownerKey, nil)
	if status != http.StatusOK {
		t.Fatalf("get %s = %d %s, want 200", id, status, raw)
	}
	return decode(t, raw)
}

func TestDelivery_SendsANewEventAtOnceAndRecordsItsSuccess(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	in := newInbox(t, answerOK)
	id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": []string{"*"}})

	// The clock never moves: the first attempt waits for no timer.
	seq := w.emit(t, demo, "parley.change.open", "")
	waitFor(t, "the receiver's first request", func() bool { return len(in.requests()) > 0 })
	if got := in.requests()[0]; got.seq != seq || got.typ != "parley.change.open" {
		t.Fatalf("first request carries seq %d type %s, want %d parley.change.open", got.seq, got.typ, seq)
	}
	waitFor(t, "the delivery to succeed", func() bool { return w.settled(t, id) })

	ds := w.deliveries(t, id)
	if len(ds) != 1 {
		t.Fatalf("delivery records = %+v, want one", ds)
	}
	d := ds[0]
	at := w.clock.Now().UnixNano()
	if d.seq != seq || d.typ != "parley.change.open" || d.status != "succeeded" || d.attempts != 1 ||
		d.responseCode.Int64 != http.StatusOK || d.err.Valid || d.nextAttemptAt.Valid || d.lastAttemptAt.Int64 != at {
		t.Errorf("delivery = %+v, want seq %d, succeeded after 1 attempt at %d with 200, no error, no next attempt", d, seq, at)
	}
	if got := in.requests()[0].delivery; got != d.id {
		t.Errorf("Parley-Delivery = %s, want the record's id %s", got, d.id)
	}
	if r := w.row(t, id); r.position != seq {
		t.Errorf("position = %d, want %d", r.position, seq)
	}
	obj := w.get(t, demo, id)
	if obj["last_status"] != "succeeded" || obj["last_delivery_at"] != w.clock.stamp() || obj["failing_since"] != nil ||
		obj["pending"] != float64(0) {
		t.Errorf("subscription = %v, want last_status succeeded, last_delivery_at %s, failing_since null, pending 0", obj, w.clock.stamp())
	}
	if got := w.metrics.deliveries("ok"); got != 1 {
		t.Errorf("deliveries reported ok = %v, want 1", got)
	}
	if n := len(in.requests()); n != 1 {
		t.Errorf("the receiver got %d requests, want 1", n)
	}
}

func TestDelivery_SendsExactlyTheEventsAnOwnerMaySeeThatMatchTheTypes(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	const fork = "demo--t1"
	w.AddWorkspace(t, access.Record{Slug: fork, Kind: access.KindFork, ParentSlug: demo, OwnerID: w.owner, OrgID: w.org, Status: access.StatusReady})
	types := []string{"parley.change.*", "parley.secret.set", "parley.webhook.*", "parley.fork.expire"}
	withForks, withoutForks := newInbox(t, answerOK), newInbox(t, answerOK)
	subWith := w.subscribe(t, demo, map[string]any{"url": withForks.url(), "types": types, "include_forks": true})
	subWithout := w.subscribe(t, demo, map[string]any{"url": withoutForks.url(), "types": types, "include_forks": false})
	// The second create is a parley.webhook.* event of demo the first
	// subscription receives; the rows below are written after it.
	from := w.lastSeq(t)

	rows := []struct {
		name         string
		write        func() int64
		withForks    bool
		withoutForks bool
	}{
		{"a change event of the workspace", func() int64 { return w.emit(t, demo, "parley.change.open", "") }, true, true},
		{"a type it does not name", func() int64 { return w.emit(t, demo, "parley.git.push", "") }, false, false},
		{"an owner-only secret event", func() int64 { return w.emit(t, demo, "parley.secret.set", "") }, true, true},
		{"an owner-only token event of a type it does not name", func() int64 { return w.emit(t, demo, "parley.token.bind", "") }, false, false},
		{"an owner-only webhook event", func() int64 { return w.emit(t, demo, "parley.webhook.update", "") }, true, true},
		{"a change event of a task fork", func() int64 { return w.emit(t, fork, "parley.change.open", "") }, true, false},
		{"an event of a task fork whose record is gone", func() int64 {
			if _, err := w.Store.Main().Exec(`DELETE FROM workspaces WHERE slug = ?`, fork); err != nil {
				t.Fatalf("delete the fork's record: %v", err)
			}
			return w.emit(t, fork, "parley.fork.expire", demo)
		}, true, false},
		{"an event of another workspace", func() int64 { return w.emit(t, other, "parley.change.open", "") }, false, false},
		{"an event without a workspace", func() int64 {
			return w.write(t, event.Input{Type: "parley.secret.set", ActorType: event.ActorAPIKey, ActorID: &w.owner,
				ResourceType: "secret", ResourceID: "s1", Metadata: map[string]any{"scope": "user"}})
		}, false, false},
	}
	var wantWith, wantWithout []int64
	for _, r := range rows {
		seq := r.write()
		if r.withForks {
			wantWith = append(wantWith, seq)
		}
		if r.withoutForks {
			wantWithout = append(wantWithout, seq)
		}
	}
	last := w.lastSeq(t)

	for _, tc := range []struct {
		name string
		id   string
		in   *inbox
		want []int64
	}{
		{"with forks", subWith, withForks, wantWith},
		{"without forks", subWithout, withoutForks, wantWithout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waitFor(t, "the loop to examine every event", func() bool { return w.settled(t, tc.id) })
			var got []int64
			for _, seq := range tc.in.seqs() {
				if seq > from {
					got = append(got, seq)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("delivered seqs = %v, want %v", got, tc.want)
			}
			var records []int64
			for _, d := range w.deliveries(t, tc.id) {
				if d.seq > from {
					records = append(records, d.seq)
				}
			}
			if !reflect.DeepEqual(records, tc.want) {
				t.Errorf("delivery records of seqs %v, want one per delivered event %v", records, tc.want)
			}
			if r := w.row(t, tc.id); r.position != last {
				t.Errorf("position = %d, want the last seq %d", r.position, last)
			}
		})
	}
}

func TestDelivery_SendsNothingWrittenBeforeTheSubscription(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	in := newInbox(t, answerOK)
	for range 3 {
		w.emit(t, demo, "parley.change.open", "")
	}
	id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": []string{"*"}})
	e4 := w.emit(t, demo, "parley.change.open", "")

	waitFor(t, "the loop to deliver the event", func() bool { return w.settled(t, id) })
	if got := w.get(t, demo, id)["pending"]; got != float64(0) {
		t.Errorf("pending = %v, want 0", got)
	}
	if got := in.seqs(); !reflect.DeepEqual(got, []int64{e4}) {
		t.Errorf("the receiver got seqs %v, want only %d", got, e4)
	}
}

func TestDelivery_KeepsDeliveringToOtherSubscriptionsWhileOneReceiverHangs(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure + "timeout = \"60s\"\n"})
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	hanging := newInbox(t, func(_ delivered, stop <-chan struct{}) int {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-stop:
		}
		return http.StatusOK
	})
	healthy := newInbox(t, answerOK)
	types := []string{"parley.test.*"}
	subA := w.subscribe(t, demo, map[string]any{"url": hanging.url(), "types": types})
	subB := w.subscribe(t, demo, map[string]any{"url": healthy.url(), "types": types})

	e1 := w.emit(t, demo, "parley.test.event", "")
	e2 := w.emit(t, demo, "parley.test.event", "")
	e3 := w.emit(t, demo, "parley.test.event", "")
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("waited 10 s for receiver A's first request")
	}
	waitFor(t, "receiver B to get every event", func() bool { return len(healthy.requests()) >= 3 })

	if got := healthy.seqs(); !reflect.DeepEqual(got, []int64{e1, e2, e3}) {
		t.Errorf("receiver B got seqs %v, want %v", got, []int64{e1, e2, e3})
	}
	if got := hanging.seqs(); !reflect.DeepEqual(got, []int64{e1}) {
		t.Errorf("receiver A got seqs %v while it hangs in e1, want only %d", got, e1)
	}
	ds := w.deliveries(t, subA)
	if len(ds) != 1 || ds[0].seq != e1 || ds[0].status != "pending" {
		t.Errorf("A's deliveries = %+v, want only e1 %d pending", ds, e1)
	}
	if got := w.row(t, subA).position; got >= e1 {
		t.Errorf("A's position = %d while e1 %d is pending, want before it", got, e1)
	}
	if got := w.get(t, demo, subA)["pending"]; got != float64(3) {
		t.Errorf("A's pending = %v, want 3: e1 once, e2 and e3", got)
	}
	waitFor(t, "B's deliveries to succeed", func() bool { return w.settled(t, subB) })
	close(release)
}

func TestDelivery_SkipsEventsThatLeftTheLog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// answer is the status of the attempt of the first event, which the
		// receiver holds while the log moves on.
		answer int
		// status is that event's delivery once the loop has gone on.
		status string
	}{
		{"a position older than the oldest event", http.StatusOK, "succeeded"},
		{"a pending delivery whose event is gone", http.StatusInternalServerError, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, options{toml: insecure})
			held, release := make(chan struct{}, 1), make(chan int, 1)
			var heldOne atomic.Bool
			in := newInbox(t, func(_ delivered, stop <-chan struct{}) int {
				if !heldOne.CompareAndSwap(false, true) {
					return http.StatusOK
				}
				held <- struct{}{}
				select {
				case status := <-release:
					return status
				case <-stop:
					return http.StatusServiceUnavailable
				}
			})
			id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": []string{"parley.test.*"}})

			early := w.emit(t, demo, "parley.test.event", "")
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("waited 10 s for the first attempt")
			}
			gone := w.emit(t, demo, "parley.test.event", "")
			w.clock.advance(time.Hour)
			oldest := w.emit(t, demo, "parley.test.event", "")
			if got, err := w.log.Expire(context.Background(), w.clock.Now()); err != nil || got != oldest {
				t.Fatalf("Expire = %d, %v, want oldest seq %d", got, err, oldest)
			}
			release <- tc.answer

			waitFor(t, "the first attempt to be recorded", func() bool {
				ds := w.deliveries(t, id)
				return len(ds) > 0 && ds[0].attempts > 0
			})
			before := w.row(t, id)
			// A retry waits on the clock: move it past any wait.
			w.clock.advance(time.Hour)
			waitFor(t, "the receiver to get the oldest event", func() bool { return slices.Contains(in.seqs(), oldest) })
			waitFor(t, "the loop to settle", func() bool { return w.settled(t, id) })

			if got := slices.Compact(in.seqs()); !reflect.DeepEqual(got, []int64{early, oldest}) {
				t.Errorf("the receiver got seqs %v, want %d then %d (never %d)", got, early, oldest, gone)
			}
			ds := w.deliveries(t, id)
			if len(ds) != 2 || ds[0].seq != early || ds[0].status != tc.status || ds[0].nextAttemptAt.Valid ||
				ds[1].seq != oldest || ds[1].status != "succeeded" {
				t.Errorf("deliveries = %+v, want %d %s without a next attempt, then %d succeeded", ds, early, tc.status, oldest)
			}
			if got := w.metrics.deliveries("failed"); got != 1 {
				t.Errorf("deliveries reported failed = %v, want 1", got)
			}
			after := w.row(t, id)
			if after.state != "active" || after.failingSince != before.failingSince {
				t.Errorf("state %s, failing_since %v, want active and %v as before", after.state, after.failingSince, before.failingSince)
			}
			if after.position != oldest {
				t.Errorf("position = %d, want %d", after.position, oldest)
			}
			if got := w.events(t, "parley.webhook.failing"); len(got) != 0 {
				t.Errorf("the log holds parley.webhook.failing events %+v, want none", got)
			}
		})
	}
}
