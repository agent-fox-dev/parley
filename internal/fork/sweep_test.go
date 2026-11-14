package fork_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestRunPass_RemovesEveryExpiredFork(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	w.addParent(t, "demo2", func(ws *workspace.Workspace) { ws.Status = workspace.StatusArchived })
	rows := []struct {
		slug    string
		in      time.Duration
		removed bool
	}{
		{"demo--past", -time.Second, true},
		{"demo--now", 0, true},
		{"demo--later", time.Second, false},
		{"demo2--past", -time.Second, true},
	}
	var removed []string
	var closed [][2]string
	for _, row := range rows {
		w.seedFork(t, row.slug, expiring(w, row.in), func(f *fork.Fork) { f.ParentSlug = new(strings.Split(row.slug, "--")[0]) })
		if row.removed {
			removed, closed = append(removed, row.slug), append(closed, [2]string{row.slug, "fork expired"})
		}
	}
	slices.Sort(removed)
	slices.SortFunc(closed, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	r.take()

	if res := w.pass(t, context.Background()); !reflect.DeepEqual(res.Removed, removed) {
		t.Errorf("removed = %v, want %v in ascending slug order", res.Removed, removed)
	}
	for _, row := range rows {
		want := map[bool]int{true: http.StatusNotFound, false: http.StatusOK}[row.removed]
		if a := w.get(t, w.devKey, "/workspaces/"+row.slug); a.status != want {
			t.Errorf("GET %s = %d, want %d", row.slug, a.status, want)
		}
	}
	if gotRemoved, gotClosed, _ := r.take(); !reflect.DeepEqual(gotRemoved, removed) || !reflect.DeepEqual(gotClosed, closed) {
		t.Errorf("remover asked %v, session closer %v; want %v and %v", gotRemoved, gotClosed, removed, closed)
	}
}

func TestRunPass_WritesOneExpireEvent(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	session := uuid.New().String()
	for _, row := range []struct {
		name, slug, reason string
		sessionID          *string
	}{
		{"past expires_at", "demo--ttl", "ttl", nil},
		{"past expires_at in a session", "demo--session", "ttl", &session},
		{"a day after it landed", "demo--landed", "landed", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			slug, sessionID := row.slug, row.sessionID
			edit := expiring(w, -time.Minute)
			if row.reason == "landed" {
				edit = func(f *fork.Fork) { f.CreatedAt = w.clock().Add(-day) }
				r.set(func(r *remover) { r.changes[slug] = []fork.SourceChange{{Branch: "task", State: "landed"}} })
			}
			h := w.seedHeld(t, slug, edit, func(f *fork.Fork) { f.SessionID = sessionID })
			_, b1 := w.pat(t, h.slug, nil, "workspaces:read")
			_, b2 := w.pat(t, h.slug, nil, "workspaces:read")
			w.pass(t, context.Background())
			if evs := w.eventsFor(t, fork.EventDelete, h.slug); len(evs) != 0 {
				t.Errorf("delete events = %d, want none", len(evs))
			}
			evs := w.eventsFor(t, fork.EventExpire, h.slug)
			if len(evs) != 1 {
				t.Fatalf("expire events = %d, want 1", len(evs))
			}
			ev := evs[0]
			if ev.ResourceID != h.slug || ev.ActorType != event.ActorSystem || ev.ActorID != nil {
				t.Errorf("event resource %s, actor %s %v; want %s by the system", ev.ResourceID, ev.ActorType, ev.ActorID, h.slug)
			}
			if !reflect.DeepEqual(ev.SessionID, sessionID) {
				t.Errorf("session_id = %v, want %v", ev.SessionID, sessionID)
			}
			var meta map[string]any
			if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			var tokens []any
			for _, id := range slices.Sorted(slices.Values([]string{h.patID, b1, b2})) {
				tokens = append(tokens, id)
			}
			wantMembers(t, meta, map[string]any{"parent_slug": parentSlug, "reason": row.reason,
				"expires_at": timefmt.Format(h.expires), "revoked_tokens": tokens})
		})
	}
}

