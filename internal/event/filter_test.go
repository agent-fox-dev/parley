package event

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// eventRoutes are the two routes that read the event log, which apply the
// same filters, positions and access.
var eventRoutes = []string{"/events/log", "/events"}

// answer is how one of the event routes answered a request: its status, its
// headers and, unless it is a stream, its body.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// errorBody is the error envelope of an answer.
type errorBody struct {
	Error struct {
		Message   string          `json:"message"`
		ErrorType string          `json:"error_type"`
		OldestSeq json.RawMessage `json:"oldest_seq"`
	} `json:"error"`
}

// envelope decodes the answer's error envelope and fails the test when the
// answer is not one in JSON.
func (a answer) envelope(t testing.TB) errorBody {
	t.Helper()
	var env errorBody
	if !strings.HasPrefix(a.header.Get("Content-Type"), "application/json") || json.Unmarshal(a.body, &env) != nil {
		t.Fatalf("answer %d (%s) %s, want a JSON error envelope", a.status, a.header.Get("Content-Type"), a.body)
	}
	return env
}

// ask sends GET /api/v1 + path + ?query with credential and header to the
// world's router on a real connection and returns the answer. A stream that
// is answered 200 is closed at once.
func (w *world) ask(t testing.TB, path, credential, query string, header map[string]string) answer {
	t.Helper()
	if path == "/events" {
		s := w.openStream(t, credential, query, header)
		s.close()
		return answer{status: s.status, header: s.header, body: s.body}
	}
	srv := w.serve()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1"+path+"?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	for name, value := range header {
		req.Header.Set(name, value)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s?%s: %v", path, query, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("GET %s?%s: read the body: %v", path, query, err)
	}
	return answer{status: res.StatusCode, header: res.Header, body: body}
}

// replay opens a stream with credential, query and header and returns the
// seqs of the hub_event frames it sends before its second heartbeat, which
// the test has sent by moving the fake clock on by the heartbeat interval.
// The stream is closed before it returns.
func (w *world) replay(t testing.TB, credential, query string, header map[string]string) []int64 {
	t.Helper()
	s := w.openStream(t, credential, query, header)
	if s.status != http.StatusOK {
		t.Fatalf("GET /events?%s = %d %s, want 200", query, s.status, s.body)
	}
	s.heartbeat(2 * time.Second)
	w.clock.WaitTimers(t, 1)
	w.clock.Advance(heartbeatInterval)
	var got []int64
	for {
		f := s.next(2 * time.Second)
		if f.event == "heartbeat" {
			break
		}
		got = append(got, f.seq(t))
	}
	s.close()
	w.waitOpenStreams(t, 0)
	return got
}

// served returns the seqs route serves credential for query: the items of
// one page of the log, or the replay of a stream.
func (w *world) served(t testing.TB, route, credential, query string, header map[string]string) []int64 {
	t.Helper()
	if route == "/events" {
		return w.replay(t, credential, query, header)
	}
	if len(header) > 0 {
		t.Fatalf("the log takes no header, got %v", header)
	}
	return w.getLog(t, credential, query).seqs(t)
}

// pat stores a PAT of the user name with the permissions perms, bound to
// workspace unless it is "", and returns the token.
func (w *world) pat(name, workspace string, perms ...string) string {
	w.t.Helper()
	ctx := context.Background()
	var token string
	err := w.store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		token, _, err = identity.StorePAT(ctx, tx, identity.PATSpec{UserID: w.users[name], Name: "events",
			Permissions: perms, Workspace: workspace, CreatedAt: w.clock.Now()})
		return err
	})
	if err != nil {
		w.t.Fatalf("store a PAT of %s: %v", name, err)
	}
	return token
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
	for _, in := range []Input{first, on("parley.change.land", "api"), on("parley.claim.create", "web"), on("parley.changeset.x", "api")} {
		w.emit(in)
	}

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
			if got := w.served(t, route, w.admin, "after=0&"+tc.query, nil); !slices.Equal(got, tc.want) {
				t.Errorf("%s?%s: seqs = %v, want %v", route, tc.query, got, tc.want)
			}
		}
	}

	// Both routes send an event as the same bytes, and neither escapes HTML.
	marked := on("parley.change.open", "api")
	marked.Metadata = map[string]any{"title": "<a> & <b>"}
	seq := w.emit(marked).Seq
	after := "after=" + strconv.FormatInt(seq-1, 10)
	page := w.getLog(t, w.admin, after)
	if len(page.page.Items) != 1 {
		t.Fatalf("/events/log?%s: %d items, want 1", after, len(page.page.Items))
	}
	item := []byte(page.page.Items[0])
	if !bytes.Contains(item, []byte(`"title":"<a> & <b>"`)) {
		t.Errorf("log item = %s, want the metadata unescaped", item)
	}
	s := w.openStream(t, w.admin, after, nil)
	s.heartbeat(2 * time.Second)
	if f := s.next(2 * time.Second); f.event != "hub_event" || !bytes.Equal([]byte(f.data), item) {
		t.Errorf("stream frame %q, want a hub_event whose data is the log item %s", f.lines, item)
	}
	s.close()
}

