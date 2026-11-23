package sandbox_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hubtest"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestGet_AnswersObject(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	omar := h.CreateUser(t, "omar")
	h.AddMember(t, h.org, omar)
	callers := []struct{ name, cred string }{
		{"dana's key", h.key},
		{"omar's key, read access", h.IssueKey(t, omar)},
		{"dana's PAT with sandboxes:read only", h.pat(t, h.dana, "sandboxes:read")},
	}
	for _, rc := range []struct{ state, reason string }{
		{sandbox.StateProvisioning, ""},
		{sandbox.StateReady, ""},
		{sandbox.StateFailed, sandbox.ReasonProvisionFailed},
		{sandbox.StateDeleted, "api"},
	} {
		t.Run(rc.state, func(t *testing.T) {
			id := h.seedRecord(t, "demo", rc.state, func(r *sandbox.Record) { r.StateReason = rc.reason })
			rec, err := sandbox.Get(context.Background(), h.Store.Main(), id)
			if err != nil {
				t.Fatal(err)
			}
			want := objectMap(t, rec)
			want["runtime_reachable"] = true
			var first []byte
			for _, c := range callers {
				a := h.call(t, http.MethodGet, sandboxPath("demo", id), c.cred, nil)
				o := decoded(t, a, http.StatusOK)
				if !reflect.DeepEqual(o, want) {
					t.Errorf("%s: object %v; want the stored record's %v", c.name, o, want)
				}
				if rc.state == sandbox.StateDeleted && o["ended_at"] == nil {
					t.Errorf("%s: a deleted sandbox has no ended_at", c.name)
				}
				if first == nil {
					first = a.body
				} else if string(a.body) != string(first) {
					t.Errorf("%s: answer %s; want the owner's %s", c.name, a.body, first)
				}
			}
		})
	}
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	other := h.seedRecord(t, "demo--t1", sandbox.StateReady, nil)
	var bodies []string
	for _, id := range []string{"sbx_" + strings.Repeat("0", 32), other, "nope", "SBX_" + strings.Repeat("ab", 16)} {
		a := h.call(t, http.MethodGet, sandboxPath("demo", id), h.key, nil)
		if a.status != http.StatusNotFound || a.err.ErrorType != "sandbox_not_found" {
			t.Errorf("get %s under demo = %d %s; want 404 sandbox_not_found", id, a.status, a.body)
		}
		bodies = append(bodies, string(a.body))
	}
	for i, b := range bodies[1:] {
		if b != bodies[0] {
			t.Errorf("answer %d = %s; want the same as the first, %s", i+1, b, bodies[0])
		}
	}
}

