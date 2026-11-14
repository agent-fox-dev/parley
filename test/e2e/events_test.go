package e2e

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// rowsStep creates the table of the test route's rows.
var rowsStep = store.Step{
	Name:   "e2e_rows",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE e2e_rows (name TEXT NOT NULL PRIMARY KEY, workspace TEXT NOT NULL)`)
		return err
	},
}

// rowsPath is the path of the test route.
const rowsPath = "/api/v1/test/rows"

// startEventsHub starts a hub with cfg, as hubtest.StartWith does, with one
// more route, which stands in for the routes of the packages that emit:
// POST /api/v1/test/rows with {"name": …, "workspace": …} writes that row to
// a table of its own and emits parley.change.open on the workspace, with
// the name as its resource_id, in the same store transaction, and answers
// 201 with the event's record. A name already written fails the
// transaction: the answer is 500 internal, and neither the row nor the
// event is kept. The route needs a credential and no scope.
func startEventsHub(t testing.TB, cfg hubtest.Config) *hubtest.Hub {
	t.Helper()
	compose := cfg.Compose
	cfg.Compose = func(c *hub.Composition) {
		var st *store.Store
		c.AddSchemaStep(rowsStep)
		c.AddStartHook("e2e-rows", func(_ context.Context, rt *hub.Runtime) error {
			st = rt.Store
			return nil
		})
		events := c.Events()
		c.AddRoute(server.Route{Method: http.MethodPost, Pattern: "/test/rows", Handler: func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				Name      string `json:"name"`
				Workspace string `json:"workspace"`
			}
			if err := server.DecodeJSON(r, &in); err != nil {
				return err
			}
			ctx := r.Context()
			var rec event.Record
			err := st.Tx(ctx, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO e2e_rows (name, workspace) VALUES (?, ?)`, in.Name, in.Workspace); err != nil {
					return err
				}
				var err error
				rec, err = events.Emit(ctx, tx, event.Input{Type: "parley.change.open", Workspace: &in.Workspace,
					ActorType: event.ActorAdminToken, ResourceType: "change", ResourceID: in.Name})
				return err
			})
			if err != nil {
				return err
			}
			server.WriteJSON(w, http.StatusCreated, rec)
			return nil
		}})
		if compose != nil {
			compose(c)
		}
	}
	return hubtest.StartWith(t, cfg)
}

// hubEvent is the part of a hub event record the end-to-end flows read.
type hubEvent struct {
	Seq        int64   `json:"seq"`
	Type       string  `json:"type"`
	Workspace  *string `json:"workspace"`
	ResourceID string  `json:"resource_id"`
}

