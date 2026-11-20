package webhook_test

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

// retryWaits are the waits after the first twelve failed attempts of a
// delivery; every later one waits an hour.
var retryWaits = []time.Duration{
	10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 320 * time.Second,
	640 * time.Second, 1280 * time.Second, 2560 * time.Second, time.Hour, time.Hour, time.Hour,
}

func TestRetry_WaitsLongerAfterEachFailureUntilADayAfterTheFirstAttempt(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	in := newInbox(t, func(delivered, <-chan struct{}) int { return http.StatusInternalServerError })
	id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": []string{"parley.test.*"}})
	w.emit(t, demo, "parley.test.event", "")

	var first, deadline time.Time
	for n := 1; ; n++ {
		d := w.retrying(t, id, n)
		last, next := time.Unix(0, d.lastAttemptAt.Int64).UTC(), time.Unix(0, d.nextAttemptAt.Int64).UTC()
		if n == 1 {
			first, deadline = last, last.Add(24*time.Hour)
		}
		wait := time.Hour
		if n <= len(retryWaits) {
			wait = retryWaits[n-1]
		}
		want := last.Add(wait)
		if want.After(deadline) {
			want = deadline
		}
		if !next.Equal(want) {
			t.Fatalf("after failure %d at %s the next attempt is at %s, want %s", n, last, next, want)
		}
		if got := len(in.requests()); got != n {
			t.Fatalf("after failure %d the receiver got %d requests, want %d", n, got, n)
		}
		r := w.row(t, id)
		if r.state != "active" || r.lastStatus.String != "failed" || r.lastDeliveryAt.Int64 != last.UnixNano() ||
			r.failingSince.Int64 != first.UnixNano() {
			t.Fatalf("after failure %d the subscription is %+v, want active, last_status failed, last_delivery_at %s, failing_since %s",
				n, r, last, first)
		}
		if n == len(retryWaits) {
			waitFor(t, "twelve retries to be reported", func() bool { return w.metrics.deliveries("retry") == 12 })
		}
		if last.Add(time.Hour).After(deadline) {
			// The last attempt within the day: what its failure does is the
			// give-up's, not the schedule's.
			if !next.Equal(deadline) {
				t.Fatalf("after failure %d at %s the next attempt is at %s, want a day after the first attempt %s", n, last, next, deadline)
			}
			return
		}
		if n <= len(retryWaits) {
			w.clock.advance(next.Sub(w.clock.Now()) - time.Second)
			time.Sleep(20 * time.Millisecond)
			if got := len(in.requests()); got != n {
				t.Fatalf("a second before attempt %d is due the receiver got %d requests, want %d", n+1, got, n)
			}
		}
		w.clock.advance(next.Sub(w.clock.Now()))
	}
}

func TestRetry_CountsTheWaitFromTheEndOfTheFailedAttemptToTheNanosecond(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	// The attempt starts 900 ms into a second and the receiver takes 600 ms
	// to answer it.
	w.clock.advance(900 * time.Millisecond)
	in := newInbox(t, func(d delivered, _ <-chan struct{}) int {
		w.clock.advance(600 * time.Millisecond)
		return http.StatusInternalServerError
	})
	id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": testTypes})
	w.emit(t, demo, "parley.test.event", "")

	d := w.retrying(t, id, 1)
	ended := w.clock.Now()
	if next := timeAt(d.nextAttemptAt.Int64); !next.Equal(ended.Add(10 * time.Second)) {
		t.Fatalf("an attempt that failed at %s schedules the next at %s, want 10 s later", ended.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano))
	}
	w.clock.advance(10*time.Second - time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if got := len(in.requests()); got != 1 {
		t.Fatalf("a millisecond before the retry is due the receiver got %d requests, want 1", got)
	}
	w.clock.advance(time.Millisecond)
	w.retrying(t, id, 2)
}

// testTypes is the filter of the subscriptions of the state tests: the
// events the tests write, not the subscription's own parley.webhook.* events.
var testTypes = []string{"parley.test.*"}

