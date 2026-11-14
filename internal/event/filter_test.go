package event

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
)

// eventRoutes are the two routes that read the event log, which apply the
// same filters, positions and access.
var eventRoutes = []string{"/events/log", "/events"}

// replay opens a stream with credential, query and lastEventID and returns
// the seqs of the hub_event frames it sends before its second heartbeat,
// which the test has sent by moving the fake clock on by the heartbeat
// interval. The stream is closed before it returns.
func (w *world) replay(t testing.TB, credential, query, lastEventID string) []int64 {
	t.Helper()
	s := w.openStream(t, credential, query, lastEventID)
	if s.status != http.StatusOK {
		t.Fatalf("GET /events?%s = %d %s, want 200", query, s.status, s.body)
	}
	s.heartbeat(2 * time.Second)
	w.clock.WaitTimers(t, 1)
	w.clock.Advance(heartbeatInterval)
	var got []int64
	for f := s.next(2 * time.Second); f.event != "heartbeat"; f = s.next(2 * time.Second) {
		got = append(got, f.seq(t))
	}
	s.close()
	w.waitOpenStreams(t, 0)
	return got
}

// served returns the seqs route serves credential for query: the items of
// one page of the log, or the replay of a stream.
func (w *world) served(t testing.TB, route, credential, query string) []int64 {
	t.Helper()
	if route == "/events" {
		return w.replay(t, credential, query, "")
	}
	return w.getLog(t, credential, query).seqs(t)
}

// expireUpTo emits n events on slug, the first keep-1 of them an hour
// before the others, and expires the older ones, so that oldest_seq is the
// seq of the keep-th event emitted.
func (w *world) expireUpTo(t testing.TB, slug string, n, keep int) {
	t.Helper()
	for i := range n {
		if i == keep-1 {
			w.clock.Advance(time.Hour)
		}
		w.emit(on("parley.change.open", slug))
	}
	if _, err := w.events.Expire(context.Background(), w.clock.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if got := w.bounds().OldestSeq; got != int64(keep) {
		t.Fatalf("oldest_seq = %d, want %d", got, keep)
	}
}

func TestFilters_SameOnStreamAndLog(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	session := "5b1f0c1e-8d2a-4c3b-9e4f-a0b1c2d3e4f5"
	first := on("parley.change.open", "api")
	first.SessionID = ptr(session)
	w.emitTx(first, on("parley.change.land", "api"), on("parley.claim.create", "web"), on("parley.changeset.x", "api"))

	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"workspace=api", []int64{1, 2, 4}},
		{"types=parley.change.*", []int64{1, 2}},
		{"types=parley.change.land,parley.claim.*", []int64{2, 3}},
		{"types=*", []int64{1, 2, 3, 4}},
		{"types=parley.nothing.here", nil},
		{"session_id=" + strings.ToUpper(session), []int64{1}},
		{"workspace=api&types=parley.change.*&session_id=" + session, []int64{1}},
		{"unknown=1", []int64{1, 2, 3, 4}},
	} {
		for _, route := range eventRoutes {
			if got := w.served(t, route, w.admin, "after=0&"+tc.query); !slices.Equal(got, tc.want) {
				t.Errorf("%s?%s: seqs = %v, want %v", route, tc.query, got, tc.want)
			}
		}
	}

	// Both routes send an event as the same bytes, and neither escapes HTML.
	marked := on("parley.change.open", "api")
	marked.Metadata = map[string]any{"title": "<a> & <b>"}
	after := "after=" + strconv.FormatInt(w.emit(marked).Seq-1, 10)
	page := w.getLog(t, w.admin, after)
	if len(page.page.Items) != 1 || !bytes.Contains(page.page.Items[0], []byte(`"title":"<a> & <b>"`)) {
		t.Fatalf("/events/log?%s: body %s, want one item with the metadata unescaped", after, page.body)
	}
	s := w.openStream(t, w.admin, after, "")
	s.heartbeat(2 * time.Second)
	if f := s.next(2 * time.Second); f.event != "hub_event" || !bytes.Equal([]byte(f.data), page.page.Items[0]) {
		t.Errorf("stream frame %q, want a hub_event whose data is the log item %s", f.lines, page.page.Items[0])
	}
}

