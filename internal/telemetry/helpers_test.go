package telemetry_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
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

// advance moves the clock d forward.
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// The workspaces of the seed. alice owns them all, in the org acme: demo
// and other, ready; old, archived; one repository in each of the other statuses; and
// the task fork demo--task1 of demo. nowhere names no workspace.
const (
	demo      = "demo"
	other     = "other"
	old       = "old"
	pending   = "ws-pending"
	cloning   = "ws-cloning"
	failed    = "ws-failed"
	demoFork  = "demo--task1"
	nowhere   = "no-such-workspace"
	someRunID = "20260102_030405_a1b2c3"
)

// ingestionRoute is one route of the ingestion gate: its path after
// /workspaces/:slug/runs/:run_id, the table it stores its records in,
// whether it takes a batch, whether it keeps one record per workspace and
// run rather than one per id, and a valid body for it, new on every call.
type ingestionRoute struct {
	path   string
	table  string
	batch  bool
	perRun bool
	valid  func() any
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
	{path: "/events", table: "run_events", valid: func() any { return map[string]any{"event_type": "run.start"} }},
	{path: "/sessions/outcomes", table: "session_outcomes", valid: func() any {
		return map[string]any{"session_id": "0a1b2c3d-0000-4000-8000-0000000000aa", "status": "completed"}
	}},
	{path: "/tools/calls", table: "tool_calls", valid: func() any { return map[string]any{"tool_name": "bash"} }},
	{path: "/tools/errors", table: "tool_errors", valid: func() any { return map[string]any{"tool_name": "bash", "error_msg": "exit 1"} }},
	{path: "/traces", table: "traces", valid: func() any { return map[string]any{"event_type": "tool.use"} }},
	{path: "/postmortem", table: "postmortems", perRun: true, valid: func() any { return validPostmortem() }},
	{path: "/events/batch", table: "run_events", batch: true, valid: func() any { return []any{map[string]any{"event_type": "run.start"}} }},
	{path: "/traces/batch", table: "traces", batch: true, valid: func() any { return []any{map[string]any{"event_type": "tool.use"}} }},
}

// validPostmortem is a valid postmortem body with only its required members.
func validPostmortem() map[string]any {
	return map[string]any{
		"run_status": "stalled", "started_at": "2026-01-02T03:00:00+01:00", "completed_at": "2026-01-02T02:30:00Z",
		"task_summary": map[string]any{"total": 4, "done": 3}, "cost_summary": map[string]any{},
	}
}

// recordTables are the analytical store's tables of telemetry records.
var recordTables = []string{"run_events", "session_outcomes", "tool_calls", "tool_errors", "traces", "postmortems"}

// world is a hub on a settable clock whose analytical store fails every
// write while failWrites is set, the third insert of the next
// transaction that makes three once failThirdInsert is set, and blocks
// every write until its context ends while blockWrites is set, with the seed:
// alice owns every workspace of the seed in the org acme, of which bob is a
// member; carol has nothing.
// Each user has an API key (the field of the user's name); alicePAT is a
// PAT of alice's holding audit:write. telemetry is the hub's telemetry
// component.
type world struct {
	*hubtest.Hub
	clock           *clock
	telemetry       *telemetry.Component
	failWrites      atomic.Bool
	failThirdInsert atomic.Bool
	blockWrites     atomic.Bool

	alice, bob, carol          string
	aliceKey, bobKey, carolKey string
	alicePAT                   string

	runs, ids atomic.Int64
}

// newWorld starts the world's hub; each adjust changes the telemetry
// component's dependencies before they are bound.
func newWorld(t *testing.T, adjust ...func(*telemetry.Deps)) *world {
	t.Helper()
	return newWorldTOML(t, "", adjust...)
}