// failingInbox is a receiver that answers every request 500.
func failingInbox(t *testing.T) *inbox {
	t.Helper()
	return newInbox(t, func(delivered, <-chan struct{}) int { return http.StatusInternalServerError })
}

// patch changes the subscription id of demo as the owner with body, fails
// the test unless it is answered 200, and returns the object.
func (w *world) patch(t *testing.T, id string, body map[string]any) map[string]any {
	t.Helper()
	status, raw := w.Call(t, http.MethodPatch, hookPath(demo, id), w.ownerKey, body)
	if status != http.StatusOK {
		t.Fatalf("PATCH %v = %d %s, want 200", body, status, raw)
	}
	return decode(t, raw)
}

// giveUpFirst moves the clock from attempt to attempt until the first
// delivery of the subscription id is given up and the next one has had its
// first attempt.
func (w *world) giveUpFirst(t *testing.T, id string) {
	t.Helper()
	w.retryWhile(t, id, func(time.Time) bool { return w.deliveries(t, id)[0].status == "pending" })
}

// webhookEvents is the number of parley.webhook.* events in the log.
func (w *world) webhookEvents(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.Store.Main().QueryRow(`SELECT count(*) FROM hub_events WHERE type LIKE 'parley.webhook.%'`).Scan(&n); err != nil {
		t.Fatalf("count the webhook events: %v", err)
	}
	return n
}

