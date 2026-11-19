package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/webhook"
)

// webhookClock is the controlled clock of an event webhook flow, the hub's
// and the webhook component's: it moves only when the test advances it, and
// a timer fires when the clock reaches it, at once when it is due already.
type webhookClock struct{ *hubtest.JobClock }

func (c webhookClock) NewTimer(d time.Duration) event.Timer {
	if d <= 0 {
		t := make(firedTimer, 1)
		t <- c.Now()
		return t
	}
	return c.JobClock.NewTimer(d)
}

// advanceTo moves the clock on to at.
func (c webhookClock) advanceTo(at time.Time) { c.Advance(at.Sub(c.Now())) }

// firedTimer is a timer that has fired already.
type firedTimer chan time.Time

func (t firedTimer) C() <-chan time.Time { return t }
func (t firedTimer) Stop() bool          { return false }

// received is one request a receiver got, and the status it answered.
type received struct {
	header http.Header
	body   []byte
	status int
}

// seq is the request's Parley-Seq.
func (r received) seq() string { return r.header.Get("Parley-Seq") }

// receiver is a webhook receiver on a loopback port: it records every
// request and answers the status the test sets.
type receiver struct {
	*httptest.Server
	status atomic.Int64
	mu     sync.Mutex
	got    []received
}

func newReceiver(t *testing.T, status int) *receiver {
	t.Helper()
	r := &receiver{}
	r.status.Store(int64(status))
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		status := int(r.status.Load())
		r.mu.Lock()
		r.got = append(r.got, received{req.Header.Clone(), body, status})
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// accepted are the requests the receiver answered 200.
func (r *receiver) accepted() []received {
	var out []received
	for _, req := range r.requests() {
		if req.status == http.StatusOK {
			out = append(out, req)
		}
	}
	return out
}

// verifies tells whether req carries the signature that secret makes over
// its timestamp and body.
func verifies(req received, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(req.header.Get("Parley-Timestamp") + "."))
	mac.Write(req.body)
	return hmac.Equal([]byte(req.header.Get("Parley-Signature")), []byte("sha256="+hex.EncodeToString(mac.Sum(nil))))
}

// webhookFlow is a hub with a secret key, allow_insecure and a controlled
// clock, the developer dev with the API key key, and the repository demo
// dev owns.
type webhookFlow struct {
	*identityHub
	clock webhookClock
	key   string
}

func startWebhookFlow(t *testing.T) *webhookFlow {
	t.Helper()
	start := time.Now().UTC().Truncate(time.Second)
	clock := webhookClock{hubtest.NewJobClock(func() time.Time { return start })}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	h := hubtest.StartWith(t, hubtest.Config{
		TOML: "[webhooks]\nallow_insecure = true\n",
		Env:  map[string]string{secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(key)},
		Now:  clock.Now,
		Compose: func(c *hub.Composition) {
			c.AdjustWebhooks(func(d *webhook.Deps) { d.Clock = clock })
		},
	})
	dev := h.CreateUser(t, "dev")
	h.AddWorkspace(t, access.Record{Slug: "demo", OwnerID: dev, OrgID: h.CreateOrg(t, "acme"), Kind: access.KindRepository, Status: access.StatusReady})
	return &webhookFlow{identityHub: &identityHub{h}, clock: clock, key: h.IssueKey(t, dev)}
}

// webhook runs parley webhook with args as dev and fails the test unless it
// exits 0.
func (f *webhookFlow) webhook(t *testing.T, args ...string) ran {
	t.Helper()
	return f.must(t, f.key, append([]string{"webhook"}, args...)...)
}

// delivery is a delivery record as parley webhook deliveries prints it.
type delivery struct {
	ID            string  `json:"id"`
	Seq           int64   `json:"seq"`
	Type          string  `json:"type"`
	Status        string  `json:"status"`
	Attempts      int     `json:"attempts"`
	NextAttemptAt *string `json:"next_attempt_at"`
}