// logItems returns the items of GET /api/v1/events/log?after=0 as the admin
// token, and fails the test unless it is answered 200.
func logItems(t testing.TB, h *hubtest.Hub) []hubEvent {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?after=0", h.Token, nil)
	var page struct {
		Items []hubEvent `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET /api/v1/events/log?after=0 = %d %s, want 200 with a page", status, body)
	}
	return page.Items
}

// sseFrame is one frame of the event stream: its id, its event name and
// its data line.
type sseFrame struct {
	ID    int64
	Event string
	Data  string
}

// sseClient is an open GET /api/v1/events connection whose frames arrive
// on frames until the stream ends, when frames is closed.
type sseClient struct {
	res    *http.Response
	frames chan sseFrame
}

// openEventStream opens GET path with credential and the extra headers,
// and fails the test unless it is answered 200 with an event stream. The
// connection is closed when the test ends.
func openEventStream(t *testing.T, h *hubtest.Hub, path, credential string, header map[string]string) *sseClient {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.URL(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("GET %s = %d %s, want 200 with an event stream", path, res.StatusCode, body)
	}
	c := &sseClient{res: res, frames: make(chan sseFrame, 64)}
	go func() {
		defer close(c.frames)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				c.frames <- f
				f = sseFrame{}
			case strings.HasPrefix(line, "id: "):
				f.ID, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	t.Cleanup(c.close)
	return c
}

// close drops the connection, as a client that goes away does.
func (c *sseClient) close() { c.res.Body.Close() }

// next returns the next frame of kind event, skipping frames of other
// kinds, and fails the test when none arrives within 5 s.
func (c *sseClient) next(t *testing.T, event string) sseFrame {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				t.Fatalf("the stream ended while waiting for a %s frame", event)
			}
			if f.Event == event {
				return f
			}
		case <-timeout:
			t.Fatalf("no %s frame within 5 s", event)
		}
	}
}

// postRow calls the test route with credential, which writes the row name
// on workspace and emits its event, and returns the event's seq.
func postRow(t *testing.T, h *hubtest.Hub, credential, name, workspace string) int64 {
	t.Helper()
	status, body := h.Call(t, http.MethodPost, rowsPath, credential, map[string]string{"name": name, "workspace": workspace})
	var rec hubEvent
	if status != http.StatusCreated || json.Unmarshal(body, &rec) != nil || rec.Seq == 0 {
		t.Fatalf("POST %s %s = %d %s, want 201 with the event", rowsPath, name, status, body)
	}
	return rec.Seq
}

// teamOnAPI creates the developer dev, who owns the repository workspace
// 'api' of the org acme, and a second user of acme, and returns their ids.
func teamOnAPI(t *testing.T, h *hubtest.Hub, second string) (dev, other string) {
	t.Helper()
	dev, other = h.CreateUser(t, "dev"), h.CreateUser(t, second)
	org := h.CreateOrg(t, "acme")
	h.AddMember(t, org, dev)
	h.AddMember(t, org, other)
	h.AddWorkspace(t, access.Record{Slug: "api", OwnerID: dev, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady})
	return dev, other
}

// rowWithEvent reports whether the row name exists and the log holds its
// event, with seq.
func rowWithEvent(t *testing.T, h *hubtest.Hub, name string, seq int64) bool {
	t.Helper()
	var rows int
	if err := h.Store.Main().QueryRow(`SELECT count(*) FROM e2e_rows WHERE name = ?`, name).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	for _, it := range logItems(t, h) {
		if it.Seq == seq {
			return rows == 1 && it.ResourceID == name
		}
	}
	return false
}

func TestEvents_StreamResumesWithoutLoss(t *testing.T) {
	t.Parallel()
	h := startEventsHub(t, hubtest.Config{})
	dev, reviewer := teamOnAPI(t, h, "reviewer")
	devKey := h.IssueKey(t, dev)
	pat := h.IssuePAT(t, identity.PATSpec{UserID: reviewer, Permissions: []string{"audit:read"}})
	const path = "/api/v1/events?types=parley.change.*"

	first := openEventStream(t, h, path, pat, nil)
	first.next(t, "heartbeat")
	s := postRow(t, h, devKey, "c1", "api")
	if f := first.next(t, "hub_event"); f.ID != s {
		t.Fatalf("the live frame has id %d, want the event %d", f.ID, s)
	}
	first.close()

	missed := []int64{postRow(t, h, devKey, "c2", "api"), postRow(t, h, devKey, "c3", "api")}
	second := openEventStream(t, h, path, pat, map[string]string{"Last-Event-ID": strconv.FormatInt(s, 10)})
	var got []int64
	for range missed {
		got = append(got, second.next(t, "hub_event").ID)
	}
	if want := []int64{s + 1, s + 2}; !slices.Equal(got, want) || !slices.Equal(missed, want) {
		t.Fatalf("replayed %v of the events written meanwhile %v, want %v", got, missed, want)
	}
	live := postRow(t, h, devKey, "c4", "api")
	if f := second.next(t, "hub_event"); f.ID != s+3 || live != s+3 {
		t.Fatalf("the live frame after the replay has id %d (event %d), want %d", f.ID, live, s+3)
	}
	for i, name := range []string{"c1", "c2", "c3", "c4"} {
		if !rowWithEvent(t, h, name, s+int64(i)) {
			t.Errorf("row %s and its event %d are not both kept", name, s+int64(i))
		}
	}
}

// fakeNow is a clock the test sets.
type fakeNow struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeNow) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeNow) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestEvents_LongPollAndExpiredCursor(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := &fakeNow{now: t0}
	var events *event.Component
	h := startEventsHub(t, hubtest.Config{Now: clock.Now, Compose: func(c *hub.Composition) { events = c.Events() }})
	dev, agent := teamOnAPI(t, h, "agent")
	devKey := h.IssueKey(t, dev)
	pat := h.IssuePAT(t, identity.PATSpec{UserID: agent, Permissions: []string{"audit:read"}})
	for i := range 5 {
		postRow(t, h, devKey, "r"+strconv.Itoa(i+1), "api")
	}

	type answer struct {
		status int
		body   []byte
	}
	held := make(chan answer, 1)
	go func() {
		status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?wait=30&after=5", pat, nil)
		held <- answer{status, body}
	}()
	time.Sleep(200 * time.Millisecond) // let the request be held before the event is written
	committed := postRow(t, h, devKey, "r6", "api")
	var page struct {
		Items     []hubEvent `json:"items"`
		NextAfter int64      `json:"next_after"`
	}
	select {
	case a := <-held:
		if a.status != http.StatusOK || json.Unmarshal(a.body, &page) != nil || len(page.Items) != 1 || page.Items[0].Seq != committed || page.NextAfter != committed {
			t.Fatalf("the held long-poll = %d %s, want 200 with the event %d", a.status, a.body, committed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held long-poll was not answered within 5 s of the commit")
	}

	clock.advance(3 * time.Hour)
	last := postRow(t, h, devKey, "r7", "api")
	oldest, err := events.Expire(context.Background(), t0.Add(time.Hour))
	if err != nil || oldest != last {
		t.Fatalf("Expire(T+1h) = %d, %v, want %d", oldest, err, last)
	}

	status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?after=2", pat, nil)
	var gone struct {
		Error struct {
			ErrorType string `json:"error_type"`
			OldestSeq int64  `json:"oldest_seq"`
		} `json:"error"`
	}
	if status != http.StatusGone || json.Unmarshal(body, &gone) != nil || gone.Error.ErrorType != "cursor_expired" || gone.Error.OldestSeq != last {
		t.Fatalf("the stale position = %d %s, want 410 cursor_expired with oldest_seq %d", status, body, last)
	}
	resume := strconv.FormatInt(gone.Error.OldestSeq-1, 10)
	status, body = h.Call(t, http.MethodGet, "/api/v1/events/log?after="+resume, pat, nil)
	page.Items = nil
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.Items[0].Seq != last {
		t.Fatalf("resuming at oldest_seq-1 = %d %s, want 200 with exactly the event %d", status, body, last)
	}
}

// lineBuffer is the stdout of a command that runs while the test reads it.
type lineBuffer struct {
	hubtest.LogBuffer
}

// lines returns the complete lines written so far.
func (b *lineBuffer) lines() []string {
	text := b.String()
	i := strings.LastIndexByte(text, '\n')
	if i < 0 {
		return nil
	}
	return strings.Split(text[:i], "\n")
}

// waitLines waits up to 10 s for n complete lines.
func (b *lineBuffer) waitLines(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if l := b.lines(); len(l) >= n {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("stdout has %d lines after 10 s, want %d: %q", len(b.lines()), n, b.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEvents_TailSurvivesRestart(t *testing.T) {
	t.Parallel()
	// Each run of the hub composes a new event component; the test follows
	// the current one to see when the tail's stream is open.
	var events atomic.Pointer[event.Component]
	h := startEventsHub(t, hubtest.Config{Compose: func(c *hub.Composition) { events.Store(c.Events()) }})
	dev, _ := teamOnAPI(t, h, "other")
	key := h.IssueKey(t, dev)
	var stdout lineBuffer
	var stderr hubtest.LogBuffer
	waitStream := func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for events.Load().Counters().OpenStreams == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("the tail has no stream open after 10 s; stderr = %q", stderr.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exit := make(chan int, 1)
	go func() {
		exit <- cli.Run(ctx, []string{"events", "tail", "--workspace", "api", "--endpoint-url", h.URL(""), "--api-key", key}, &stdout, &stderr)
	}()
	// Without --after the tail prints only events written after its stream
	// opened, so the first ones are written once it is open.
	waitStream()
	before := []int64{postRow(t, h, key, "b1", "api"), postRow(t, h, key, "b2", "api")}
	stdout.waitLines(t, 2)

	// The events written right after the restart may come before the tail
	// has reconnected: it resumes from the last position it received.
	h.Restart(t)
	after := []int64{postRow(t, h, key, "a1", "api"), postRow(t, h, key, "a2", "api")}
	lines := stdout.waitLines(t, 4)
	cancel()
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("events tail exited %d after the interrupt, want 0; stderr = %q", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("events tail did not return within 5 s of the interrupt")
	}

	var seqs []int64
	for _, line := range stdout.lines() {
		var rec hubEvent
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Workspace == nil || *rec.Workspace != "api" {
			t.Fatalf("stdout line %q is not an event of api", line)
		}
		seqs = append(seqs, rec.Seq)
	}
	if want := append(before, after...); len(lines) != 4 || !slices.Equal(seqs, want) {
		t.Fatalf("printed seqs %v, want each of %v once, in order", seqs, want)
	}
	if after[0] != before[1]+1 || seqs[3] != seqs[0]+3 {
		t.Errorf("seqs %v are not four consecutive numbers across the restart", seqs)
	}
}
