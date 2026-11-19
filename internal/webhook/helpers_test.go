package webhook_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
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

// clock is a settable clock for the hub, starting on a whole second.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// advance moves the clock d forward.
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// stamp is the clock's time as the hub writes a timestamp.
func (c *clock) stamp() string { return c.Now().UTC().Truncate(time.Second).Format(time.RFC3339) }

// world is a hub with a secret key configured, on a settable clock, and the
// seed: the user owner, with the API key ownerKey, owns the repositories
// demo and other in the org acme. log is the hub's event component, which
// the tests write events of other packages with.
type world struct {
	*hubtest.Hub
	clock    *clock
	log      *event.Component
	owner    string
	ownerKey string
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
	w := &world{clock: newClock()}
	w.Hub = hubtest.StartWith(t, hubtest.Config{
		TOML: opts.toml,
		Now:  w.clock.Now,
		Env:  map[string]string{secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(key)},
		Compose: func(c *hub.Composition) {
			w.log = c.Events()
			c.AdjustWebhooks(func(d *webhook.Deps) { d.Resolver = names })
			for _, a := range opts.adjust {
				c.AdjustWebhooks(a)
			}
		},
	})
	w.owner = w.CreateUser(t, "owner")
	w.ownerKey = w.IssueKey(t, w.owner)
	org := w.CreateOrg(t, "acme")
	for _, slug := range []string{demo, other} {
		w.AddWorkspace(t, access.Record{Slug: slug, OwnerID: w.owner, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady})
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
	var seq int64
	err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		rec, err := w.log.Emit(context.Background(), tx, event.Input{
			Type: typ, Workspace: &slug, ActorType: event.ActorSystem,
			ResourceType: "test", ResourceID: "r1", Parent: parent,
		})
		seq = rec.Seq
		return err
	})
	if err != nil {
		t.Fatalf("emit %s on %s: %v", typ, slug, err)
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

// table is every row of the table name, every column as text, ordered by
// id, so that two snapshots compare equal only when nothing changed.
func (w *world) table(t *testing.T, name string) []string {
	t.Helper()
	rows, err := w.Store.Main().Query(`SELECT * FROM ` + name + ` ORDER BY id`)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
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
