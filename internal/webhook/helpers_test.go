package webhook_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/metrics"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/webhook"
)

// TestMain runs the package's tests at most hubtest.HubParallel at a time:
// each starts a hub, and hubs contend for one lock past that (see
// hubtest.HubParallel).
func TestMain(m *testing.M) {
	hubtest.LimitParallel(hubtest.HubParallel)
	os.Exit(m.Run())
}

// The workspaces of the seed, both repositories of the owner.
const (
	demo  = "demo"
	other = "other"
)

// clock is a settable clock for the hub and the webhook component, starting
// on a whole second. Its timers fire only when advance reaches them, so that
// no test waits for real time to pass.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*clockTimer]bool
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), timers: map[*clockTimer]bool{}}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer returns a timer that fires once advance has brought the clock d
// from now, at once when d is not positive.
func (c *clock) NewTimer(d time.Duration) event.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &clockTimer{clock: c, at: c.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.c <- c.now
	} else {
		c.timers[t] = true
	}
	return t
}

// advance moves the clock d forward and fires the timers then due.
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for t := range c.timers {
		if !t.at.After(c.now) {
			delete(c.timers, t)
			t.c <- c.now
		}
	}
}

type clockTimer struct {
	clock *clock
	at    time.Time
	c     chan time.Time
}

func (t *clockTimer) C() <-chan time.Time { return t.c }

func (t *clockTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	pending := t.clock.timers[t]
	delete(t.clock.timers, t)
	return pending
}

// stamp is the clock's time as the hub writes a timestamp.
func (c *clock) stamp() string { return c.Now().UTC().Truncate(time.Second).Format(time.RFC3339) }

// world is a hub with a secret key configured, on a settable clock, and the
// seed: the user owner, with the API key ownerKey, owns the repositories
// demo and other in the org acme. log is the hub's event component, which
// the tests write events of other packages with.
// The webhook component runs on the world's clock, reports to its metrics
// and connects through a dialer that reaches loopback receivers alone.
type world struct {
	*hubtest.Hub
	clock    *clock
	log      *event.Component
	metrics  *recorder
	owner    string
	ownerKey string
	org      string
}

// options are what a world is started with besides its seed.
type options struct {
	// toml is added to config.toml, such as a [webhooks] section.
	toml string
	// adjust change the webhook component's dependencies before they are
	// bound.
	adjust []func(*webhook.Deps)
}

// newWorld starts a world's hub with opts.
func newWorld(t *testing.T, opts options) *world {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	w := &world{clock: newClock(), metrics: &recorder{Recorder: metrics.Discard}}
	w.Hub = hubtest.StartWith(t, hubtest.Config{
		TOML: opts.toml,
		Now:  w.clock.Now,
		Env:  map[string]string{secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(key)},
		Compose: func(c *hub.Composition) {
			w.log = c.Events()
			c.AdjustWebhooks(func(d *webhook.Deps) {
				d.Resolver, d.Clock, d.Recorder, d.Dialer = names, w.clock, w.metrics, &loopbackDialer{}
			})
			for _, a := range opts.adjust {
				c.AdjustWebhooks(a)
			}
		},
	})
	w.owner = w.CreateUser(t, "owner")
	w.ownerKey = w.IssueKey(t, w.owner)
	w.org = w.CreateOrg(t, "acme")
	for _, slug := range []string{demo, other} {
		w.AddWorkspace(t, access.Record{Slug: slug, OwnerID: w.owner, OrgID: w.org, Kind: access.KindRepository, Status: access.StatusReady})
	}
	return w
}

// resolver answers host names as a DNS server would, from a fixed list, so
// that no test asks the network; a name it does not list does not resolve.
type resolver map[string][]netip.Addr

