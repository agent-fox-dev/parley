package webhook_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/webhook"
	"github.com/google/uuid"
)

// seeded is a delivery record a test writes to the store as the loop or a
// ping would have; a zero time is a NULL column.
type seeded struct {
	id, typ, status string
	seq             int64
	attempts        int
	createdAt       time.Time
	lastAttemptAt   time.Time
	nextAttemptAt   time.Time
}

// seed stores d as a delivery of the subscription sub.
func (w *world) seed(t *testing.T, sub string, d seeded) {
	t.Helper()
	nanos := func(at time.Time) any {
		if at.IsZero() {
			return nil
		}
		return at.UnixNano()
	}
	_, err := w.Store.Main().Exec(`INSERT INTO webhook_deliveries (id, subscription_id, seq, type, status, attempts,
		response_code, duration_ms, created_at, first_attempt_at, last_attempt_at, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, ?, 200, 5, ?, ?, ?, ?)`,
		d.id, sub, d.seq, d.typ, d.status, d.attempts, d.createdAt.UnixNano(),
		nanos(d.lastAttemptAt), nanos(d.lastAttemptAt), nanos(d.nextAttemptAt))
	if err != nil {
		t.Fatalf("seed delivery %s: %v", d.id, err)
	}
}

// switchOff switches the subscription id off, so that no loop touches the
// records a test seeds for it.
func (w *world) switchOff(t *testing.T, id string) {
	t.Helper()
	if status, raw := w.Call(t, http.MethodPatch, hookPath(demo, id), w.ownerKey, map[string]any{"active": false}); status != http.StatusOK {
		t.Fatalf("switch off %s = %d %s", id, status, raw)
	}
}

// deliveryList reads one page of the deliveries of the subscription id of
// demo with the query query.
func (w *world) deliveryList(t *testing.T, id string, query url.Values) (int, listPage, []byte) {
	t.Helper()
	path := hookPath(demo, id) + "/deliveries"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	status, raw := w.Call(t, http.MethodGet, path, w.ownerKey, nil)
	var page listPage
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatalf("decode the page %s: %v", raw, err)
		}
	}
	return status, page, raw
}

// redeliverPath is the path of the redelivery of delivery of the
// subscription id of demo.
func redeliverPath(id, delivery string) string {
	return hookPath(demo, id) + "/deliveries/" + delivery + "/redeliver"
}

// recordMembers are the members of a delivery record, sorted.
var recordMembers = []string{"attempts", "created_at", "duration_ms", "error", "id", "last_attempt_at",
	"next_attempt_at", "response_code", "seq", "status", "type"}