func TestFilters_RejectMalformed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emit(on("parley.change.open", "api"))
	entries := func(n int) string {
		types := make([]string, n)
		for i := range types {
			types[i] = "parley.change.t" + string(rune('a'+i))
		}
		return strings.Join(types, ",")
	}

	for _, tc := range []struct {
		query string
		ok    bool
	}{
		{"types=" + entries(21), false},
		{"types=", false},
		{"types=a,,b", false},
		{"types=parley.change.open,", false},
		{"types=parley.*.land", false},
		{"types=*x", false},
		{"types=**", false},
		{"session_id=nope", false},
		{"session_id=", false},
		{"types=" + entries(20), true},
		{"session_id=5B1F0C1E-8D2A-4C3B-9E4F-A0B1C2D3E4F5", true},
	} {
		for _, route := range eventRoutes {
			a := w.ask(t, route, w.admin, tc.query, nil)
			if tc.ok {
				if a.status != http.StatusOK {
					t.Errorf("%s?%s: status = %d %s, want 200", route, tc.query, a.status, a.body)
				}
				continue
			}
			if env := a.envelope(t); a.status != http.StatusBadRequest || env.Error.ErrorType != "invalid_request" {
				t.Errorf("%s?%s: answer = %d %s, want 400 invalid_request", route, tc.query, a.status, a.body)
			}
		}
	}
	// The accepted cases opened streams, which give their places back once
	// the client has left, not before ask returns.
	w.waitOpenStreams(t, 0)
}