func (r resolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := r[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// names are the host names every world's webhook component resolves:
// ci.example is outside the hub's networks, inside.example inside them, and
// mixed.example has one address of each.
var names = resolver{
	"ci.example":     {netip.MustParseAddr("203.0.113.10")},
	"inside.example": {netip.MustParseAddr("10.0.0.5")},
	"mixed.example":  {netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("10.0.0.5")},
}

// loopbackDialer dials the loopback addresses the tests' receivers listen
// on, and holds a dial to any other address until the attempt ends, as a
// network that drops the packets does, so that no test reaches the network
// and an attempt to a name of the resolver above stays in flight.
type loopbackDialer struct{ net.Dialer }

func (d *loopbackDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if addr, err := netip.ParseAddr(host); err == nil && addr.IsLoopback() {
		return d.Dialer.DialContext(ctx, network, address)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// recorder records what the webhook component reports to the metrics: the
// sum of the Adds of each metric and result.
type recorder struct {
	metrics.Recorder
	mu   sync.Mutex
	adds map[string]float64
}

func (r *recorder) Add(name string, labels map[string]string, value float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.adds == nil {
		r.adds = map[string]float64{}
	}
	r.adds[name+"/"+labels["result"]] += value
	return nil
}

// deliveries is the number of delivery attempts reported with result.
func (r *recorder) deliveries(result string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.adds[metrics.WebhookDeliveriesTotal+"/"+result]
}

// hooksPath is the path of the webhook collection of the workspace slug.
func hooksPath(slug string) string { return "/api/v1/workspaces/" + slug + "/webhooks" }

// hookPath is the path of the subscription id of the workspace slug.
func hookPath(slug, id string) string { return hooksPath(slug) + "/" + id }

// create sends a create of body on the workspace slug as credential.
func (w *world) create(t *testing.T, credential, slug string, body any) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodPost, hooksPath(slug), credential, body)
}

// mustCreate creates a subscription of url on slug as the owner, fails the
// test unless it is answered 201, and returns the answer.
func (w *world) mustCreate(t *testing.T, slug, url string) map[string]any {
	t.Helper()
	status, raw := w.create(t, w.ownerKey, slug, map[string]any{"url": url})
	if status != http.StatusCreated {
		t.Fatalf("create on %s = %d %s, want 201", slug, status, raw)
	}
	return decode(t, raw)
}

// count is the number of subscriptions of the workspace slug, "" for all.
func (w *world) count(t *testing.T, slug string) int {
	t.Helper()
	var n int
	err := w.Store.Main().QueryRow(`SELECT count(*) FROM webhook_subscriptions WHERE ? = '' OR workspace = ?`, slug, slug).Scan(&n)
	if err != nil {
		t.Fatalf("count the subscriptions: %v", err)
	}
	return n
}

// hubEvent is a hub event as the tests read it from the event log.
type hubEvent struct {
	Seq          int64
	Type         string
	Workspace    sql.NullString
	ActorType    string
	ActorID      sql.NullString
	ResourceType string
	ResourceID   string
	Metadata     map[string]any
}

// events are the hub events of the type typ in the log, oldest first.
func (w *world) events(t *testing.T, typ string) []hubEvent {
	t.Helper()
	rows, err := w.Store.Main().QueryContext(context.Background(),
		`SELECT seq, type, workspace, actor_type, actor_id, resource_type, resource_id, metadata
		 FROM hub_events WHERE type = ? ORDER BY seq`, typ)
	if err != nil {
		t.Fatalf("read the event log: %v", err)
	}
	defer rows.Close()
	var out []hubEvent
	for rows.Next() {
		var e hubEvent
		var meta string
		if err := rows.Scan(&e.Seq, &e.Type, &e.Workspace, &e.ActorType, &e.ActorID, &e.ResourceType, &e.ResourceID, &meta); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(meta), &e.Metadata); err != nil {
			t.Fatalf("metadata %s: %v", meta, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// lastSeq is the seq of the newest event of the log.
func (w *world) lastSeq(t *testing.T) int64 {
	t.Helper()
	var seq int64
	if err := w.Store.Main().QueryRow(`SELECT coalesce(max(seq), 0) FROM hub_events`).Scan(&seq); err != nil {
		t.Fatalf("read the newest seq: %v", err)
	}
	return seq
}

// emit writes an event of the type typ on the workspace slug, as another
// package of the hub does, and returns its seq; parent is the slug of the
// fork's parent when slug is a task fork, else "".
func (w *world) emit(t *testing.T, slug, typ, parent string) int64 {
	t.Helper()
	return w.write(t, event.Input{
		Type: typ, Workspace: &slug, ActorType: event.ActorSystem,
		ResourceType: "test", ResourceID: "r1", Parent: parent,
	})
}

// write writes the event in to the log and returns its seq.
func (w *world) write(t *testing.T, in event.Input) int64 {
	t.Helper()
	var seq int64
	err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		rec, err := w.log.Emit(context.Background(), tx, in)
		seq = rec.Seq
		return err
	})
	if err != nil {
		t.Fatalf("emit %s: %v", in.Type, err)
	}
	return seq
}

// archive sets the status of the workspace slug to archived.
func (w *world) archive(t *testing.T, slug string) {
	t.Helper()
	if _, err := w.Store.Main().Exec(`UPDATE workspaces SET status = 'archived' WHERE slug = ?`, slug); err != nil {
		t.Fatalf("archive %s: %v", slug, err)
	}
}

// configured is every subscription's row as the routes write it, every
// column as text, ordered by id, so that two snapshots compare equal only
// when no route changed a subscription. The columns the delivery loop moves
// as it goes, the position and the outcome of the last delivery, are left
// out.
func (w *world) configured(t *testing.T) []string {
	t.Helper()
	rows, err := w.Store.Main().Query(`SELECT id, workspace, url, types, include_forks, description, state,
		created_by, created_at, updated_at, secret_sealed FROM webhook_subscriptions ORDER BY id`)
	if err != nil {
		t.Fatalf("read the subscriptions: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals...))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// decode decodes a JSON object.
func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

// errorOf is the error member of an error envelope.
func errorOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	e, ok := decode(t, raw)["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %s", raw)
	}
	return e
}

// insecure lets a subscription deliver to the tests' http receivers on
// 127.0.0.1.
const insecure = "[webhooks]\nallow_insecure = true\n"

// delivered is a request a receiver got: the event's seq and type and the
// delivery's id from its headers, and its body.
type delivered struct {
	seq      int64
	typ      string
	delivery string
	body     []byte
}

// inbox is a webhook receiver on 127.0.0.1 that records every request and
// answers it with the status answer returns. stop is closed when the test
// ends, before the server closes, so that an answer that waits lets the test
// end.
type inbox struct {
	*httptest.Server
	mu  sync.Mutex
	got []delivered
}

func newInbox(t *testing.T, answer func(d delivered, stop <-chan struct{}) int) *inbox {
	t.Helper()
	in := &inbox{}
	stop := make(chan struct{})
	in.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seq, _ := strconv.ParseInt(r.Header.Get("Parley-Seq"), 10, 64)
		d := delivered{seq: seq, typ: r.Header.Get("Parley-Event"), delivery: r.Header.Get("Parley-Delivery"), body: body}
		in.mu.Lock()
		in.got = append(in.got, d)
		in.mu.Unlock()
		w.WriteHeader(answer(d, stop))
	}))
	t.Cleanup(in.Close)
	t.Cleanup(func() { close(stop) })
	return in
}

// answerOK answers every request 200.
func answerOK(delivered, <-chan struct{}) int { return http.StatusOK }

// url is the receiver's webhook URL.
func (in *inbox) url() string { return in.URL + "/hook" }

// requests are the requests the receiver got, in arrival order.
func (in *inbox) requests() []delivered {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]delivered(nil), in.got...)
}

