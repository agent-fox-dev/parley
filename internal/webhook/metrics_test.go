package webhook_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/metrics"
)

// scrape reads the hub's metrics: the value of each series, keyed by the
// series as the exposition writes it, such as
// parley_webhook_deliveries_total{result="ok"}.
func (w *world) scrape(t *testing.T) map[string]float64 {
	t.Helper()
	status, raw := w.Call(t, http.MethodGet, "/metrics", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d %s", status, raw)
	}
	out := map[string]float64{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("metrics line %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

// deliveriesTotal is the series of parley_webhook_deliveries_total with the
// result result.
func deliveriesTotal(result string) string {
	return metrics.WebhookDeliveriesTotal + `{result="` + result + `"}`
}

func TestMetrics_CountEveryAttemptAndTheEventsQueued(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure + "timeout = \"60s\"\n", hubMetrics: true})

	// Delivered at once.
	quick := answeringWith(http.StatusOK)
	quickIn := newInbox(t, quick.answer)
	atOnce := w.subscribe(t, demo, map[string]any{"url": quickIn.url(), "types": []string{"parley.test.once"}})
	w.emit(t, demo, "parley.test.once", "")
	waitFor(t, "the first event to be delivered", func() bool { return w.settled(t, atOnce) })

	// Failing twice, then delivered.
	flaky := answeringWith(http.StatusInternalServerError)
	twice := w.subscribe(t, demo, map[string]any{"url": newInbox(t, flaky.answer).url(), "types": []string{"parley.test.twice"}})
	w.emit(t, demo, "parley.test.twice", "")
	first := w.retrying(t, twice, 1)
	w.clock.advance(timeAt(first.nextAttemptAt.Int64).Sub(w.clock.Now()))
	second := w.retrying(t, twice, 2)
	flaky.set(http.StatusOK)
	w.clock.advance(timeAt(second.nextAttemptAt.Int64).Sub(w.clock.Now()))
	waitFor(t, "the third attempt to deliver the event", func() bool { return w.settled(t, twice) })

	// Given up a day after its first attempt.
	lost := w.subscribe(t, demo, map[string]any{"url": failingInbox(t).url(), "types": []string{"parley.test.lost"}})
	w.emit(t, demo, "parley.test.lost", "")
	w.retrying(t, lost, 1)
	w.giveUpFirst(t, lost)
	givenUp := w.deliveries(t, lost)[0]
	if givenUp.status != "failed" {
		t.Fatalf("the delivery of the lost event = %+v, want it given up", givenUp)
	}

	// A successful ping, a failed ping and a failed redelivery.
	if status, raw := w.Call(t, http.MethodPost, hookPath(demo, atOnce)+"/ping", w.ownerKey, nil); status != http.StatusOK ||
		decode(t, raw)["status"] != "succeeded" {
		t.Fatalf("ping = %d %s, want 200 succeeded", status, raw)
	}
	unreachable := w.subscribe(t, demo, map[string]any{"url": closedURL(t), "types": []string{"parley.test.never"}})
	if status, raw := w.Call(t, http.MethodPost, hookPath(demo, unreachable)+"/ping", w.ownerKey, nil); status != http.StatusOK ||
		decode(t, raw)["status"] != "failed" {
		t.Fatalf("ping of an unreachable receiver = %d %s, want 200 failed", status, raw)
	}
	quick.set(http.StatusInternalServerError)
	var event string
	for _, d := range w.deliveries(t, atOnce) {
		if d.seq != 0 {
			event = d.id
		}
	}
	if status, raw := w.Call(t, http.MethodPost, redeliverPath(atOnce, event), w.ownerKey, nil); status != http.StatusOK ||
		decode(t, raw)["status"] != "failed" {
		t.Fatalf("redeliver to a failing receiver = %d %s, want 200 failed", status, raw)
	}

	// The loop reports an attempt once it has recorded it.
	want := map[string]float64{"ok": 3, "retry": 2 + float64(givenUp.attempts-1), "failed": 3}
	var got map[string]float64
	counted := func() bool {
		got = w.scrape(t)
		for result, n := range want {
			if got[deliveriesTotal(result)] != n {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(10 * time.Second)
	for !counted() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for result, n := range want {
		if got[deliveriesTotal(result)] != n {
			t.Errorf("%s = %v, want %v", deliveriesTotal(result), got[deliveriesTotal(result)], n)
		}
	}

	// The queue: events after the position of each subscription that may
	// receive them. The receivers' host does not answer, so each loop holds
	// its first attempt and gets no further.
	w.subscribe(t, demo, map[string]any{"url": "https://ci.example/a", "types": []string{"parley.queue.active"}})
	failing := w.subscribe(t, demo, map[string]any{"url": "https://ci.example/f", "types": []string{"parley.queue.failing"}})
	if _, err := w.Store.Main().Exec(`UPDATE webhook_subscriptions SET state = 'failing' WHERE id = ?`, failing); err != nil {
		t.Fatal(err)
	}
	disabled := w.subscribe(t, demo, map[string]any{"url": "https://ci.example/d", "types": []string{"parley.queue.disabled"}})
	w.patch(t, disabled, map[string]any{"active": false})
	for typ, n := range map[string]int{"parley.queue.active": 4, "parley.queue.failing": 2, "parley.queue.disabled": 5} {
		for range n {
			w.emit(t, demo, typ, "")
		}
	}
	if got := w.scrape(t)[metrics.WebhookQueueDepth]; got != 6 {
		t.Errorf("%s = %v, want 6: 4 of the active subscription, 2 of the failing one, none of the disabled one",
			metrics.WebhookQueueDepth, got)
	}
}

func TestMetrics_CountAnAttemptOnceWhenItsDeliveryEndedMeanwhile(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	for i, tc := range []struct {
		name   string
		answer int
		result string
	}{
		{"an attempt that succeeds", http.StatusOK, "ok"},
		{"an attempt that fails", http.StatusInternalServerError, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrived, release := make(chan struct{}, 1), make(chan struct{})
			in := newInbox(t, func(_ delivered, stop <-chan struct{}) int {
				arrived <- struct{}{}
				select {
				case <-release:
				case <-stop:
				}
				return tc.answer
			})
			typ := "parley.test.ended" + strconv.Itoa(i)
			id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": []string{typ}})
			w.emit(t, demo, typ, "")
			<-arrived
			before := map[string]float64{}
			for _, r := range []string{"ok", "retry", "failed"} {
				before[r] = w.metrics.deliveries(r)
			}

			// Switched off while the receiver holds the attempt: the
			// delivery ends, and no retry follows whatever the receiver says.
			w.switchOff(t, id)
			close(release)
			total := func() float64 {
				return w.metrics.deliveries("ok") + w.metrics.deliveries("retry") + w.metrics.deliveries("failed")
			}
			waitFor(t, "the attempt to be counted", func() bool { return total() > before["ok"]+before["retry"]+before["failed"] })
			time.Sleep(20 * time.Millisecond)
			for _, r := range []string{"ok", "retry", "failed"} {
				want := before[r]
				if r == tc.result {
					want++
				}
				if got := w.metrics.deliveries(r); got != want {
					t.Errorf("deliveries reported %s = %v, want %v", r, got, want)
				}
			}
			if ds := w.deliveries(t, id); len(ds) != 1 || ds[0].status != "failed" || ds[0].attempts != 0 {
				t.Errorf("delivery records = %+v, want the one ended by the switch-off, its attempt unrecorded", ds)
			}
		})
	}
}