// deliveries are the delivery records of the subscription id, newest first.
func (f *webhookFlow) deliveries(t *testing.T, id string, args ...string) []delivery {
	t.Helper()
	r := f.webhook(t, append([]string{"deliveries", "demo", id, "--limit", "200"}, args...)...)
	var page struct {
		Items []delivery `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &page); err != nil {
		t.Fatalf("deliveries printed %q: %v", r.stdout, err)
	}
	return page.Items
}

// nextAttempt waits until the subscription id has a pending delivery whose
// next attempt is scheduled after the clock's time, and returns that time,
// or false once it has nothing left to send.
func (f *webhookFlow) nextAttempt(t *testing.T, id string) (time.Time, bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		now := f.clock.Now()
		pending := false
		for _, d := range f.deliveries(t, id, "--status", "pending") {
			pending = true
			if d.NextAttemptAt == nil {
				continue
			}
			next, err := time.Parse(time.RFC3339, *d.NextAttemptAt)
			if err != nil {
				t.Fatal(err)
			}
			if next.After(now) {
				return next, true
			}
		}
		if !pending && f.webhook(t, "get", "demo", id).json(t)["pending"] == float64(0) {
			return time.Time{}, false
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waited 10 s for the next attempt of %s to be scheduled", id)
	return time.Time{}, false
}

// retryWhile moves the clock from one scheduled attempt of the subscription
// id to the next, each once the attempt before it is recorded, while more
// holds for the time the next one is due.
func (f *webhookFlow) retryWhile(t *testing.T, id string, more func(next time.Time) bool) {
	t.Helper()
	for range 500 {
		next, scheduled := f.nextAttempt(t, id)
		if !scheduled || !more(next) {
			return
		}
		f.clock.advanceTo(next)
	}
	t.Fatalf("moved the clock through 500 attempts of %s", id)
}

// hookEvent is an event of the event log, with its record as served.
type hookEvent struct {
	Seq        int64  `json:"seq"`
	Type       string `json:"type"`
	ActorType  string `json:"actor_type"`
	ResourceID string `json:"resource_id"`
	raw        json.RawMessage
}

// eventLog is the whole event log as the admin token reads it.
func (f *webhookFlow) eventLog(t *testing.T) []hookEvent {
	t.Helper()
	status, body := f.Call(t, http.MethodGet, "/api/v1/events/log?after=0&limit=200", f.Token, nil)
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET /api/v1/events/log = %d %s, want 200 with a page", status, body)
	}
	events := make([]hookEvent, 0, len(page.Items))
	for _, raw := range page.Items {
		e := hookEvent{raw: raw}
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	return events
}

// ofType are the events of typ about the subscription id.
func ofType(events []hookEvent, typ, id string) []hookEvent {
	var out []hookEvent
	for _, e := range events {
		if e.Type == typ && e.ResourceID == id {
			out = append(out, e)
		}
	}
	return out
}

// waitDelivered waits until the subscription id has recorded the delivery
// of the event seq as succeeded.
func (f *webhookFlow) waitDelivered(t *testing.T, id, seq string) {
	t.Helper()
	eventually(t, 10*time.Second, "the delivery of seq "+seq+" to be recorded", func() bool {
		for _, d := range f.deliveries(t, id, "--status", "succeeded") {
			if strconv.FormatInt(d.Seq, 10) == seq {
				return true
			}
		}
		return false
	})
}

// waitForRequests waits until the receiver holds n requests.
func waitForRequests(t *testing.T, r *receiver, n int) {
	t.Helper()
	eventually(t, 10*time.Second, strconv.Itoa(n)+" requests at the receiver", func() bool { return len(r.requests()) >= n })
}

func TestWebhook_ADeveloperSubscribesAReceiverGetsASignedEventAndInspectsTheDeliveries(t *testing.T) {
	parallel(t)
	f := startWebhookFlow(t)
	rcv := newReceiver(t, http.StatusOK)

	created := f.webhook(t, "create", "demo", "--url", rcv.URL+"/hook")
	sub := created.json(t)
	secretValue, _ := sub["secret"].(string)
	id, _ := sub["id"].(string)
	if len(secretValue) != len("whsec_")+64 || id == "" {
		t.Fatalf("create printed %s, want a subscription with its secret", created.stdout)
	}
	if created.stderr == "" || strings.Contains(created.stderr, secretValue) {
		t.Errorf("create wrote %q on stderr, want the show-once warning without the secret", created.stderr)
	}
	f.webhook(t, "update", "demo", id, "--description", "CI trigger")
	waitForRequests(t, rcv, 1)
	f.waitDelivered(t, id, rcv.requests()[0].seq())
	// A second later, as time passes, so that the ping is the newer record.
	f.clock.Advance(time.Second)
	ping := f.webhook(t, "ping", "demo", id)

	if status := ping.json(t)["status"]; status != "succeeded" {
		t.Errorf("ping printed %s, want status succeeded", ping.stdout)
	}
	got := rcv.requests()
	if len(got) != 2 {
		t.Fatalf("the receiver holds %d requests, want 2", len(got))
	}
	update, pinged := got[0], got[1]
	if update.header.Get("Parley-Event") != "parley.webhook.update" || pinged.header.Get("Parley-Event") != "parley.webhook.ping" || pinged.seq() != "0" {
		t.Errorf("the receiver got %s (seq %s) then %s (seq %s), want the update event, then the ping with seq 0",
			update.header.Get("Parley-Event"), update.seq(), pinged.header.Get("Parley-Event"), pinged.seq())
	}
	for _, req := range got {
		if !verifies(req, secretValue) {
			t.Errorf("the request %s carries the signature %q, which the secret create printed does not make", req.header.Get("Parley-Event"), req.header.Get("Parley-Signature"))
		}
	}
	log := f.eventLog(t)
	var logged json.RawMessage
	for _, e := range log {
		if strconv.FormatInt(e.Seq, 10) == update.seq() {
			logged = e.raw
		}
		if e.Type == "parley.webhook.ping" {
			t.Errorf("the event log holds the ping %s", e.raw)
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, logged); err != nil || !bytes.Equal(compact.Bytes(), update.body) {
		t.Errorf("the update request's body is %s, want the event log's record of seq %s: %s", update.body, update.seq(), logged)
	}
	ds := f.deliveries(t, id)
	if len(ds) != 2 || ds[0].Type != "parley.webhook.ping" || ds[1].Type != "parley.webhook.update" {
		t.Fatalf("deliveries = %+v, want the ping, then the update event", ds)
	}
	for _, d := range ds {
		if d.Status != "succeeded" || d.Attempts != 1 {
			t.Errorf("delivery %+v, want succeeded after 1 attempt", d)
		}
	}
	if s := f.webhook(t, "get", "demo", id).json(t); s["state"] != "active" || s["last_status"] != "succeeded" || s["pending"] != float64(0) {
		t.Errorf("get printed %v, want state active, last_status succeeded and pending 0", s)
	}
}

func TestWebhook_AReceiverStaysDownTheSubscriptionIsDisabledAndTheDeveloperBringsItBack(t *testing.T) {
	parallel(t)
	f := startWebhookFlow(t)
	rcv := newReceiver(t, http.StatusInternalServerError)
	id, _ := f.webhook(t, "create", "demo", "--url", rcv.URL+"/hook").json(t)["id"].(string)
	f.webhook(t, "update", "demo", id, "--description", "e1")
	f.webhook(t, "update", "demo", id, "--description", "e2")
	waitForRequests(t, rcv, 1)
	first := rcv.requests()[0]
	e1 := first.seq()
	since := f.clock.Now()

	// A day of retries, until e1 is given up.
	f.retryWhile(t, id, func(time.Time) bool {
		for _, d := range f.deliveries(t, id) {
			if strconv.FormatInt(d.Seq, 10) == e1 {
				return d.Status == "pending"
			}
		}
		return true
	})
	if at := f.clock.Now(); !at.Equal(since.Add(24 * time.Hour)) {
		t.Errorf("e1 was given up at %s, want a day after its first attempt, %s", at, since.Add(24*time.Hour))
	}
	var e1Delivery delivery
	for _, d := range f.deliveries(t, id) {
		if strconv.FormatInt(d.Seq, 10) == e1 {
			e1Delivery = d
		}
	}
	if e1Delivery.Status != "failed" || e1Delivery.ID != first.header.Get("Parley-Delivery") {
		t.Errorf("e1's delivery is %+v, want failed under the id of its first attempt %s", e1Delivery, first.header.Get("Parley-Delivery"))
	}
	if s := f.webhook(t, "get", "demo", id).json(t); s["state"] != "failing" {
		t.Errorf("after a day get printed %v, want state failing", s)
	}

	// Three days after the first failure, and the minute of the check.
	end := since.Add(72 * time.Hour)
	f.retryWhile(t, id, func(next time.Time) bool { return next.Before(end.Add(-time.Second)) })
	f.clock.advanceTo(end.Add(-time.Second))
	time.Sleep(20 * time.Millisecond)
	check := end.Add(61 * time.Second)
	f.retryWhile(t, id, func(next time.Time) bool { return next.Before(check) })
	f.clock.advanceTo(check)
	eventually(t, 10*time.Second, "the subscription to be disabled", func() bool {
		return f.webhook(t, "get", "demo", id).json(t)["state"] == "disabled"
	})
	log := f.eventLog(t)
	failing, disabled := ofType(log, "parley.webhook.failing", id), ofType(log, "parley.webhook.disabled", id)
	if len(failing) != 1 || len(disabled) != 1 || failing[0].ActorType != "system" || disabled[0].ActorType != "system" {
		t.Errorf("the log holds the failing events %+v and the disabled events %+v, want one of each by the system", failing, disabled)
	}

	rcv.status.Store(http.StatusOK)
	f.webhook(t, "update", "demo", id, "--enable")
	f.webhook(t, "update", "demo", id, "--description", "e9")
	updates := ofType(f.eventLog(t), "parley.webhook.update", id)
	e9 := strconv.FormatInt(updates[len(updates)-1].Seq, 10)
	eventually(t, 10*time.Second, "a request the receiver accepts", func() bool { return len(rcv.accepted()) > 0 })
	if got := rcv.accepted()[0].seq(); got != e9 {
		t.Fatalf("the first request the receiver accepted carries seq %s, want e9's %s", got, e9)
	}
	f.waitDelivered(t, id, e9)
	var did string
	for _, d := range f.deliveries(t, id, "--status", "failed") {
		if strconv.FormatInt(d.Seq, 10) == e1 {
			did = d.ID
		}
	}
	redelivered := f.webhook(t, "redeliver", "demo", id, did)

	if status := redelivered.json(t)["status"]; status != "succeeded" {
		t.Errorf("redeliver printed %s, want status succeeded", redelivered.stdout)
	}
	accepted := rcv.accepted()
	var seqs []string
	for _, req := range accepted {
		seqs = append(seqs, req.seq())
	}
	if !slices.Equal(seqs, []string{e9, e1}) {
		t.Errorf("the receiver accepted the seqs %v, want e9 %s, then the redelivery of e1 %s", seqs, e9, e1)
	}
	if last := accepted[len(accepted)-1]; last.header.Get("Parley-Delivery") != first.header.Get("Parley-Delivery") || did != first.header.Get("Parley-Delivery") {
		t.Errorf("the redelivery came as %s from the record %s, want the Parley-Delivery of e1's first attempt %s",
			last.header.Get("Parley-Delivery"), did, first.header.Get("Parley-Delivery"))
	}
	if s := f.webhook(t, "get", "demo", id).json(t); s["state"] != "active" || s["failing_since"] != nil || s["last_status"] != "succeeded" {
		t.Errorf("get printed %v, want state active, failing_since null and last_status succeeded", s)
	}
}