// hostOf is the host of the receiver's URL, the one part of it an event
// names.
func hostOf(t *testing.T, in *inbox) string {
	t.Helper()
	u, err := url.Parse(in.url())
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

// timeAt is the time of a stored timestamp.
func timeAt(nanos int64) time.Time { return time.Unix(0, nanos).UTC() }

func TestGiveUp_EndsAnEventADayAfterItsFirstAttemptAndGoesOnWithTheNext(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	in := newInbox(t, func(delivered, <-chan struct{}) int { return http.StatusServiceUnavailable })
	id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": testTypes})
	e1 := w.emit(t, demo, "parley.test.one", "")
	e2 := w.emit(t, demo, "parley.test.two", "")
	first := timeAt(w.retrying(t, id, 1).lastAttemptAt.Int64)

	w.giveUpFirst(t, id)
	end := first.Add(24 * time.Hour)
	ds := w.deliveries(t, id)
	if len(ds) != 2 {
		t.Fatalf("delivery records = %+v, want e1 and e2", ds)
	}
	if d := ds[0]; d.seq != e1 || d.status != "failed" || d.nextAttemptAt.Valid || !timeAt(d.lastAttemptAt.Int64).Equal(end) {
		t.Errorf("delivery of e1 = %+v, want failed at %s with no next attempt", d, end)
	}
	if d := ds[1]; d.seq != e2 || d.status != "pending" || d.attempts != 1 || !timeAt(d.lastAttemptAt.Int64).Equal(end) {
		t.Errorf("delivery of e2 = %+v, want pending after its first attempt at %s, the instant e1 was given up", d, end)
	}
	if r := w.row(t, id); r.state != "failing" || r.position != e1 || !timeAt(r.failingSince.Int64).Equal(first) {
		t.Errorf("subscription = %+v, want failing at position %d (e1) with failing_since %s", r, e1, first)
	}
	failing := w.events(t, "parley.webhook.failing")
	if len(failing) != 1 {
		t.Fatalf("the log holds %d parley.webhook.failing events, want 1", len(failing))
	}
	want := map[string]any{
		"webhook_id": id, "host": hostOf(t, in), "seq": float64(e1), "failing_since": first.Format(time.RFC3339),
		"response_code": float64(http.StatusServiceUnavailable), "error": nil,
	}
	if e := failing[0]; e.ActorType != "system" || e.ActorID.Valid || e.Workspace.String != demo || e.ResourceType != "event_webhook" ||
		e.ResourceID != id || !reflect.DeepEqual(e.Metadata, want) {
		t.Errorf("failing event = %+v, want actor system on %s, resource event_webhook %s, metadata %v", e, demo, id, want)
	}
	waitFor(t, "the given-up event to be reported failed", func() bool { return w.metrics.deliveries("failed") == 1 })

	w.retryWhile(t, id, func(time.Time) bool { return true })
	ds = w.deliveries(t, id)
	if d := ds[1]; d.status != "failed" || d.nextAttemptAt.Valid || !timeAt(d.lastAttemptAt.Int64).Equal(end.Add(24*time.Hour)) {
		t.Errorf("delivery of e2 = %+v, want failed a day after its own first attempt", d)
	}
	if r := w.row(t, id); r.state != "failing" || r.position < e2 {
		t.Errorf("subscription = %+v, want failing past e2 %d", r, e2)
	}
	if n := len(w.events(t, "parley.webhook.failing")); n != 1 {
		t.Errorf("the log holds %d parley.webhook.failing events after a second give-up, want still 1", n)
	}
	waitFor(t, "both given-up events to be reported failed", func() bool { return w.metrics.deliveries("failed") == 2 })
}

func TestRecovery_ASuccessClearsFailingSinceAndMakesTheSubscriptionActive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state string
		// fail makes the attempts of the subscription id fail until it is in
		// state, and returns its pending delivery.
		fail func(t *testing.T, w *world, id string) deliveryRow
	}{
		{"an active subscription after two failed attempts", "active", func(t *testing.T, w *world, id string) deliveryRow {
			w.emit(t, demo, "parley.test.one", "")
			d := w.retrying(t, id, 1)
			w.clock.advance(timeAt(d.nextAttemptAt.Int64).Sub(w.clock.Now()))
			return w.retrying(t, id, 2)
		}},
		{"a failing subscription with the next event pending", "failing", func(t *testing.T, w *world, id string) deliveryRow {
			w.emit(t, demo, "parley.test.one", "")
			w.emit(t, demo, "parley.test.two", "")
			w.giveUpFirst(t, id)
			return w.retrying(t, id, 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, options{toml: insecure})
			receiver := answeringWith(http.StatusInternalServerError)
			in := newInbox(t, receiver.answer)
			id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": testTypes})
			d := tc.fail(t, w, id)
			if r := w.row(t, id); r.state != tc.state || !r.failingSince.Valid {
				t.Fatalf("before the success the subscription is %+v, want %s with failing_since set", r, tc.state)
			}
			events := w.webhookEvents(t)

			receiver.set(http.StatusOK)
			w.clock.advance(timeAt(d.nextAttemptAt.Int64).Sub(w.clock.Now()))
			waitFor(t, "the pending delivery to succeed", func() bool { return w.settled(t, id) })
			if r := w.row(t, id); r.state != "active" || r.failingSince.Valid || r.lastStatus.String != "succeeded" {
				t.Errorf("after the success the subscription is %+v, want active, no failing_since, last_status succeeded", r)
			}
			if n := w.webhookEvents(t); n != events {
				t.Errorf("the success wrote %d parley.webhook.* events, want none", n-events)
			}
		})
	}
}