func TestRunPass_GoesOnPastAForkItCannotRemove(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	for name, fail := range map[string]func(*remover){
		"sandbox collaborator":  func(r *remover) { r.failSandbox = true },
		"remover of owned data": func(r *remover) { r.failRemove = true },
	} {
		t.Run(name, func(t *testing.T) {
			p := "demo--" + name[:2]
			a, b, c := p+"a", p+"b", p+"c"
			for _, slug := range []string{a, b, c} {
				w.seedFork(t, slug, expiring(w, -time.Second))
			}
			r.set(func(r *remover) { r.only = b; fail(r) })
			t.Cleanup(func() { r.set(func(r *remover) { r.only, r.failSandbox, r.failRemove = "", false, false }) })
			if res := w.pass(t, context.Background()); !reflect.DeepEqual(res.Failed, []string{b}) {
				t.Errorf("failed = %v, want [%s]", res.Failed, b)
			}
			for slug, want := range map[string]int{a: http.StatusNotFound, b: http.StatusOK, c: http.StatusNotFound} {
				if got := w.get(t, w.devKey, "/workspaces/"+slug).status; got != want {
					t.Errorf("GET %s = %d, want %d", slug, got, want)
				}
			}
			if n := len(w.eventsFor(t, fork.EventExpire, b)); n != 0 {
				t.Errorf("expire events of %s = %d, want none", b, n)
			}
			if !strings.Contains(w.Log.String(), `"slug":"`+b+`"`) {
				t.Errorf("no log record names %s; log = %s", b, w.Log.String())
			}
			r.set(func(r *remover) { r.only, r.failSandbox, r.failRemove = "", false, false })
			w.pass(t, context.Background())
			wantError(t, w.get(t, w.devKey, "/workspaces/"+b), http.StatusNotFound, "not_found")
			if n := len(w.eventsFor(t, fork.EventExpire, b)); n != 1 {
				t.Errorf("expire events of %s = %d, want 1", b, n)
			}
		})
	}
}

func TestRunPass_LeavesTheForksOfABusyParentForALaterPass(t *testing.T) {
	t.Parallel()
	w, _ := newRemovalWorld(t)
	for _, c := range []struct {
		name   string
		status workspace.Status
		marker workspace.Marker
	}{
		{"pending", workspace.StatusPending, workspace.MarkerNone},
		{"cloning", workspace.StatusCloning, workspace.MarkerNone},
		{"maintenance marker", workspace.StatusReady, workspace.MarkerArchive},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug := "demo--" + c.name[:2]
			w.seedFork(t, slug, expiring(w, -time.Second))
			w.setParent(t, c.status, c.marker)
			w.pass(t, context.Background())
			w.record(t, slug)
			if n := len(w.eventsFor(t, fork.EventExpire, slug)); n != 0 {
				t.Errorf("expire events = %d, want none while the parent is busy", n)
			}
			w.exec(t, `UPDATE workspaces SET status = ?, maintenance = ? WHERE slug = ?`, workspace.StatusReady, workspace.MarkerNone, parentSlug)
			w.pass(t, context.Background())
			if _, found, err := fork.BySlug(context.Background(), w.Store.Main(), slug); err != nil || found {
				t.Errorf("fork record: found %v, %v; want it gone", found, err)
			}
			if n := len(w.eventsFor(t, fork.EventExpire, slug)); n != 1 {
				t.Errorf("expire events = %d, want 1", n)
			}
		})
	}
}

