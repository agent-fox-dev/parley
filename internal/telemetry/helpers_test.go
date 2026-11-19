package telemetry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// TestMain runs the package's tests at most hubtest.HubParallel at a time:
// each starts a hub, and hubs contend for one lock past that (see
// hubtest.HubParallel).
func TestMain(m *testing.M) {
	hubtest.LimitParallel(hubtest.HubParallel)
	os.Exit(m.Run())
}

// clock is a settable clock for the hub, starting on a whole second,
// 2026-01-02T03:04:05Z.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// The workspaces of the seed. alice owns them all, in the org acme: demo,
// ready; old, archived; one repository in each of the other statuses; and
// the task fork demo--task1 of demo. nowhere names no workspace.
const (
	demo      = "demo"
	old       = "old"
	pending   = "ws-pending"
	cloning   = "ws-cloning"
	failed    = "ws-failed"
	demoFork  = "demo--task1"
	nowhere   = "no-such-workspace"
	someRunID = "20260102_030405_a1b2c3"
)

// ingestionRoute is one route of the ingestion gate: its path after
// /workspaces/:slug/runs/:run_id, whether it takes a batch, and a valid body
// for it, new on every call.
type ingestionRoute struct {
	path  string
	batch bool
	valid func() any
}

// stored is the status a valid request to the route is answered with.
func (r ingestionRoute) stored() int {
	if r.batch {
		return http.StatusOK
	}
	return http.StatusCreated
}

// ingestionRoutes are every route registered through the ingestion gate,
// each with a valid body; the gate's tables run over all of them.
var ingestionRoutes = []ingestionRoute{
	{path: "/events", valid: func() any { return map[string]any{"event_type": "run.start"} }},
}

// recordTables are the analytical store's tables of telemetry records.
var recordTables = []string{"run_events"}

// world is a hub on a settable clock whose analytical store fails every
// write while failWrites is set, with the seed: alice owns every workspace
// of the seed in the org acme, of which bob is a member; carol has nothing.
// Each user has an API key (the field of the user's name); alicePAT is a
// PAT of alice's holding audit:write.
type world struct {
	*hubtest.Hub
	clock      *clock
	failWrites atomic.Bool

	alice, bob, carol          string
	aliceKey, bobKey, carolKey string
	alicePAT                   string

	runs atomic.Int64
}

// newWorld starts the world's hub; each adjust changes the telemetry
// component's dependencies before they are bound.
func newWorld(t *testing.T, adjust ...func(*telemetry.Deps)) *world {
	t.Helper()
	w := &world{clock: newClock()}
	w.Hub = hubtest.StartWith(t, hubtest.Config{Now: w.clock.Now, Compose: func(c *hub.Composition) {
		c.AdjustTelemetry(func(d *telemetry.Deps) { d.DB = faultDB{DB: d.DB, fail: &w.failWrites} })
		for _, a := range adjust {
			c.AdjustTelemetry(a)
		}
	}})
	w.alice, w.bob, w.carol = w.CreateUser(t, "alice"), w.CreateUser(t, "bob"), w.CreateUser(t, "carol")
	w.aliceKey, w.bobKey, w.carolKey = w.IssueKey(t, w.alice), w.IssueKey(t, w.bob), w.IssueKey(t, w.carol)
	w.alicePAT = w.pat(t, w.alice, "audit:write")
	acme := w.CreateOrg(t, "acme")
	w.AddMember(t, acme, w.alice)
	w.AddMember(t, acme, w.bob)
	repos := map[string]string{
		demo: access.StatusReady, old: access.StatusArchived, pending: access.StatusPending,
		cloning: access.StatusCloning, failed: access.StatusFailed,
	}
	for slug, status := range repos {
		w.AddWorkspace(t, access.Record{Slug: slug, OwnerID: w.alice, OrgID: acme, Kind: access.KindRepository, Status: status})
	}
	w.AddWorkspace(t, access.Record{Slug: demoFork, OwnerID: w.alice, OrgID: acme, Kind: access.KindFork, ParentSlug: demo, Status: access.StatusReady})
	return w
}

// pat issues a PAT of the user userID with permissions.
func (w *world) pat(t *testing.T, userID string, permissions ...string) string {
	t.Helper()
	return w.IssuePAT(t, identity.PATSpec{UserID: userID, Permissions: permissions})
}

// freshRun is a valid run id that no other call returns, so that a test's
// records are its own.
func (w *world) freshRun() string {
	return fmt.Sprintf("20260102_030405_%06x", w.runs.Add(1))
}

// post sends body to the ingestion route path of the workspace slug and the
// run runID as credential.
func (w *world) post(t *testing.T, credential, slug, runID, path string, body any) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/runs/"+runID+path, credential, body)
}

// countAllRecords is the number of telemetry records of every kind.
func (w *world) countAllRecords(t *testing.T) int {
	t.Helper()
	n := 0
	for _, table := range recordTables {
		n += w.count(t, `SELECT COUNT(*) FROM `+table)
	}
	return n
}

// count answers query, which counts rows of the analytical store.
func (w *world) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := w.Store.Analytics().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// decode decodes a JSON object answer.
func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

// errorOf is the error_type and message of an error envelope.
func errorOf(t *testing.T, raw []byte) (errorType, message string) {
	t.Helper()
	var env struct {
		Error struct {
			ErrorType string `json:"error_type"`
			Message   string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode error envelope %s: %v", raw, err)
	}
	return env.Error.ErrorType, env.Error.Message
}

// errInjected is the failure faultDB injects.
var errInjected = errors.New("injected store failure")

// faultDB is the analytical store with a fault a test switches on: while
// fail is set, every statement of a write transaction fails.
type faultDB struct {
	telemetry.DB
	fail *atomic.Bool
}

func (f faultDB) BeginTx(ctx context.Context) (telemetry.Tx, error) {
	tx, err := f.DB.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	return faultTx{Tx: tx, fail: f.fail}, nil
}

// faultTx is a transaction of faultDB.
type faultTx struct {
	telemetry.Tx
	fail *atomic.Bool
}

func (f faultTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if f.fail.Load() {
		return nil, errInjected
	}
	return f.Tx.ExecContext(ctx, query, args...)
}