func TestDisable_SwitchesOffASubscriptionFailingForThreeDays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		events int
	}{
		{"with events still to send", 4},
		{"with nothing left to send since its event was given up", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, options{toml: insecure})
			in := failingInbox(t)
			id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": testTypes})
			for range tc.events {
				w.emit(t, demo, "parley.test.event", "")
			}
			w.retrying(t, id, 1)
			since := timeAt(w.row(t, id).failingSince.Int64)
			end := since.Add(72 * time.Hour)

			w.retryWhile(t, id, func(next time.Time) bool { return next.Before(end.Add(-time.Second)) })
			w.clock.advance(end.Add(-time.Second).Sub(w.clock.Now()))
			time.Sleep(20 * time.Millisecond)
			if r := w.row(t, id); r.state != "failing" {
				t.Fatalf("a second before three days of failing the subscription is %+v, want failing", r)
			}
			// The attempts due before the check that disables, the one that
			// gives an event up at the end of the third day included, are made
			// and recorded first, so that none is in flight when the
			// subscription is disabled.
			check := end.Add(61 * time.Second)
			w.retryWhile(t, id, func(next time.Time) bool { return next.Before(check) })
			var pending string
			for _, d := range w.deliveries(t, id) {
				if d.status == "pending" {
					pending = d.id
				}
			}

			w.clock.advance(check.Sub(w.clock.Now()))
			waitFor(t, "the subscription to be disabled", func() bool { return w.row(t, id).state == "disabled" })
			for _, d := range w.deliveries(t, id) {
				if d.status == "pending" || d.nextAttemptAt.Valid || d.id == pending && d.status != "failed" {
					t.Errorf("after the disabling delivery %+v is left, want every delivery ended with no next attempt", d)
				}
			}
			disabled := w.events(t, "parley.webhook.disabled")
			if len(disabled) != 1 {
				t.Fatalf("the log holds %d parley.webhook.disabled events, want 1", len(disabled))
			}
			want := map[string]any{"webhook_id": id, "host": hostOf(t, in), "failing_since": since.Format(time.RFC3339)}
			if e := disabled[0]; e.ActorType != "system" || e.ActorID.Valid || e.Workspace.String != demo || e.ResourceType != "event_webhook" ||
				e.ResourceID != id || !reflect.DeepEqual(e.Metadata, want) {
				t.Errorf("disabled event = %+v, want actor system on %s, resource event_webhook %s, metadata %v", e, demo, id, want)
			}

			sent := len(in.requests())
			w.clock.advance(3 * time.Hour)
			time.Sleep(20 * time.Millisecond)
			if n := len(in.requests()); n != sent {
				t.Errorf("the receiver got %d requests after the disabling, want none", n-sent)
			}
		})
	}
}

func TestSwitchOn_ResumesADisabledSubscriptionAfterTheSwitchAndKeepsAFailingOnesDelivery(t *testing.T) {
	t.Parallel()
	t.Run("a disabled subscription", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, options{toml: insecure})
		receiver := answeringWith(http.StatusInternalServerError)
		in := newInbox(t, receiver.answer)
		id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": testTypes})
		e0 := w.emit(t, demo, "parley.test.zero", "")
		w.retrying(t, id, 1)
		before := w.row(t, id)
		if obj := w.patch(t, id, map[string]any{"active": true}); obj["state"] != "active" {
			t.Errorf("switching on an active subscription answered %v, want state active", obj)
		}
		if r := w.row(t, id); r != before {
			t.Errorf("switching on an active subscription changed it to %+v from %+v", r, before)
		}
		w.patch(t, id, map[string]any{"active": false})
		receiver.set(http.StatusOK)
		w.emit(t, demo, "parley.test.one", "")
		w.emit(t, demo, "parley.test.two", "")

		obj := w.patch(t, id, map[string]any{"active": true})
		if obj["state"] != "active" || obj["failing_since"] != nil {
			t.Errorf("switching on answered %v, want state active and failing_since null", obj)
		}
		updates := w.events(t, "parley.webhook.update")
		if len(updates) != 3 {
			t.Fatalf("the log holds %d update events, want 3", len(updates))
		}
		on := updates[2]
		if m := on.Metadata; !reflect.DeepEqual(m["fields"], []any{"active"}) || m["state"] != "active" {
			t.Errorf("switch-on event metadata = %v, want fields [active] and state active", m)
		}
		if r := w.row(t, id); r.position != on.Seq {
			t.Errorf("position after the switch-on = %d, want the seq of its update event %d", r.position, on.Seq)
		}
		e3 := w.emit(t, demo, "parley.test.three", "")
		waitFor(t, "the loop to deliver the event written after the switch-on", func() bool { return w.settled(t, id) })
		if seqs := in.seqs(); !reflect.DeepEqual(seqs, []int64{e0, e3}) {
			t.Errorf("the receiver got seqs %v, want e0 %d before the switch-off and e3 %d after the switch-on alone", seqs, e0, e3)
		}
		if ds := w.deliveries(t, id); len(ds) != 2 || ds[0].seq != e0 || ds[1].seq != e3 {
			t.Errorf("delivery records = %+v, want e0 and e3 alone", ds)
		}
		if obj := w.get(t, demo, id); obj["state"] != "active" || obj["failing_since"] != nil {
			t.Errorf("GET = %v, want state active and failing_since null", obj)
		}
	})
	t.Run("a failing subscription", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, options{toml: insecure})
		in := failingInbox(t)
		id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": testTypes})
		w.emit(t, demo, "parley.test.one", "")
		w.emit(t, demo, "parley.test.two", "")
		w.giveUpFirst(t, id)
		pending := w.retrying(t, id, 1)
		before := w.row(t, id)

		obj := w.patch(t, id, map[string]any{"active": true})
		if obj["state"] != "active" || obj["failing_since"] != nil {
			t.Errorf("switching on answered %v, want state active and failing_since null", obj)
		}
		updates := w.events(t, "parley.webhook.update")
		if len(updates) != 1 || !reflect.DeepEqual(updates[0].Metadata["fields"], []any{"active"}) || updates[0].Metadata["state"] != "active" {
			t.Errorf("update events = %+v, want one with fields [active] and state active", updates)
		}
		if r := w.row(t, id); r.state != "active" || r.failingSince.Valid || r.position != before.position {
			t.Errorf("subscription = %+v, want active with no failing_since at position %d as before", r, before.position)
		}
		if ds := w.deliveries(t, id); ds[1] != pending {
			t.Errorf("the pending delivery is %+v after the switch-on, want it as it was: %+v", ds[1], pending)
		}
	})
}