func TestRoutes_RejectBadParameters(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// Events to return, so that an accepted wait answers at once.
	w.emitMany(10)
	types := func(n int) string {
		out := make([]string, n)
		for i := range out {
			out[i] = "parley.change.t" + string(rune('a'+i))
		}
		return strings.Join(out, ",")
	}
	log := eventRoutes[:1]
	for _, tc := range []struct {
		routes, queries []string
		ok              bool
	}{
		{log, []string{"limit=0", "limit=201", "limit=x", "wait=-1", "wait=61", "wait=1.5", "wait=x", "after=3&cursor=4", "cursor=11"}, false},
		{eventRoutes, []string{"types=" + types(21), "types=", "types=a,,b", "types=parley.change.open,", "types=parley.*.land",
			"types=*x", "types=**", "session_id=nope", "session_id=", "after=-1", "after=abc", "after=1.5", "after=11"}, false},
		{log, []string{"limit=1", "limit=200", "wait=0", "wait=60"}, true},
		{eventRoutes, []string{"types=" + types(20), "session_id=5B1F0C1E-8D2A-4C3B-9E4F-A0B1C2D3E4F5"}, true},
	} {
		for _, route := range tc.routes {
			for _, query := range tc.queries {
				a := w.ask(t, route, w.admin, query, "")
				if !tc.ok {
					a.refused(t, route+"?"+query, http.StatusBadRequest, "invalid_request")
				} else if a.status != http.StatusOK {
					t.Errorf("%s?%s: status = %d %s, want 200", route, query, a.status, a.body)
				}
			}
		}
	}
	for _, id := range []string{"x", "-3", "11"} {
		w.ask(t, "/events", w.admin, "", id).refused(t, "Last-Event-ID "+id, http.StatusBadRequest, "invalid_request")
	}
	// The accepted cases opened streams, which give their places back once
	// the client has left, not before ask returns.
	w.waitOpenStreams(t, 0)
}

func TestFilters_WorkspaceNotReadable(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(access.Record{Slug: "gone", OwnerID: w.users["dev"], OrgID: w.acme, Kind: access.KindRepository})
	w.emitTx(on("parley.change.open", "api"), on("parley.change.open", "gone"), on("parley.change.land", "gone"))
	w.deleteWorkspace("gone")
	w.events.SetCoverage(coverage{"api": {"api"}})
	bound := w.patToken("dev", "api", "audit:read")
	out, mem := w.key("out"), w.key("mem")

	for _, route := range eventRoutes {
		unreadable := w.ask(t, route, out, "workspace=api", "")
		if env := unreadable.refused(t, route+": out with workspace=api", http.StatusNotFound, "not_found"); env.Error.Message != "workspace not found" {
			t.Errorf("%s: message %q, want 'workspace not found'", route, env.Error.Message)
		}
		for _, tc := range []struct {
			name, credential, query string
			coverage                Coverage
			status                  int
		}{
			{"out, a workspace that does not exist", out, "workspace=nosuch", nil, http.StatusNotFound},
			{"bound PAT of dev, a workspace of dev it does not cover", bound, "workspace=web", coverage{"api": {"api"}}, http.StatusNotFound},
			{"bound PAT of dev, the workspace it covers", bound, "workspace=api", coverage{"api": {"api"}}, http.StatusOK},
			{"bound PAT of dev, no covered set", bound, "workspace=api", nil, http.StatusNotFound},
			{"bound PAT of dev, no workspace filter", bound, "", nil, http.StatusOK},
			{"mem, a workspace of its org", mem, "workspace=api", nil, http.StatusOK},
			{"mem, the deleted workspace", mem, "workspace=gone", nil, http.StatusNotFound},
		} {
			w.events.SetCoverage(tc.coverage)
			a := w.ask(t, route, tc.credential, tc.query, "")
			if a.status != tc.status || (tc.status == http.StatusNotFound && !bytes.Equal(a.body, unreadable.body)) {
				t.Errorf("%s: %s: answer = %d %s, want %d, and the body of an unreadable workspace when 404", route, tc.name, a.status, a.body, tc.status)
			}
		}

		if got := w.served(t, route, w.admin, "after=0&workspace=gone"); !slices.Equal(got, []int64{2, 3}) {
			t.Errorf("%s: the admin token with workspace=gone: seqs = %v, want the events stored under it, [2 3]", route, got)
		}
	}
}

func TestPosition_Expired(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// mem cannot read web: an expired position is refused whether or not the
	// caller could have seen what left the log.
	w.expireUpTo(t, "web", 10, 6)
	mem := w.key("mem")

	for _, tc := range []struct{ route, query, lastEventID string }{
		{"/events/log", "after=4", ""},
		{"/events/log", "cursor=0", ""},
		{"/events", "after=4", ""},
		{"/events", "", "4"},
		{"/events", "after=7", "4"},
	} {
		what := tc.route + "?" + tc.query + " Last-Event-ID " + tc.lastEventID
		env := w.ask(t, tc.route, mem, tc.query, tc.lastEventID).refused(t, what, http.StatusGone, "cursor_expired")
		if env.Error.Message != "event cursor expired" || string(env.Error.OldestSeq) != "6" {
			t.Errorf("%s: envelope %+v, want 'event cursor expired' with oldest_seq 6", what, env.Error)
		}
	}
	if got := w.events.Counters().OpenStreams; got != 0 {
		t.Errorf("open_streams = %d, want 0", got)
	}
}