func TestGet_WaitFor(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, state, query string
		// wait is the wait the query asks for.
		wait time.Duration
		// to is the state the sandbox is moved to once the request waits; ""
		// for none, when the test clock passes the wait instead.
		to, want string
		// held is whether the request waits rather than answering at once.
		held bool
	}{
		{"ready already holds", sandbox.StateReady, "wait_for=ready&wait=30", 30 * time.Second, "", sandbox.StateReady, false},
		{"an ended sandbox ends a wait for ready", sandbox.StateFailed, "wait_for=ready&wait=30", 30 * time.Second, "", sandbox.StateFailed, false},
		{"becomes ready during the wait", sandbox.StateProvisioning, "wait_for=ready&wait=30", 30 * time.Second, sandbox.StateReady, sandbox.StateReady, true},
		{"the wait passes", sandbox.StateReady, "wait_for=ended&wait=2", 2 * time.Second, "", sandbox.StateReady, true},
		{"stops during the wait", sandbox.StateReady, "wait_for=ended&wait=30", 30 * time.Second, sandbox.StateStopped, sandbox.StateStopped, true},
		{"one second passes", sandbox.StateProvisioning, "wait_for=ready&wait=1", time.Second, "", sandbox.StateProvisioning, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := h.seedRecord(t, "demo", tc.state, nil)
			before := h.timers.created(tc.wait)
			answered := h.getting(ctx, sandboxPath("demo", id)+"?"+tc.query, h.key)
			if tc.held {
				hubtest.Eventually(t, "the request to wait", func() bool { return h.timers.created(tc.wait) > before })
				select {
				case a := <-answered:
					t.Fatalf("answered %d %s before the wait ended", a.status, a.body)
				default:
				}
				if tc.to != "" {
					if applied, err := h.svc.Transition(ctx, nil, id, tc.state, tc.to, "", nil); err != nil || !applied {
						t.Fatalf("Transition = %t, %v", applied, err)
					}
				} else {
					h.clock.Advance(tc.wait)
				}
			}
			select {
			case a := <-answered:
				if o := decoded(t, a, http.StatusOK); o["state"] != tc.want {
					t.Errorf("state = %v; want %s", o["state"], tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("no answer within 1 s")
			}
		})
	}

	// A woken read reads the record as committed, without waiting for the
	// store's turn of writes, which a caller's open transaction holds.
	t.Run("a held turn of writes does not hold the answer", func(t *testing.T) {
		id := h.seedRecord(t, "demo", sandbox.StateProvisioning, nil)
		other := h.seedRecord(t, "demo", sandbox.StateReady, nil)
		before := h.timers.created(30 * time.Second)
		answered := h.getting(ctx, sandboxPath("demo", id)+"?wait_for=ready&wait=30", h.key)
		hubtest.Eventually(t, "the request to wait", func() bool { return h.timers.created(30*time.Second) > before })
		// A change that notified no read: the move of the other sandbox
		// notifies them.
		if _, err := h.Store.Main().Exec(`UPDATE sandboxes SET state = ? WHERE id = ?`, sandbox.StateReady, id); err != nil {
			t.Fatal(err)
		}
		moved, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- h.Store.Tx(ctx, func(tx *sql.Tx) error {
				applied, err := h.svc.Transition(ctx, tx, other, sandbox.StateReady, sandbox.StateStopped, "", nil)
				if err == nil && !applied {
					err = errors.New("the move of the other sandbox did not apply")
				}
				close(moved)
				<-release
				return err
			})
		}()
		hubtestWait(t, moved, "the other sandbox's move")
		inTime := false
		select {
		case a := <-answered:
			inTime = true
			if o := decoded(t, a, http.StatusOK); o["state"] != sandbox.StateReady {
				t.Errorf("state = %v; want ready", o["state"])
			}
		case <-time.After(time.Second):
			t.Error("no answer within 1 s while a transaction held the turn of writes")
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !inTime {
			<-answered
		}
	})

	t.Run("a cancelled request leaves nothing behind", func(t *testing.T) {
		id := h.seedRecord(t, "demo", sandbox.StateProvisioning, nil)
		const wait = 45 * time.Second
		cctx, cancel := context.WithCancel(ctx)
		answered := h.getting(cctx, sandboxPath("demo", id)+"?wait_for=ready&wait=45", h.key)
		hubtest.Eventually(t, "the request to wait", func() bool { return h.timers.live(wait) == 1 })
		cancel()
		if a := <-answered; a.status != 0 {
			t.Errorf("a cancelled request got %d %s", a.status, a.body)
		}
		hubtest.Eventually(t, "the handler to stop waiting once its request was cancelled", func() bool { return h.timers.live(wait) == 0 })
	})
}

func TestGet_RefusesBadWait(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	id := h.seedRecord(t, "demo", sandbox.StateReady, nil)
	for _, q := range []string{
		"wait_for=ready", "wait=10", "wait_for=done&wait=10", "wait_for=ready&wait=0", "wait_for=ready&wait=61",
		"wait_for=ready&wait=1.5", "wait_for=ready&wait=x", "wait_for=ended&wait=-1",
	} {
		if a := h.call(t, http.MethodGet, sandboxPath("demo", id)+"?"+q, h.key, nil); a.status != http.StatusBadRequest || a.err.ErrorType != "invalid_request" {
			t.Errorf("get ?%s = %d %s; want 400 invalid_request", q, a.status, a.body)
		}
	}
	decoded(t, h.call(t, http.MethodGet, sandboxPath("demo", id)+"?wait_for=ready&wait=60", h.key, nil), http.StatusOK)
	before := h.timers.created(time.Second)
	answered := h.getting(context.Background(), sandboxPath("demo", id)+"?wait_for=ended&wait=1", h.key)
	hubtest.Eventually(t, "the request to wait", func() bool { return h.timers.created(time.Second) > before })
	h.clock.Advance(time.Second)
	select {
	case a := <-answered:
		decoded(t, a, http.StatusOK)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer to wait_for=ended&wait=1")
	}
}