func TestSwitchOff_EndsThePendingDeliveryAndSendsNothingMore(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	in := failingInbox(t)
	id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": testTypes})
	w.emit(t, demo, "parley.test.one", "")
	pending := w.retrying(t, id, 1)

	if obj := w.patch(t, id, map[string]any{"active": false}); obj["state"] != "disabled" {
		t.Errorf("switching off answered %v, want state disabled", obj)
	}
	updates := w.events(t, "parley.webhook.update")
	if len(updates) != 1 || !reflect.DeepEqual(updates[0].Metadata["fields"], []any{"active"}) || updates[0].Metadata["state"] != "disabled" {
		t.Errorf("update events = %+v, want one with fields [active] and state disabled", updates)
	}
	ds := w.deliveries(t, id)
	if len(ds) != 1 || ds[0].id != pending.id || ds[0].status != "failed" || ds[0].nextAttemptAt.Valid || ds[0].attempts != 1 {
		t.Fatalf("delivery records = %+v, want the pending one failed with no next attempt", ds)
	}
	if r := w.row(t, id); r.state != "disabled" || !r.failingSince.Valid {
		t.Errorf("subscription = %+v, want disabled with failing_since kept", r)
	}

	if obj := w.patch(t, id, map[string]any{"active": false}); obj["state"] != "disabled" {
		t.Errorf("switching off a disabled subscription answered %v, want state disabled", obj)
	}
	if n := len(w.events(t, "parley.webhook.update")); n != 2 {
		t.Errorf("the log holds %d update events after a second switch-off, want 2", n)
	}

	w.emit(t, demo, "parley.test.two", "")
	w.clock.advance(2 * time.Hour)
	time.Sleep(20 * time.Millisecond)
	if n := len(in.requests()); n != 1 {
		t.Errorf("the receiver got %d requests, want the one before the switch-off", n)
	}
	if ds := w.deliveries(t, id); len(ds) != 1 {
		t.Errorf("delivery records = %+v, want none for the event written while disabled", ds)
	}
}