func TestDeliveries_ListsTheSubscriptionsRecordsNewestFirst(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{})
	none := map[string]any{"url": "https://ci.example/x", "types": []string{"parley.none"}}
	id, otherID := w.subscribe(t, demo, none), w.subscribe(t, demo, none)
	w.switchOff(t, id)

	t0 := w.clock.Now()
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	pair := []string{uuid.NewString(), uuid.NewString()}
	sort.Sort(sort.Reverse(sort.StringSlice(pair)))
	oldest, older, pending := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, d := range []seeded{
		{id: oldest, typ: "parley.test.a", status: "succeeded", seq: 3, attempts: 1, createdAt: at(-5 * time.Minute), lastAttemptAt: at(-5 * time.Minute)},
		{id: older, typ: "parley.test.a", status: "succeeded", seq: 4, attempts: 1, createdAt: at(-4 * time.Minute), lastAttemptAt: at(-4 * time.Minute)},
		{id: pair[0], typ: "parley.webhook.ping", status: "failed", seq: 0, attempts: 1, createdAt: at(-3 * time.Minute), lastAttemptAt: at(-3 * time.Minute)},
		{id: pair[1], typ: "parley.test.a", status: "failed", seq: 5, attempts: 7, createdAt: at(-3 * time.Minute), lastAttemptAt: at(-2 * time.Minute)},
		{id: pending, typ: "parley.test.a", status: "pending", seq: 6, attempts: 1, createdAt: at(-time.Minute), lastAttemptAt: at(-time.Minute), nextAttemptAt: at(10 * time.Second)},
	} {
		w.seed(t, id, d)
	}
	w.seed(t, otherID, seeded{id: uuid.NewString(), typ: "parley.test.a", status: "succeeded", seq: 3, attempts: 1, createdAt: t0, lastAttemptAt: t0})
	newestFirst := []string{pending, pair[0], pair[1], older, oldest}

	status, page, raw := w.deliveryList(t, id, nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", status, raw)
	}
	if got := ids(page.Items); !reflect.DeepEqual(got, newestFirst) || page.NextCursor != nil {
		t.Errorf("ids = %v (next %v), want %v and no next cursor", got, page.NextCursor, newestFirst)
	}
	for _, it := range page.Items {
		keys := make([]string, 0, len(it))
		for k := range it {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, recordMembers) {
			t.Errorf("record %v has members %v, want %v", it["id"], keys, recordMembers)
		}
		if (it["status"] == "pending") != (it["next_attempt_at"] != nil) {
			t.Errorf("record %v: status %v with next_attempt_at %v", it["id"], it["status"], it["next_attempt_at"])
		}
	}

	var paged []string
	query := url.Values{"limit": {"2"}}
	for range 4 {
		status, page, raw := w.deliveryList(t, id, query)
		if status != http.StatusOK {
			t.Fatalf("list %v = %d %s, want 200", query, status, raw)
		}
		paged = append(paged, ids(page.Items)...)
		if page.NextCursor == nil {
			break
		}
		query.Set("cursor", *page.NextCursor)
	}
	if !reflect.DeepEqual(paged, newestFirst) {
		t.Errorf("pages of 2 = %v, want %v", paged, newestFirst)
	}

	for _, tc := range []struct {
		status string
		want   []string
	}{
		{"failed", []string{pair[0], pair[1]}},
		{"pending", []string{pending}},
		{"succeeded", []string{older, oldest}},
	} {
		_, page, _ := w.deliveryList(t, id, url.Values{"status": {tc.status}})
		if got := ids(page.Items); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("status=%s: ids = %v, want %v", tc.status, got, tc.want)
		}
	}
	status, _, raw = w.deliveryList(t, id, url.Values{"status": {"lost"}})
	if status != http.StatusBadRequest || errorOf(t, raw)["error_type"] != "invalid_request" {
		t.Errorf("status=lost = %d %s, want 400 invalid_request", status, raw)
	}
}

