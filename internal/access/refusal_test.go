package access_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
)

// refusalHub is a guard hub with the identity routes, the probe route and
// the allowlisted GET /workspaces, alice's workspaces w1 and w5, and a
// refusal limiter between the guard and a recording emitter, on a clock the
// test advances.
type refusalHub struct {
	*guardHub
	alice  string
	events *recordingEmitter
	clock  *testClock
}

func newRefusalHub(t *testing.T) *refusalHub {
	h := newGuardHub(t)
	h.identityRoutes()
	h.probe(http.MethodGet, probePattern, access.NeedRead, "git:read")
	h.probe(http.MethodGet, "/workspaces", "", "")
	alice, org := h.user("alice")
	seed(t, h.st, repo("w1", alice, org, access.StatusReady), repo("w5", alice, org, access.StatusReady))
	r := &refusalHub{guardHub: h, alice: alice, events: &recordingEmitter{}, clock: newTestClock()}
	h.limit(r.events, r.clock, slog.New(slog.DiscardHandler))
	return r
}

// The two refusals the tests send: a workspace outside the binding (404)
// and an identity route (403).
const (
	otherWorkspace = "/workspaces/w5/probe"
	identityRoute  = "/user"
)

// refuse sends GET path with token and fails the test unless the guard
// refused it.
func (r *refusalHub) refuse(path, token string) answer {
	r.t.Helper()
	a := r.do(http.MethodGet, path, token)
	if e := decode(r.t, a); e.Error.ErrorType != "not_found" && e.Error.ErrorType != "token_bound" {
		r.t.Fatalf("GET %s = %d %s, want a refusal of the guard", path, a.status, a.body)
	}
	return a
}

// eventsOf returns the metadata of the events emitted so far for the token
// tokenID, in order.
func (r *refusalHub) eventsOf(tokenID string) []map[string]any {
	var got []map[string]any
	for _, e := range r.events.all() {
		if e.event.ResourceID == tokenID {
			got = append(got, e.event.Metadata)
		}
	}
	return got
}

func TestRefusalLimiter_ReportsATokensFirstRefusalAsOneEvent(t *testing.T) {
	r := newRefusalHub(t)
	const gitPattern = "/git/:org/:slug.git/git-upload-pack"

	for _, tc := range []struct {
		name, method, path    string
		git                   bool // GuardGit on w5 instead of path
		status                int
		route, surface, typed string // typed: the requested workspace, "" for none
	}{
		{"another workspace", http.MethodGet, otherWorkspace, false, http.StatusNotFound, "/api/v1" + probePattern, "api", "w5"},
		{"a slug with no workspace, as typed", http.MethodGet, "/workspaces/NoSuch/probe", false, http.StatusNotFound, "/api/v1" + probePattern, "api", "NoSuch"},
		{"an identity route", http.MethodGet, identityRoute, false, http.StatusForbidden, "/api/v1/user", "api", ""},
		{"git on another workspace", http.MethodPost, "", true, http.StatusNotFound, gitPattern, "git", "w5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, id := r.pat(r.alice, "w1", "git:read")
			before := len(r.events.all())
			if tc.git {
				d, err := r.svc.GuardGit(context.Background(), boundPAT(r.alice, id, "w1"), "w5", tc.method, gitPattern)
				if err != nil || d.Pass || d.Status != tc.status {
					t.Fatalf("GuardGit = %+v, %v, want a refusal with %d", d, err, tc.status)
				}
			} else if a := r.do(tc.method, tc.path, tok); a.status != tc.status {
				t.Fatalf("%s %s = %d %s, want %d", tc.method, tc.path, a.status, a.body, tc.status)
			}
			got := r.events.all()[before:]
			if len(got) != 1 {
				t.Fatalf("%d events, want 1: %+v", len(got), got)
			}
			want := access.Event{
				Type: "parley.token.bound_refused", Workspace: "w1", ActorType: "pat", ActorID: r.alice,
				ResourceType: "token", ResourceID: id,
				Metadata: map[string]any{
					"token_id": id, "bound_workspace": "w1", "method": tc.method, "route": tc.route,
					"status": tc.status, "surface": tc.surface, "suppressed": 0,
				},
			}
			if tc.typed != "" {
				want.Metadata["requested_workspace"] = tc.typed
			}
			if !reflect.DeepEqual(got[0].event, want) || got[0].tx != nil {
				t.Errorf("emitted %+v in tx %v,\nwant %+v emitted now", got[0].event, got[0].tx, want)
			}
		})
	}

	tok, id := r.pat(r.alice, "w1", "git:read")
	r.refuse(identityRoute, tok)
	r.refuse(identityRoute, tok)
	r.limit(r.events, r.clock, slog.New(slog.DiscardHandler))
	r.refuse(identityRoute, tok)
	if got := r.eventsOf(id); len(got) != 2 || got[1]["suppressed"] != 0 {
		t.Errorf("events across a new limiter = %+v, want a second event with suppressed 0", got)
	}
}