func TestFilters_WorkspaceNotReadable(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(access.Record{Slug: "gone", OwnerID: w.users["dev"], OrgID: w.acme, Kind: access.KindRepository})
	w.emit(on("parley.change.open", "api"))
	w.emit(on("parley.change.open", "gone"))
	w.emit(on("parley.change.land", "gone"))
	w.deleteWorkspace("gone")
	w.events.SetCoverage(coverage{"api": {"api"}})
	bound := w.pat("dev", "api", "audit:read")
	out, mem := w.key("out"), w.key("mem")

	for _, route := range eventRoutes {
		unreadable := w.ask(t, route, out, "workspace=api", nil)
		if env := unreadable.envelope(t); unreadable.status != http.StatusNotFound || env.Error.ErrorType != "not_found" ||
			env.Error.Message != "workspace not found" {
			t.Errorf("%s: out with workspace=api = %d %s, want 404 not_found 'workspace not found'", route, unreadable.status, unreadable.body)
		}
		missing := w.ask(t, route, out, "workspace=nosuch", nil)
		if missing.status != unreadable.status || !bytes.Equal(missing.body, unreadable.body) {
			t.Errorf("%s: out with workspace=nosuch = %d %s, want the answer to workspace=api, %d %s",
				route, missing.status, missing.body, unreadable.status, unreadable.body)
		}

		for _, tc := range []struct {
			name, credential, query string
			coverage                Coverage
			status                  int
		}{
			{"bound PAT of dev, a workspace of dev it does not cover", bound, "workspace=web", coverage{"api": {"api"}}, http.StatusNotFound},
			{"bound PAT of dev, the workspace it covers", bound, "workspace=api", coverage{"api": {"api"}}, http.StatusOK},
			{"bound PAT of dev, no covered set", bound, "workspace=api", nil, http.StatusNotFound},
			{"bound PAT of dev, no workspace filter", bound, "", nil, http.StatusOK},
			{"mem, a workspace of its org", mem, "workspace=api", nil, http.StatusOK},
			{"mem, the deleted workspace", mem, "workspace=gone", nil, http.StatusNotFound},
		} {
			w.events.SetCoverage(tc.coverage)
			a := w.ask(t, route, tc.credential, tc.query, nil)
			if a.status != tc.status {
				t.Errorf("%s: %s: status = %d %s, want %d", route, tc.name, a.status, a.body, tc.status)
			}
			if tc.status == http.StatusNotFound && !bytes.Equal(a.body, unreadable.body) {
				t.Errorf("%s: %s: body = %s, want %s", route, tc.name, a.body, unreadable.body)
			}
		}

		if got := w.served(t, route, w.admin, "after=0&workspace=gone", nil); !slices.Equal(got, []int64{2, 3}) {
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

	for _, tc := range []struct {
		route, query string
		header       map[string]string
	}{
		{"/events/log", "after=4", nil},
		{"/events/log", "cursor=0", nil},
		{"/events", "after=4", nil},
		{"/events", "", map[string]string{"Last-Event-ID": "4"}},
		{"/events", "after=7", map[string]string{"Last-Event-ID": "4"}},
	} {
		a := w.ask(t, tc.route, mem, tc.query, tc.header)
		env := a.envelope(t)
		if a.status != http.StatusGone || env.Error.ErrorType != "cursor_expired" || env.Error.Message != "event cursor expired" ||
			string(env.Error.OldestSeq) != "6" {
			t.Errorf("%s?%s %v: answer = %d %s, want 410 cursor_expired 'event cursor expired' with oldest_seq 6",
				tc.route, tc.query, tc.header, a.status, a.body)
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
			for range 3 {
				w.emit(on("parley.change.open", "api"))
			}
			// Without the notifier a waiting reader wakes only at its own
			// timer, so the expiry lands between two of its reads.
			w.stop()
			var stream *eventStream
			var held <-chan logReply
			if route == "/events" {
				stream = w.openStream(t, w.admin, "after=3", nil)
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
			r := answerWithin(t, 2*time.Second, held)
			var env errorBody
			if err := json.Unmarshal(r.raw, &env); err != nil || r.status != http.StatusGone ||
				env.Error.ErrorType != "cursor_expired" || string(env.Error.OldestSeq) != "5" {
				t.Errorf("answer = %d %s, want 410 cursor_expired with oldest_seq 5", r.status, r.raw)
			}
		})
	}
}

func TestPosition_Invalid(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	refused := func(route, query string, header map[string]string) {
		t.Helper()
		a := w.ask(t, route, w.admin, query, header)
		if env := a.envelope(t); a.status != http.StatusBadRequest || env.Error.ErrorType != "invalid_request" {
			t.Errorf("%s?%s %v: answer = %d %s, want 400 invalid_request", route, query, header, a.status, a.body)
		}
	}

	for _, route := range eventRoutes {
		refused(route, "after=1", nil)
	}
	refused("/events", "", map[string]string{"Last-Event-ID": "1"})

	for range 10 {
		w.emit(on("parley.change.open", "api"))
	}
	for _, query := range []string{"after=-1", "after=abc", "after=1.5", "after=11"} {
		for _, route := range eventRoutes {
			refused(route, query, nil)
		}
	}
	refused("/events/log", "cursor=11", nil)
	for _, id := range []string{"x", "-3", "11"} {
		refused("/events", "", map[string]string{"Last-Event-ID": id})
	}
}

func TestPosition_ValidBoundaries(t *testing.T) {
	t.Parallel()
	empty := newWorld(t)
	for _, route := range eventRoutes {
		if got := empty.served(t, route, empty.admin, "after=0", nil); len(got) != 0 {
			t.Errorf("an empty hub: %s?after=0: seqs = %v, want none", route, got)
		}
	}
	if page := empty.getLog(t, empty.admin, "after=0"); !bytes.Contains(page.raw, []byte(`"items":[]`)) {
		t.Errorf("an empty hub: /events/log?after=0: body = %s, want items == []", page.raw)
	}

	w := newWorld(t)
	w.expireUpTo(t, "api", 10, 6)
	for _, tc := range []struct {
		query  string
		header map[string]string
		want   []int64
	}{
		{"after=10", nil, nil},
		{"after=5", nil, seqRange(6, 10)},
		{"after=2", map[string]string{"Last-Event-ID": "8"}, []int64{9, 10}},
	} {
		for _, route := range eventRoutes {
			if route == "/events/log" && tc.header != nil {
				continue
			}
			if got := w.served(t, route, w.admin, tc.query, tc.header); !slices.Equal(got, tc.want) {
				t.Errorf("%s?%s %v: seqs = %v, want %v", route, tc.query, tc.header, got, tc.want)
			}
		}
	}
}

func TestRoutes_ScopeAndCallers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emit(on("parley.change.open", "api"))
	w.events.SetCoverage(coverage{"api": {"api"}})
	noScope := w.pat("dev", "", "changes:read")

	for _, tc := range []struct {
		name, credential, query string
		status                  int
	}{
		{"PAT with changes:read only", noScope, "", http.StatusForbidden},
		{"the same PAT with a malformed filter", noScope, "types=*x", http.StatusForbidden},
		{"the same PAT with an unreadable workspace", noScope, "workspace=nosuch", http.StatusForbidden},
		{"the same PAT with a position past the log", noScope, "after=99", http.StatusForbidden},
		{"PAT with audit:read", w.pat("dev", "", "audit:read"), "", http.StatusOK},
		{"PAT with audit:write", w.pat("dev", "", "audit:write"), "", http.StatusOK},
		{"API key", w.key("dev"), "", http.StatusOK},
		{"admin token", w.admin, "", http.StatusOK},
		{"PAT with audit:read bound to api", w.pat("dev", "api", "audit:read"), "workspace=api", http.StatusOK},
	} {
		for _, route := range eventRoutes {
			a := w.ask(t, route, tc.credential, tc.query, nil)
			if a.status != tc.status {
				t.Errorf("%s: %s: status = %d %s, want %d", route, tc.name, a.status, a.body, tc.status)
				continue
			}
			if tc.status == http.StatusForbidden {
				if env := a.envelope(t); env.Error.ErrorType != "insufficient_scope" {
					t.Errorf("%s: %s: error_type = %q, want insufficient_scope", route, tc.name, env.Error.ErrorType)
				}
			}
		}
	}
}
