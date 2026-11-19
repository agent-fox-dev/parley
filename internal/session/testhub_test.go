package session_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/session"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// TestMain runs the package's tests at most hubtest.HubParallel at a time:
// each starts a hub, and hubs contend for one lock past that (see
// hubtest.HubParallel).
func TestMain(m *testing.M) {
	hubtest.LimitParallel(hubtest.HubParallel)
	os.Exit(m.Run())
}

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

// Advance moves the clock d forward.
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// stamp is the clock's time as the hub writes a timestamp.
func (c *clock) stamp() string { return c.Now().UTC().Truncate(time.Second).Format(time.RFC3339) }

// The workspaces of the seed: alice's repository demo and its task forks,
// and dave's repository other.
const (
	demo  = "demo"
	fork1 = "demo--t1"
	fork2 = "demo--t2"
	other = "other"
)

// world is a hub with the session component composed, on a settable clock,
// and the seed: alice owns demo and its forks demo--t1 and demo--t2 in the
// org acme, of which bob and erin are members; carol has nothing; dave owns
// other in the org zeta. Each user has an API key (the field of the user's
// name); alicePAT is a PAT of alice's holding sessions:write.
type world struct {
	*hubtest.Hub
	clock *clock
	// sessions is the hub's session component, events its event log and
	// workspaces its workspace service.
	sessions   *session.Component
	events     *event.Component
	workspaces *workspace.Service

	alice, bob, carol, dave, erin       string
	aliceKey, bobKey, carolKey, daveKey string
	erinKey, alicePAT                   string

	// acme is the id of the org of demo and its forks.
	acme string
}

// newWorld starts the world's hub; each adjust changes the session
// component's dependencies before they are bound.
func newWorld(t *testing.T, adjust ...func(*session.Deps)) *world {
	t.Helper()
	w := &world{clock: newClock()}
	w.Hub = hubtest.StartWith(t, hubtest.Config{Now: w.clock.Now, Compose: func(c *hub.Composition) {
		w.sessions, w.events, w.workspaces = c.Sessions(), c.Events(), c.Workspaces()
		for _, a := range adjust {
			c.AdjustSessions(a)
		}
	}})
	w.alice, w.bob, w.carol = w.CreateUser(t, "alice"), w.CreateUser(t, "bob"), w.CreateUser(t, "carol")
	w.dave, w.erin = w.CreateUser(t, "dave"), w.CreateUser(t, "erin")
	w.aliceKey, w.bobKey, w.carolKey = w.IssueKey(t, w.alice), w.IssueKey(t, w.bob), w.IssueKey(t, w.carol)
	w.daveKey, w.erinKey = w.IssueKey(t, w.dave), w.IssueKey(t, w.erin)
	w.alicePAT = w.pat(t, w.alice, "", "sessions:write")
	w.acme = w.CreateOrg(t, "acme")
	zeta := w.CreateOrg(t, "zeta")
	for _, u := range []string{w.alice, w.bob, w.erin} {
		w.AddMember(t, w.acme, u)
	}
	w.AddWorkspace(t, access.Record{Slug: demo, OwnerID: w.alice, OrgID: w.acme, Kind: access.KindRepository, Status: access.StatusReady})
	for _, f := range []string{fork1, fork2} {
		w.AddWorkspace(t, access.Record{Slug: f, OwnerID: w.alice, OrgID: w.acme, Kind: access.KindFork, ParentSlug: demo, Status: access.StatusReady})
	}
	w.AddWorkspace(t, access.Record{Slug: other, OwnerID: w.dave, OrgID: zeta, Kind: access.KindRepository, Status: access.StatusReady})
	return w
}

// pat issues a PAT of the user userID with permissions, bound to the
// workspace bound ("" for none).
func (w *world) pat(t *testing.T, userID, bound string, permissions ...string) string {
	t.Helper()
	return w.IssuePAT(t, identity.PATSpec{UserID: userID, Permissions: permissions, Workspace: bound})
}

// post sends POST /sessions with body as credential.
func (w *world) post(t *testing.T, credential string, body any) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodPost, "/api/v1/sessions", credential, body)
}

// open opens a session with body as credential, fails the test unless it
// is answered 201, and returns the session object.
func (w *world) open(t *testing.T, credential string, body any) map[string]any {
	t.Helper()
	status, raw := w.post(t, credential, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /sessions %v = %d %s, want 201", body, status, raw)
	}
	return decode(t, raw)
}

// get sends GET /sessions/:id as credential.
func (w *world) get(t *testing.T, credential, id string) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodGet, "/api/v1/sessions/"+id, credential, nil)
}

// removeWorkspace deletes the record of the workspace slug, as a removal
// leaves it.
func (w *world) removeWorkspace(t *testing.T, slug string) {
	t.Helper()
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE slug = ?`, slug)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// setStatus sets the status of the workspace slug's record.
func (w *world) setStatus(t *testing.T, slug, status string) {
	t.Helper()
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ? WHERE slug = ?`, status, slug)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// count is the number of rows of table.
func (w *world) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := w.Store.Main().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// usageCount is the number of usage records of the session id.
func (w *world) usageCount(t *testing.T, id string) int {
	t.Helper()
	var n int
	err := w.Store.Main().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM session_usage WHERE session_id = ?`, id).Scan(&n)
	if err != nil {
		t.Fatalf("count the usage records of session %s: %v", id, err)
	}
	return n
}

// freshIDs counts the ids freshID has returned.
var freshIDs atomic.Int64

// freshID is a UUID, in lower case, that no other call returns.
func freshID() string {
	n := freshIDs.Add(1)
	return fmt.Sprintf("%08x-f00d-4000-8000-%012x", n, n)
}

// terminate sets the session id to terminated, ended at the clock's time,
// directly in the store, as a forced close leaves it.
func (w *world) terminate(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET status = 'terminated', ended_at = ? WHERE id = ?`, w.clock.stamp(), id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rawPost sends POST /sessions with body as credential without the test's
// helpers, for a goroutine, which must not end the test.
func (w *world) rawPost(credential string, body any) (int, []byte, error) {
	return w.rawPostTo("/api/v1/sessions", credential, body)
}

// rawPostTo is rawPost to the path path.
func (w *world) rawPostTo(path, credential string, body any) (int, []byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, w.URL(path), bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, b, err
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

// boundCoverage is a coverage that counts a PAT bound to a workspace as
// bound, covering that workspace alone.
type boundCoverage struct{}

func (boundCoverage) Covers(_ context.Context, c access.Caller, slug string) (bool, bool, error) {
	return c.Workspace != "", c.Workspace == slug, nil
}