func TestRefusalLimiter_SuppressesFurtherRefusalsOfATokenForAMinute(t *testing.T) {
	r := newRefusalHub(t)
	tokA, idA := r.pat(r.alice, "w1", "git:read")
	tokB, _ := r.pat(r.alice, "w1", "git:read")
	reported := map[string]answer{identityRoute: r.refuse(identityRoute, tokB), otherWorkspace: r.refuse(otherWorkspace, tokA)}

	for _, step := range []struct {
		after time.Duration
		path  string
	}{{time.Second, otherWorkspace}, {29 * time.Second, identityRoute}, {29 * time.Second, otherWorkspace}} {
		r.clock.advance(step.after)
		a, want := r.refuse(step.path, tokA), reported[step.path]
		if a.status != want.status || !bytes.Equal(a.body, want.body) {
			t.Errorf("suppressed GET %s = %d %s, reported %d %s", step.path, a.status, a.body, want.status, want.body)
		}
	}
	if got := r.eventsOf(idA); len(got) != 1 {
		t.Errorf("events of the token = %+v, want only the first", got)
	}
}

func TestRefusalLimiter_CarriesTheSuppressedCountOnTheNextEventAfterAMinute(t *testing.T) {
	r := newRefusalHub(t)
	tokA, idA := r.pat(r.alice, "w1", "git:read")

	r.refuse(otherWorkspace, tokA)
	for _, step := range []struct {
		after time.Duration
		path  string
	}{{time.Second, otherWorkspace}, {29 * time.Second, identityRoute}, {29 * time.Second, otherWorkspace}, {time.Second, identityRoute}} {
		r.clock.advance(step.after)
		r.refuse(step.path, tokA)
	}
	got := r.eventsOf(idA)
	if len(got) != 2 {
		t.Fatalf("events = %+v, want 2", got)
	}
	if e := got[1]; e["suppressed"] != 3 || e["route"] != "/api/v1/user" || e["status"] != http.StatusForbidden || e["requested_workspace"] != nil {
		t.Errorf("event at t0+60s = %+v, want suppressed 3 and the 403 of GET /api/v1/user", e)
	}
	r.clock.advance(time.Minute)
	r.refuse(otherWorkspace, tokA)
	if got := r.eventsOf(idA); len(got) != 3 || got[2]["suppressed"] != 0 {
		t.Errorf("events = %+v, want a third with suppressed 0", got)
	}
}

func TestRefusalLimiter_LimitsEachTokenOnItsOwn(t *testing.T) {
	r := newRefusalHub(t)
	tokA, idA := r.pat(r.alice, "w1", "git:read")
	tokB, idB := r.pat(r.alice, "w1", "git:read")

	for _, tok := range []string{tokA, tokB, tokA, tokB, tokB} {
		r.refuse(otherWorkspace, tok)
		r.clock.advance(time.Second)
	}
	var order []string
	for _, e := range r.events.all() {
		order = append(order, e.event.ResourceID)
	}
	if !reflect.DeepEqual(order, []string{idA, idB}) || r.eventsOf(idB)[0]["suppressed"] != 0 {
		t.Fatalf("events of %v, want one of A then one of B with suppressed 0", order)
	}
	r.clock.advance(56 * time.Second) // t0+61s
	r.refuse(identityRoute, tokA)
	r.clock.advance(time.Second)
	r.refuse(identityRoute, tokB)
	if a, b := r.eventsOf(idA), r.eventsOf(idB); len(a) != 2 || len(b) != 2 || a[1]["suppressed"] != 1 || b[1]["suppressed"] != 2 {
		t.Errorf("events of A %+v, of B %+v; want second events with suppressed 1 and 2", a, b)
	}
}