func TestList_FiltersAndOrder(t *testing.T) {
	t.Parallel()
	var omar string
	h := startCreateHub(t, agentProfile, func(d *sandbox.Deps) {
		next := d.Readable
		d.Readable = func(ctx context.Context, c access.Caller) (access.ReadableSet, error) {
			if c.UserID == omar {
				return access.ReadableSet{Entries: map[string]access.Level{"demo": access.LevelMember, "demo--m1": access.LevelMember}}, nil
			}
			return next(ctx, c)
		}
	})
	h.world(t)
	h.fork(t, "demo--m1", "demo", recordStart.Add(7*24*time.Hour), nil)
	omar = h.CreateUser(t, "omar")
	h.AddMember(t, h.org, omar)
	omarKey := h.IssueKey(t, omar)
	session := recordSession
	a := h.seedRecord(t, "demo", sandbox.StateReady, func(r *sandbox.Record) {
		r.SessionID, r.Labels = &session, map[string]string{"team": "core", "tier": "1"}
	})
	b := h.seedRecord(t, "demo", sandbox.StateStopped, func(r *sandbox.Record) { r.Labels = map[string]string{"team": "core"} })
	c := h.seedRecord(t, "demo", sandbox.StateDeleted, nil)
	d := h.seedRecord(t, "demo--t1", sandbox.StateReady, func(r *sandbox.Record) { r.CreatedAt = recordStart.Add(-time.Hour) })
	e := h.seedRecord(t, "demo--m1", sandbox.StateFailed, func(r *sandbox.Record) { r.CreatedAt = recordStart.Add(time.Hour) })

	for _, tc := range []struct {
		name, cred, slug, query string
		want                    []string
	}{
		{"all but deleted", h.key, "demo", "", newestFirst(a, b)},
		{"deleted when asked", h.key, "demo", "state=deleted", []string{c}},
		{"any of the states", h.key, "demo", "state=ready&state=stopped", newestFirst(a, b)},
		{"the session", h.key, "demo", "session_id=" + recordSession, []string{a}},
		{"a label", h.key, "demo", "label=team=core", newestFirst(a, b)},
		{"every label", h.key, "demo", "label=team=core&label=tier=1", []string{a}},
		{"a label none has", h.key, "demo", "label=team=web", []string{}},
		{"the forks the caller can read", h.key, "demo", "include_forks=true", append(append([]string{e}, newestFirst(a, b)...), d)},
		{"not a fork the caller cannot read", omarKey, "demo", "include_forks=true", append([]string{e}, newestFirst(a, b)...)},
		{"include_forks=false", h.key, "demo", "include_forks=false", newestFirst(a, b)},
		{"include_forks on a fork", h.key, "demo--t1", "include_forks=true", []string{d}},
		{"a workspace without sandboxes", h.key, "demo--t2", "", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/workspaces/" + tc.slug + "/sandboxes"
			if tc.query != "" {
				path += "?" + tc.query
			}
			if got, next := listIDs(t, h.call(t, http.MethodGet, path, tc.cred, nil)); !slices.Equal(got, tc.want) || next != "" {
				t.Errorf("ids %v, next_cursor %q; want %v and null", got, next, tc.want)
			}
		})
	}

	t.Run("pages of two", func(t *testing.T) {
		query := "/workspaces/demo/sandboxes?include_forks=true&state=ready&state=stopped&state=deleted&state=failed&limit=2"
		var got []string
		cursor := ""
		for range 4 {
			path := query
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			ids, next := listIDs(t, h.call(t, http.MethodGet, path, h.key, nil))
			got = append(got, ids...)
			if cursor = next; cursor == "" {
				break
			}
		}
		if want := append(append([]string{e}, newestFirst(a, b, c)...), d); !slices.Equal(got, want) || cursor != "" {
			t.Errorf("pages %v ending with cursor %q; want %v ending with null", got, cursor, want)
		}
	})

	t.Run("the hub-wide list reads the stored sandboxes", func(t *testing.T) {
		got, _ := listIDs(t, h.call(t, http.MethodGet, "/sandboxes?state=ready", h.Token, nil))
		if want := []string{a, d}; !slices.Equal(got, want) {
			t.Errorf("GET /sandboxes?state=ready = %v; want %v", got, want)
		}
	})
}