func TestRedeliver_SendsTheEventOnceMoreUnderItsDeliveryID(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	for i, tc := range []struct {
		name string
		// first is how the delivery ended: succeeded, failed, or failed by
		// a switch-off that left the subscription disabled.
		first string
		// now is the receiver's answer to the redelivery.
		now    int
		status string
		result string
	}{
		{"a failed delivery, the receiver now answering 200", "failed", http.StatusOK, "succeeded", "ok"},
		{"a succeeded delivery, the receiver now answering 500", "succeeded", http.StatusInternalServerError, "failed", "failed"},
		{"a failed delivery of a disabled subscription", "disabled", http.StatusOK, "succeeded", "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := answeringWith(http.StatusInternalServerError)
			if tc.first == "succeeded" {
				a.set(http.StatusOK)
			}
			in := newInbox(t, a.answer)
			typ := "parley.test.redeliver" + string(rune('a'+i))
			id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": []string{typ}})
			w.emit(t, demo, typ, "")
			if tc.first == "succeeded" {
				waitFor(t, "the event to be delivered", func() bool { return w.settled(t, id) })
			} else {
				w.retrying(t, id, 1)
				w.switchOff(t, id)
				if tc.first == "failed" {
					if status, raw := w.Call(t, http.MethodPatch, hookPath(demo, id), w.ownerKey, map[string]any{"active": true}); status != http.StatusOK {
						t.Fatalf("switch on = %d %s", status, raw)
					}
				}
			}
			original := w.deliveries(t, id)[0]
			first := in.requests()[0]
			row, rows := w.row(t, id), w.configured(t)
			counted := w.metrics.deliveries(tc.result)
			w.clock.advance(time.Minute)
			a.set(tc.now)

			status, raw := w.Call(t, http.MethodPost, redeliverPath(id, original.id), w.ownerKey, nil)
			if status != http.StatusOK {
				t.Fatalf("redeliver = %d %s, want 200", status, raw)
			}
			got := decode(t, raw)
			if got["id"] != original.id || got["attempts"] != float64(original.attempts+1) || got["status"] != tc.status ||
				got["response_code"] != float64(tc.now) || got["error"] != nil ||
				got["last_attempt_at"] != w.clock.stamp() || got["next_attempt_at"] != nil {
				t.Errorf("record = %v, want id %s, %d attempts, %s with code %d, last attempt %s, no next attempt",
					got, original.id, original.attempts+1, tc.status, tc.now, w.clock.stamp())
			}
			reqs := in.requests()
			last := reqs[len(reqs)-1]
			if len(reqs) != 2 || last.delivery != first.delivery || last.seq != first.seq || !bytes.Equal(last.body, first.body) {
				t.Errorf("the receiver got %d requests, the last %s seq %d %s; want 2, the last %s seq %d %s",
					len(reqs), last.delivery, last.seq, last.body, first.delivery, first.seq, first.body)
			}
			if got := w.row(t, id); got != row {
				t.Errorf("subscription = %+v, want it unchanged %+v", got, row)
			}
			if got := w.configured(t); !reflect.DeepEqual(got, rows) {
				t.Errorf("subscriptions = %v\nwant them unchanged %v", got, rows)
			}
			if got := w.metrics.deliveries(tc.result); got != counted+1 {
				t.Errorf("deliveries reported %s = %v, want %v", tc.result, got, counted+1)
			}

			stored := w.deliveries(t, id)[0]
			w.clock.advance(time.Hour)
			if got := w.deliveries(t, id)[0]; got != stored || got.status == "pending" || got.nextAttemptAt.Valid {
				t.Errorf("an hour later the delivery is %+v, want it ended as %+v", got, stored)
			}
			if n := len(in.requests()); n != 2 {
				t.Errorf("an hour later the receiver got %d requests, want 2", n)
			}
		})
	}
}