func TestRunPass_EndsAfterTheRemovalInProgressWhenCancelled(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	slugs := []string{"demo--a", "demo--b", "demo--c"}
	for _, slug := range slugs {
		w.seedFork(t, slug, expiring(w, -time.Second))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.set(func(r *remover) {
		r.during = func(slug string) {
			if slug == slugs[1] {
				cancel()
			}
		}
	})
	if res := w.pass(t, ctx); !res.Stopped {
		t.Errorf("pass = %+v, want it stopped", res)
	}
	for i, slug := range slugs {
		_, found, err := fork.BySlug(context.Background(), w.Store.Main(), slug)
		if err != nil {
			t.Fatal(err)
		}
		events := len(w.eventsFor(t, fork.EventExpire, slug))
		switch {
		case found == (events != 0):
			t.Errorf("%s: record %v with %d expire events, want removed with one event or whole without", slug, found, events)
		case i == 0 && found, i == 2 && !found:
			t.Errorf("%s: record %v, want the first removed and the last whole", slug, found)
		}
	}
}

func TestRunPass_ReturnsAtOnceWhileAnotherPassRuns(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	w.seedFork(t, "demo--a", expiring(w, -time.Second))
	entered, release := make(chan struct{}), make(chan struct{})
	r.set(func(r *remover) {
		r.during = func(string) {
			close(entered)
			<-release
		}
	})
	done := make(chan fork.PassResult)
	go func() {
		// A pass the hub runs itself at that moment is waited for first.
		for {
			if res := w.forks.RunPass(context.Background()); !res.Skipped {
				done <- res
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	within(t, "the first pass reaches the sandboxes", func() { <-entered })
	if res := w.forks.RunPass(context.Background()); !res.Skipped || len(res.Removed) != 0 {
		t.Errorf("second pass = %+v, want it skipped", res)
	}
	if removed, _, _ := r.take(); len(removed) != 0 {
		t.Errorf("remover of owned data asked %v while the first pass waits, want nothing", removed)
	}
	close(release)
	var res fork.PassResult
	within(t, "the first pass ends", func() { res = <-done })
	if !reflect.DeepEqual(res.Removed, []string{"demo--a"}) {
		t.Errorf("first pass = %+v, want demo--a removed", res)
	}
}

func TestSweepLoop_PassesAtStartAndAtEachTickUntilCancelled(t *testing.T) {
	t.Parallel()
	var passes atomic.Int32
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		fork.SweepLoop(ctx, ticks, func(context.Context) { passes.Add(1) })
	}()
	eventually(t, "the pass at start", func() bool { return passes.Load() == 1 })
	within(t, "the loop takes a tick", func() { ticks <- time.Now() })
	eventually(t, "the pass of the tick", func() bool { return passes.Load() == 2 })
	cancel()
	within(t, "the loop ends", func() { <-ended })
	select {
	case ticks <- time.Now():
		t.Error("the loop took a tick after its context was cancelled")
	case <-time.After(50 * time.Millisecond):
	}
	if n := passes.Load(); n != 2 {
		t.Errorf("passes = %d, want 2", n)
	}
}

func TestSweep_RunsAtEachTickAndAsTheMaintenanceTask(t *testing.T) {
	t.Parallel()
	tk := &ticker{c: make(chan time.Time)}
	w, _ := newRemovalWorld(t, tk.compose)
	w.seedFork(t, "demo--a", expiring(w, -time.Second))
	within(t, "the sweep takes a tick", func() { tk.c <- time.Now() })
	eventually(t, "the tick's pass removes demo--a", func() bool { return len(w.eventsFor(t, fork.EventExpire, "demo--a")) == 1 })

	// Held, the sweep waits for its ticker before its first pass, so only
	// the maintenance loop's first pass can remove the fork.
	w.seedFork(t, "demo--b", expiring(w, -time.Second))
	tk.hold.Lock()
	defer tk.hold.Unlock()
	w.Restart(t)
	eventually(t, "the maintenance pass removes demo--b", func() bool { return len(w.eventsFor(t, fork.EventExpire, "demo--b")) == 1 })
}

func TestRunPass_KeepsAnExpiredForkWhoseLifetimeWasSetAgain(t *testing.T) {
	t.Parallel()
	w, _ := newRemovalWorld(t)
	created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "t1", "ttl": "5m"}))
	w.advance(6 * time.Minute)
	if a := w.get(t, w.devKey, "/workspaces/demo--t1"); a.status != http.StatusOK {
		t.Fatalf("GET after expires_at = %d %s, want 200", a.status, a.body)
	}
	got := w.patch(t, w.devKey, "/workspaces/demo--t1", map[string]any{"ttl": "1h"}).object(t, http.StatusOK)["expires_at"]
	if want := timefmt.Format(w.clock().Add(time.Hour)); got != want {
		t.Errorf("expires_at = %v, want %s", got, want)
	}
	w.pass(t, context.Background())
	if a := w.get(t, w.devKey, "/workspaces/demo--t1"); a.status != http.StatusOK {
		t.Errorf("GET after the pass = %d %s, want 200", a.status, a.body)
	}
	if n := len(w.eventsOf(t, fork.EventExpire)); n != 0 {
		t.Errorf("expire events = %d, want none", n)
	}
}