func TestList_RefusesBadQuery(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	for _, q := range []string{"state=running", "state=ready&state=", "label=team", "label==x", "include_forks=yes", "include_forks=1"} {
		if a := h.call(t, http.MethodGet, "/workspaces/demo/sandboxes?"+q, h.key, nil); a.status != http.StatusBadRequest || a.err.ErrorType != "invalid_request" {
			t.Errorf("list ?%s = %d %s; want 400 invalid_request", q, a.status, a.body)
		}
	}
}

func TestRead_Authorization(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	id := h.seedRecord(t, "demo", sandbox.StateReady, nil)
	off := startProbeHub(t, false)
	stranger := h.IssueKey(t, h.CreateUser(t, "stranger"))
	gitRead := h.pat(t, h.dana, "git:read")
	for _, route := range []struct{ name, suffix string }{{"get", "/" + id}, {"list", ""}} {
		for _, tc := range []struct {
			name       string
			hub        *probeHub
			slug, cred string
			status     int
			errorType  string
		}{
			{"sandboxes not enabled", off, "ws", off.IssueKey(t, off.owner), http.StatusNotFound, "sandboxes_disabled"},
			{"a PAT with git:read", h.probeHub, "demo", gitRead, http.StatusForbidden, "insufficient_scope"},
			{"the same PAT on a slug that does not exist", h.probeHub, "ghost", gitRead, http.StatusForbidden, "insufficient_scope"},
			{"a stranger", h.probeHub, "demo", stranger, http.StatusNotFound, "not_found"},
			{"the admin token", h.probeHub, "demo", h.Token, http.StatusOK, ""},
			// The hub's scope check lets a write scope satisfy its read scope.
			{"a PAT with sandboxes:write only", h.probeHub, "demo", h.pat(t, h.dana, "sandboxes:write"), http.StatusOK, ""},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				a := tc.hub.call(t, http.MethodGet, "/workspaces/"+tc.slug+"/sandboxes"+route.suffix, tc.cred, nil)
				if a.status != tc.status || a.err.ErrorType != tc.errorType {
					t.Errorf("= %d %s; want %d %q", a.status, a.body, tc.status, tc.errorType)
				}
			})
		}
	}
}

func TestRead_RuntimeUnreachable(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	ctx := context.Background()
	id := h.seedRecord(t, "demo", sandbox.StateReady, nil)
	reachable := func(want bool) {
		t.Helper()
		g := decoded(t, h.call(t, http.MethodGet, sandboxPath("demo", id), h.key, nil), http.StatusOK)
		if g["runtime_reachable"] != want || g["state"] != sandbox.StateReady {
			t.Errorf("get: runtime_reachable %v, state %v; want %t, ready", g["runtime_reachable"], g["state"], want)
		}
		l := decoded(t, h.call(t, http.MethodGet, "/workspaces/demo/sandboxes", h.key, nil), http.StatusOK)
		items, _ := l["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("list %v; want the one sandbox", l)
		}
		if it := items[0].(map[string]any); it["runtime_reachable"] != want || it["id"] != id {
			t.Errorf("list item %v; want %s with runtime_reachable %t", it, id, want)
		}
	}
	h.rt.SetUnreachable(true)
	if _, err := h.svc.Client().ListSandboxes(ctx); err == nil {
		t.Fatal("ListSandboxes of an unreachable runtime succeeded")
	}
	reachable(false)
	h.rt.SetUnreachable(false)
	if _, err := h.svc.Client().ListSandboxes(ctx); err != nil {
		t.Fatal(err)
	}
	reachable(true)
}