func TestRedeliver_RefusesWhatCannotBeSentAgain(t *testing.T) {
	t.Parallel()
	// keyless takes the operator's keys away from the webhook component, so
	// that no signing secret opens.
	var keyless atomic.Bool
	w := newWorld(t, options{toml: insecure, adjust: []func(*webhook.Deps){func(d *webhook.Deps) {
		keys := d.Keys
		d.Keys = func() secret.Keys {
			if keyless.Load() {
				return secret.Keys{}
			}
			return keys()
		}
	}}})
	a := answeringWith(http.StatusOK)
	in := newInbox(t, a.answer)
	id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": []string{"parley.test.*"}})
	otherID := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": []string{"parley.none"}})
	gone := w.emit(t, demo, "parley.test.gone", "")
	waitFor(t, "the event to be delivered", func() bool { return w.settled(t, id) })
	ping := func(sub string) string {
		status, raw := w.Call(t, http.MethodPost, hookPath(demo, sub)+"/ping", w.ownerKey, nil)
		if status != http.StatusOK {
			t.Fatalf("ping = %d %s, want 200", status, raw)
		}
		pid, _ := decode(t, raw)["id"].(string)
		return pid
	}
	pingID, otherDelivery := ping(id), ping(otherID)
	w.clock.advance(time.Hour)
	a.set(http.StatusInternalServerError)
	kept := w.emit(t, demo, "parley.test.kept", "")
	pending := w.retrying(t, id, 1)
	if oldest, err := w.log.Expire(context.Background(), w.clock.Now()); err != nil || oldest != kept {
		t.Fatalf("Expire = %d, %v, want oldest seq %d", oldest, err, kept)
	}
	var goneDelivery string
	for _, d := range w.deliveries(t, id) {
		if d.seq == gone {
			goneDelivery = d.id
		}
	}

	for _, tc := range []struct {
		name, delivery string
		// keyless is true when no key opens the signing secret.
		keyless   bool
		status    int
		errorType string
	}{
		{"not a UUID", "abc", false, http.StatusBadRequest, "invalid_request"},
		{"an unknown id", uuid.NewString(), false, http.StatusNotFound, "delivery_not_found"},
		{"a delivery of another subscription", otherDelivery, false, http.StatusNotFound, "delivery_not_found"},
		{"a pending delivery", pending.id, false, http.StatusConflict, "delivery_pending"},
		{"a ping", pingID, false, http.StatusBadRequest, "invalid_request"},
		{"an event that left the log", goneDelivery, false, http.StatusGone, "event_expired"},
		{"an unknown id without a key", uuid.NewString(), true, http.StatusNotFound, "delivery_not_found"},
		{"a ping without a key", pingID, true, http.StatusBadRequest, "invalid_request"},
		{"a pending delivery without a key", pending.id, true, http.StatusServiceUnavailable, "secrets_unavailable"},
		{"an event that left the log without a key", goneDelivery, true, http.StatusServiceUnavailable, "secrets_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyless.Store(tc.keyless)
			defer keyless.Store(false)
			mine, others, sent := w.deliveries(t, id), w.deliveries(t, otherID), len(in.requests())
			status, raw := w.Call(t, http.MethodPost, redeliverPath(id, tc.delivery), w.ownerKey, nil)
			if status != tc.status {
				t.Fatalf("redeliver = %d %s, want %d", status, raw, tc.status)
			}
			if e := errorOf(t, raw); e["error_type"] != tc.errorType {
				t.Errorf("error_type = %v, want %s", e["error_type"], tc.errorType)
			}
			if n := len(in.requests()); n != sent {
				t.Errorf("the receiver got %d requests, want none", n-sent)
			}
			if got := w.deliveries(t, id); !reflect.DeepEqual(got, mine) {
				t.Errorf("deliveries = %+v\nwant them unchanged %+v", got, mine)
			}
			if got := w.deliveries(t, otherID); !reflect.DeepEqual(got, others) {
				t.Errorf("the other subscription's deliveries = %+v\nwant them unchanged %+v", got, others)
			}
		})
	}
}

func TestPruneDeliveries_DeletesEndedRecordsLastAttemptedOrCreatedBeforeTheTime(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{})
	id := w.subscribe(t, demo, map[string]any{"url": "https://ci.example/x", "types": []string{"parley.none"}})
	w.switchOff(t, id)
	now := w.clock.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	const day = 24 * time.Hour
	var kept []string
	for _, tc := range []struct {
		typ, status   string
		created, last time.Time
		keep          bool
	}{
		{"parley.test.a", "succeeded", ago(7*day + time.Second), ago(7*day + time.Second), false},
		{"parley.test.a", "failed", ago(8 * day), ago(8 * day), false},
		{"parley.webhook.ping", "succeeded", ago(9 * day), ago(9 * day), false},
		{"parley.test.a", "succeeded", ago(7 * day), ago(7 * day), true},
		{"parley.test.a", "failed", ago(time.Hour), ago(time.Hour), true},
		{"parley.test.a", "pending", ago(9 * day), ago(9 * day), true},
		{"parley.test.a", "succeeded", ago(10 * day), ago(day), true},
		// Ended without an attempt: judged by when it was created.
		{"parley.test.a", "failed", ago(8 * day), time.Time{}, false},
		{"parley.test.a", "failed", ago(day), time.Time{}, true},
	} {
		did := uuid.NewString()
		w.seed(t, id, seeded{id: did, typ: tc.typ, status: tc.status, seq: 1, attempts: 1, createdAt: tc.created, lastAttemptAt: tc.last})
		if tc.keep {
			kept = append(kept, did)
		}
	}

	n, err := w.hooks.PruneDeliveries(context.Background(), now.Add(-168*time.Hour))
	if err != nil || n != 4 {
		t.Errorf("PruneDeliveries = %d, %v, want 4, nil", n, err)
	}
	var left []string
	for _, d := range w.deliveries(t, id) {
		left = append(left, d.id)
	}
	slices.Sort(left)
	slices.Sort(kept)
	if !reflect.DeepEqual(left, kept) {
		t.Errorf("records left = %v, want %v", left, kept)
	}
}