func TestRefusalLimiter_LeavesTheAnswerAsItIsWhenTheEmitFails(t *testing.T) {
	r := newRefusalHub(t)
	ref, _ := r.pat(r.alice, "w1", "git:read")
	ref2, _ := r.pat(r.alice, "w1", "git:read")
	want := []answer{r.refuse(otherWorkspace, ref), r.refuse(identityRoute, ref2)}

	for _, tc := range []struct {
		name   string
		events *recordingEmitter
	}{
		{"an error", &recordingEmitter{err: errors.New("log full")}},
		{"a panic", &recordingEmitter{panics: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			r.limit(tc.events, r.clock, slog.New(slog.NewTextHandler(&log, nil)))
			tokA, _ := r.pat(r.alice, "w1", "git:read")
			got := []answer{r.do(http.MethodGet, otherWorkspace, tokA)}
			r.clock.advance(time.Second)
			got = append(got, r.do(http.MethodGet, identityRoute, tokA))
			for i := range want {
				if got[i].status != want[i].status || !bytes.Equal(got[i].body, want[i].body) {
					t.Errorf("answer %d = %d %s, want %d %s as with a working emitter", i, got[i].status, got[i].body, want[i].status, want[i].body)
				}
			}
			if n := len(tc.events.all()); n != 1 {
				t.Errorf("the emitter was called %d times, want once", n)
			}
			lines := strings.Split(strings.TrimSpace(log.String()), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || strings.Contains(log.String(), tokA) {
				t.Errorf("log = %q, want one warning without the token", log.String())
			}
		})
	}
}

func TestRefusalLimiter_ReportsNothingForARequestTheGuardDoesNotRefuse(t *testing.T) {
	r := newRefusalHub(t)
	tokA, idA := r.pat(r.alice, "w1", "git:read")
	noScope, _ := r.pat(r.alice, "w1", "changes:read")
	carol, _ := r.user("carol")
	lostAccess, _ := r.pat(carol, "w1", "git:read")
	wrong := tokA[:len(tokA)-1] + map[bool]string{true: "b", false: "a"}[strings.HasSuffix(tokA, "a")]
	requests := []struct {
		path, credential string
		status           int
	}{
		{"/workspaces/w1/probe", tokA, http.StatusOK},
		{"/workspaces", tokA, http.StatusOK},
		{"/workspaces/w1/probe", noScope, http.StatusForbidden},
		{otherWorkspace, wrong, http.StatusUnauthorized},
		{"/workspaces/w1/probe", lostAccess, http.StatusNotFound},
	}
	send := func() {
		for _, q := range requests {
			if a := r.do(http.MethodGet, q.path, q.credential); a.status != q.status {
				t.Errorf("GET %s = %d %s, want %d", q.path, a.status, a.body, q.status)
			}
		}
	}

	send()
	if got := r.events.all(); len(got) != 0 {
		t.Fatalf("events = %+v, want none", got)
	}
	r.refuse(identityRoute, tokA)
	send()
	r.clock.advance(time.Minute)
	r.refuse(identityRoute, tokA)
	if got := r.eventsOf(idA); len(got) != 2 || got[0]["suppressed"] != 0 || got[1]["suppressed"] != 0 {
		t.Errorf("events = %+v, want two with suppressed 0", got)
	}
}

func TestRefusalLimiter_CountsConcurrentRefusalsOfATokenOnce(t *testing.T) {
	events, clock := &recordingEmitter{}, newTestClock()
	l := access.NewRefusalLimiter(events, clock.now, slog.New(slog.DiscardHandler))
	f := access.Refusal{TokenID: "t1", UserID: "u1", BoundWorkspace: "w1", Method: http.MethodGet,
		Route: "/api/v1/user", Status: http.StatusForbidden, Surface: access.SurfaceAPI}

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { l.Refused(context.Background(), f) })
	}
	wg.Wait()
	clock.advance(time.Minute)
	l.Refused(context.Background(), f)
	if got := events.all(); len(got) != 2 || got[1].event.Metadata["suppressed"] != 49 {
		t.Errorf("events = %+v, want two, the second with suppressed 49", got)
	}
}