// newWorldTOML is newWorld with toml added to the hub's configuration file,
// after its [server] table.
func newWorldTOML(t *testing.T, toml string, adjust ...func(*telemetry.Deps)) *world {
	t.Helper()
	w := &world{clock: newClock()}
	w.Hub = hubtest.StartWith(t, hubtest.Config{Now: w.clock.Now, TOML: toml, Compose: func(c *hub.Composition) {
		w.telemetry = c.Telemetry()
		c.AdjustTelemetry(func(d *telemetry.Deps) {
			d.DB = faultDB{DB: d.DB, fail: &w.failWrites, failThird: &w.failThirdInsert, block: &w.blockWrites}
		})
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
		demo: access.StatusReady, other: access.StatusReady, old: access.StatusArchived, pending: access.StatusPending,
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

// freshID is a record id that no other call returns.
func (w *world) freshID() string {
	return fmt.Sprintf("0a1b2c3d-0000-4000-8000-%012x", w.ids.Add(1))
}

// post sends body to the ingestion route path of the workspace slug and the
// run runID as credential.
func (w *world) post(t *testing.T, credential, slug, runID, path string, body any) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/runs/"+runID+path, credential, body)
}

// answer is the status and body of an answer to one of several requests
// sent at once.
type answer struct {
	status int
	body   []byte
}

// postAll sends every body of bodies at once to the ingestion route path of
// the workspace slug and the run runID as credential, and returns the
// answers in the order of bodies.
func (w *world) postAll(t *testing.T, credential, slug, runID, path string, bodies []any) []answer {
	t.Helper()
	url := w.URL("/api/v1/workspaces/" + slug + "/runs/" + runID + path)
	answers := make([]answer, len(bodies))
	errs := make([]error, len(bodies))
	var wg sync.WaitGroup
	for i, body := range bodies {
		wg.Go(func() { answers[i], errs[i] = postOnce(url, credential, body) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return answers
}

// postOnce sends body as JSON to url with the bearer credential, from any
// goroutine.
func postOnce(url, credential string, body any) (answer, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return answer{}, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return answer{status: res.StatusCode, body: b}, err
}

// createdOnce checks that exactly one of answers is 201 and every other
// 200, all with the same body, and returns that body.
func createdOnce(t *testing.T, answers []answer) []byte {
	t.Helper()
	var created []byte
	statuses := map[int]int{}
	for _, a := range answers {
		statuses[a.status]++
		if a.status == http.StatusCreated {
			created = a.body
		}
	}
	if statuses[http.StatusCreated] != 1 || statuses[http.StatusOK] != len(answers)-1 {
		t.Fatalf("answers by status = %v, want one 201 and %d 200", statuses, len(answers)-1)
	}
	for _, a := range answers {
		if !bytes.Equal(a.body, created) {
			t.Errorf("answer %d %s, want the body of the 201: %s", a.status, a.body, created)
		}
	}
	return created
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

// storedRow is every column of the record id in table, by name.
func (w *world) storedRow(t *testing.T, table, id string) map[string]any {
	t.Helper()
	rows, err := w.Store.Analytics().QueryContext(context.Background(), `SELECT * FROM `+table+` WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("read %s %s: %v", table, id, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("%s holds no record %s (%v)", table, id, rows.Err())
	}
	values := make([]any, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		t.Fatalf("read %s %s: %v", table, id, err)
	}
	row := make(map[string]any, len(columns))
	for i, c := range columns {
		row[c] = values[i]
	}
	return row
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

// errInterrupted is what a blocked write of faultDB fails with when its
// context ends: like the store's driver, an error that does not wrap the
// context's.
var errInterrupted = errors.New("interrupted (9)")

// blockLimit is the longest a blocked write of faultDB waits for its
// context to end, so that a write without a limit fails the test instead
// of hanging it.
const blockLimit = 5 * time.Second

// faultDB is the analytical store with faults a test switches on: while
// fail is set, every statement of a write transaction fails; once
// failThird is set, the third insert of the next transaction that makes
// three fails, and failThird is cleared; while block is set, every
// statement of a write transaction waits until its context ends (at most
// blockLimit) and then fails.
type faultDB struct {
	telemetry.DB
	fail, failThird, block *atomic.Bool
}

func (f faultDB) BeginTx(ctx context.Context) (telemetry.Tx, error) {
	tx, err := f.DB.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, fail: f.fail, failThird: f.failThird, block: f.block}, nil
}

// faultTx is a transaction of faultDB, used by one goroutine.
type faultTx struct {
	telemetry.Tx
	fail, failThird, block *atomic.Bool
	inserts                int
}

func (f *faultTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if f.block.Load() {
		select {
		case <-ctx.Done():
			return nil, errInterrupted
		case <-time.After(blockLimit):
			return nil, fmt.Errorf("a blocked write waited %s and its context did not end", blockLimit)
		}
	}
	if f.fail.Load() {
		return nil, errInjected
	}
	if strings.HasPrefix(strings.TrimSpace(query), "INSERT") {
		f.inserts++
		if f.inserts == 3 && f.failThird.CompareAndSwap(true, false) {
			return nil, errInjected
		}
	}
	return f.Tx.ExecContext(ctx, query, args...)
}

// recorder is the telemetry component bound, without a hub, to an
// analytical store of its own on SQLite, on a clock fixed at
// 2026-01-02T03:04:05Z, with a session lookup, a log that keeps every line
// it writes, and the faults of faultDB, switched by fail and block.
type recorder struct {
	comp        *telemetry.Component
	db          *sql.DB
	log         *hubtest.LogBuffer
	fail, block atomic.Bool
	// failThird is never set; faultDB needs it.
	failThird atomic.Bool
}

// newRecorder binds the recorder to sessions, with writeTimeout as the
// write limit (zero for the default).
func newRecorder(t *testing.T, sessions telemetry.SessionRuns, writeTimeout time.Duration) *recorder {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, telemetry.SchemaSteps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &recorder{comp: &telemetry.Component{}, db: st.Analytics(), log: &hubtest.LogBuffer{}}
	r.comp.Bind(telemetry.Deps{
		DB:           faultDB{DB: telemetry.SQL(st.Analytics()), fail: &r.fail, failThird: &r.failThird, block: &r.block},
		Now:          newClock().Now,
		Logger:       slog.New(slog.NewTextHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		WriteTimeout: writeTimeout,
		Sessions:     sessions,
	})
	return r
}

// runEvents is the number of stored run events.
func (r *recorder) runEvents(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM run_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// logLines are the lines the recorder's log holds.
func (r *recorder) logLines() []string {
	return strings.Split(strings.TrimRight(r.log.String(), "\n"), "\n")
}

// sessionRuns is a session lookup over a fixed set: the run id of each
// session ("" for one without), and err, when set, as every lookup's
// failure.
type sessionRuns struct {
	runs map[string]string
	err  error
}

func (s sessionRuns) RunOf(_ context.Context, id string) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	run, found := s.runs[id]
	return run, found, nil
}