// seqs are the seqs of the requests the receiver got, in arrival order.
func (in *inbox) seqs() []int64 {
	var out []int64
	for _, d := range in.requests() {
		out = append(out, d.seq)
	}
	return out
}

// waitFor polls cond until it holds, and fails the test when it does not
// within 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// subscribe creates a subscription of body on slug as the owner, fails the
// test unless it is answered 201, and returns its id.
func (w *world) subscribe(t *testing.T, slug string, body map[string]any) string {
	t.Helper()
	status, raw := w.create(t, w.ownerKey, slug, body)
	if status != http.StatusCreated {
		t.Fatalf("create on %s = %d %s, want 201", slug, status, raw)
	}
	id, _ := decode(t, raw)["id"].(string)
	return id
}

// subscriptionRow is the stored state of a subscription the delivery loop
// changes.
type subscriptionRow struct {
	position       int64
	state          string
	lastStatus     sql.NullString
	lastDeliveryAt sql.NullInt64
	failingSince   sql.NullInt64
}

// row reads the stored state of the subscription id.
func (w *world) row(t *testing.T, id string) subscriptionRow {
	t.Helper()
	var r subscriptionRow
	err := w.Store.Main().QueryRow(`SELECT position, state, last_status, last_delivery_at, failing_since
		FROM webhook_subscriptions WHERE id = ?`, id).Scan(&r.position, &r.state, &r.lastStatus, &r.lastDeliveryAt, &r.failingSince)
	if err != nil {
		t.Fatalf("read event webhook %s: %v", id, err)
	}
	return r
}

// deliveryRow is a stored delivery record.
type deliveryRow struct {
	id            string
	seq           int64
	typ           string
	status        string
	attempts      int
	responseCode  sql.NullInt64
	err           sql.NullString
	lastAttemptAt sql.NullInt64
	nextAttemptAt sql.NullInt64
}

// deliveries reads the delivery records of the subscription id, by seq.
func (w *world) deliveries(t *testing.T, id string) []deliveryRow {
	t.Helper()
	rows, err := w.Store.Main().Query(`SELECT id, seq, type, status, attempts, response_code, error, last_attempt_at, next_attempt_at
		FROM webhook_deliveries WHERE subscription_id = ? ORDER BY seq`, id)
	if err != nil {
		t.Fatalf("read the deliveries of %s: %v", id, err)
	}
	defer rows.Close()
	var out []deliveryRow
	for rows.Next() {
		var d deliveryRow
		if err := rows.Scan(&d.id, &d.seq, &d.typ, &d.status, &d.attempts, &d.responseCode, &d.err, &d.lastAttemptAt, &d.nextAttemptAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// settled reports whether the subscription id has examined the log up to
// its newest event and has no pending delivery.
func (w *world) settled(t *testing.T, id string) bool {
	t.Helper()
	if w.row(t, id).position != w.lastSeq(t) {
		return false
	}
	for _, d := range w.deliveries(t, id) {
		if d.status == "pending" {
			return false
		}
	}
	return true
}