func TestRunPass_RepeatsADiscardThatFailedUntilItSucceeds(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	failing := func(fail bool) { r.set(func(r *remover) { r.failDiscard = fail }) }
	del := func(slug string) {
		failing(true)
		if a := w.call(t, http.MethodDelete, "/workspaces/"+slug, w.devKey, nil); a.status != http.StatusNoContent {
			t.Fatalf("DELETE = %d %s, want 204", a.status, a.body)
		}
	}

	t.Run("removed fork", func(t *testing.T) {
		h := w.seedHeld(t, "demo--t1")
		del(h.slug)
		if n := len(w.eventsFor(t, fork.EventDelete, h.slug)); n != 1 {
			t.Errorf("delete events = %d, want 1", n)
		}
		wantError(t, w.get(t, w.devKey, "/workspaces/"+h.slug), http.StatusNotFound, "not_found")
		w.pass(t, context.Background())
		if !w.pending(t, h.slug) || w.head(t, w.workspaces.RepoPath(h.slug), "task") != h.task {
			t.Fatal("a discard that fails again must stay pending, with the repository in place")
		}
		failing(false)
		w.pass(t, context.Background())
		if _, err := os.Stat(w.workspaces.RepoPath(h.slug)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("repository of the fork: %v, want it gone with its branch task", err)
		}
		if w.pending(t, h.slug) {
			t.Error("the discard is still pending after it succeeded")
		}
	})

	t.Run("fork created again under the name", func(t *testing.T) {
		w.seedHeld(t, "demo--t2")
		del("demo--t2")
		failing(false)
		created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "t2"}))
		w.pass(t, context.Background())
		if w.pending(t, "demo--t2") {
			t.Error("the discard is still pending though a fork has the slug again")
		}
		if _, err := os.Stat(w.workspaces.RepoPath("demo--t2")); err != nil {
			t.Errorf("repository of the new fork: %v, want it kept by the discard of the old one", err)
		}
	})
}

// expiring sets a fork's expires_at to in from the time of w.
func expiring(w *world, in time.Duration) func(*fork.Fork) {
	return func(f *fork.Fork) { f.ExpiresAt = new(w.clock().Add(in)) }
}

// pass runs one sweep pass with ctx and returns what it did; a pass the hub
// runs itself at that moment is waited for first.
func (w *world) pass(t *testing.T, ctx context.Context) fork.PassResult {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		res := w.forks.RunPass(ctx)
		if !res.Skipped {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatal("every sweep pass was skipped for 5 s")
		}
	}
}

// pending reports whether a discard of the fork slug's repository is
// pending.
func (w *world) pending(t *testing.T, slug string) bool {
	t.Helper()
	var n int
	if err := w.Store.Main().QueryRowContext(context.Background(), `SELECT count(*) FROM fork_discards WHERE slug = ?`, slug).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// ticker is the sweep's ticker in a test: a tick is a send on c, and while
// hold is locked the sweep waits for its ticker before its first pass.
type ticker struct {
	c    chan time.Time
	hold sync.Mutex
}

// compose makes tk the sweep's ticker of the hub being composed.
func (tk *ticker) compose(c *hub.Composition) {
	c.AdjustForks(func(d *fork.Deps) {
		d.SweepTicks = func() (<-chan time.Time, func()) {
			tk.hold.Lock()
			defer tk.hold.Unlock()
			return tk.c, func() {}
		}
	})
}

// within runs fn, which waits for something, and fails the test when it
// has not returned within 10 s.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: not within 10 s", what)
	}
}

// eventually waits up to 10 s for cond to hold and fails the test, naming
// what, when it does not.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 10 s", what)
		}
	}
}