func TestPosition_ExpiresWhileOpen(t *testing.T) {
	t.Parallel()
	for _, route := range eventRoutes {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.emitMany(3)
			// Without the notifier a waiting reader wakes only at its own
			// timer, so the expiry lands between two of its reads.
			w.stop()
			var stream *eventStream
			var held <-chan answer
			if route == "/events" {
				stream = w.openStream(t, w.admin, "after=3", "")
				if stream.status != http.StatusOK {
					t.Fatalf("GET /events?after=3 = %d %s, want 200", stream.status, stream.body)
				}
				stream.heartbeat(2 * time.Second)
			} else {
				held = w.holdLog(w.admin, "wait=60&after=3")
			}
			w.clock.WaitTimers(t, 1)
			w.emit(on("parley.change.open", "api"))
			w.emit(on("parley.change.open", "api"))
			// A cutoff after every event keeps only the newest, 5, so the
			// reader's position 3 is below oldest_seq minus 1.
			if oldest, err := w.events.Expire(context.Background(), w.clock.Now().Add(time.Hour)); err != nil || oldest != 5 {
				t.Fatalf("Expire = %d, %v, want 5", oldest, err)
			}

			if route == "/events" {
				w.clock.Advance(heartbeatInterval)
				stream.ends(2 * time.Second)
				return
			}
			w.clock.Advance(maxLogWait * time.Second)
			if env := answerWithin(t, 2*time.Second, held).refused(t, "the held request", http.StatusGone, "cursor_expired"); string(env.Error.OldestSeq) != "5" {
				t.Errorf("oldest_seq = %s, want 5", env.Error.OldestSeq)
			}
		})
	}
}

func TestPosition_Boundaries(t *testing.T) {
	t.Parallel()
	empty := newWorld(t)
	for _, route := range eventRoutes {
		if got := empty.served(t, route, empty.admin, "after=0"); len(got) != 0 {
			t.Errorf("an empty hub: %s?after=0: seqs = %v, want none", route, got)
		}
		empty.ask(t, route, empty.admin, "after=1", "").refused(t, "an empty hub: "+route+"?after=1", http.StatusBadRequest, "invalid_request")
	}
	empty.ask(t, "/events", empty.admin, "", "1").refused(t, "an empty hub: Last-Event-ID 1", http.StatusBadRequest, "invalid_request")
	if page := empty.getLog(t, empty.admin, "after=0"); !bytes.Contains(page.body, []byte(`"items":[]`)) {
		t.Errorf("an empty hub: /events/log?after=0: body = %s, want items == []", page.body)
	}

	w := newWorld(t)
	w.expireUpTo(t, "api", 10, 6)
	for _, route := range eventRoutes {
		for query, want := range map[string][]int64{"after=10": nil, "after=5": seqRange(6, 10)} {
			if got := w.served(t, route, w.admin, query); !slices.Equal(got, want) {
				t.Errorf("%s?%s: seqs = %v, want %v", route, query, got, want)
			}
		}
	}
	if got := w.replay(t, w.admin, "after=2", "8"); !slices.Equal(got, []int64{9, 10}) {
		t.Errorf("/events?after=2 with Last-Event-ID 8: seqs = %v, want [9 10]", got)
	}
}

func TestRoutes_ScopeAndCallers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emitMany(1)
	w.events.SetCoverage(coverage{"api": {"api"}})
	noScope := w.patToken("dev", "", "changes:read")

	for _, tc := range []struct {
		name, credential, query string
		status                  int
	}{
		{"PAT with changes:read only", noScope, "", http.StatusForbidden},
		{"the same PAT with a malformed filter", noScope, "types=*x", http.StatusForbidden},
		{"the same PAT with an unreadable workspace", noScope, "workspace=nosuch", http.StatusForbidden},
		{"the same PAT with a position past the log", noScope, "after=99", http.StatusForbidden},
		{"PAT with audit:read", w.patToken("dev", "", "audit:read"), "", http.StatusOK},
		{"PAT with audit:write", w.patToken("dev", "", "audit:write"), "", http.StatusOK},
		{"API key", w.key("dev"), "", http.StatusOK},
		{"admin token", w.admin, "", http.StatusOK},
		{"PAT with audit:read bound to api", w.patToken("dev", "api", "audit:read"), "workspace=api", http.StatusOK},
	} {
		for _, route := range eventRoutes {
			a := w.ask(t, route, tc.credential, tc.query, "")
			if tc.status == http.StatusForbidden {
				a.refused(t, route+": "+tc.name, http.StatusForbidden, "insufficient_scope")
			} else if a.status != tc.status {
				t.Errorf("%s: %s: status = %d %s, want %d", route, tc.name, a.status, a.body, tc.status)
			}
		}
	}
}