func TestSigning_SendsNothingWhileTheSecretCannotBeOpened(t *testing.T) {
	t.Parallel()
	// keysOff is how the webhook component sees the operator's keys: 0 as
	// configured, 1 without a key, 2 with a key that opens nothing stored.
	var keysOff atomic.Int32
	wrong := make(secret.Key, 32)
	_, _ = rand.Read(wrong)
	w := newWorld(t, options{toml: insecure, adjust: []func(*webhook.Deps){func(d *webhook.Deps) {
		keys := d.Keys
		d.Keys = func() secret.Keys {
			switch keysOff.Load() {
			case 1:
				return secret.Keys{}
			case 2:
				return secret.Keys{Current: wrong}
			}
			return keys()
		}
	}}})
	in := newInbox(t, answerOK)
	id := w.subscribe(t, demo, map[string]any{"url": in.url(), "types": []string{"parley.test.*"}})
	e0 := w.emit(t, demo, "parley.test.event", "")
	waitFor(t, "the first event to be delivered", func() bool { return w.settled(t, id) })
	succeeded := w.deliveries(t, id)[0]

	keysOff.Store(1)
	e1 := w.emit(t, demo, "parley.test.event", "")
	waitFor(t, "the loop to wait for the secret", func() bool {
		ds := w.deliveries(t, id)
		return len(ds) == 2 && ds[1].seq == e1 && w.clock.armed(w.clock.Now().Add(time.Minute))
	})
	if ds := w.deliveries(t, id); ds[1].status != "pending" || ds[1].attempts != 0 {
		t.Errorf("the delivery of e1 = %+v, want pending without an attempt", ds[1])
	}
	if got := in.seqs(); !reflect.DeepEqual(got, []int64{e0}) {
		t.Errorf("the receiver got seqs %v, want only %d", got, e0)
	}
	position := w.row(t, id).position

	for _, mode := range []struct {
		name string
		off  int32
	}{{"no key", 1}, {"a key that matches neither", 2}} {
		keysOff.Store(mode.off)
		for _, call := range []struct{ name, path string }{
			{"ping", hookPath(demo, id) + "/ping"},
			{"redeliver", redeliverPath(id, succeeded.id)},
		} {
			t.Run(mode.name+"/"+call.name, func(t *testing.T) {
				before, sent := w.deliveries(t, id), len(in.requests())
				status, raw := w.Call(t, http.MethodPost, call.path, w.ownerKey, nil)
				if status != http.StatusServiceUnavailable || errorOf(t, raw)["error_type"] != "secrets_unavailable" {
					t.Errorf("%s = %d %s, want 503 secrets_unavailable", call.name, status, raw)
				}
				if got := w.deliveries(t, id); !reflect.DeepEqual(got, before) {
					t.Errorf("deliveries = %+v\nwant them unchanged %+v", got, before)
				}
				if n := len(in.requests()); n != sent {
					t.Errorf("the receiver got %d requests, want none", n-sent)
				}
			})
		}
	}
	if got := w.row(t, id).position; got != position {
		t.Errorf("position = %d, want it left at %d", got, position)
	}

	keysOff.Store(0)
	w.clock.advance(time.Minute)
	waitFor(t, "e1 to be delivered once the key is back", func() bool { return w.settled(t, id) })
	if got := in.seqs(); !reflect.DeepEqual(got, []int64{e0, e1}) {
		t.Errorf("the receiver got seqs %v, want %d then %d once", got, e0, e1)
	}
}
