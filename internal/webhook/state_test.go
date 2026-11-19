package webhook_test

import (
	"net/http"
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
