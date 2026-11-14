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

// refusalOf is a refusal of the token tokenID of u1, bound to w1, of
// method route with status, naming the workspace requested ("" for none).
func refusalOf(tokenID, method, route string, status int, surface, requested string) access.Refusal {
	f := access.Refusal{TokenID: tokenID, UserID: "u1", BoundWorkspace: "w1", Method: method, Route: route,
		Status: status, Surface: surface}
	if requested != "" {
		f.RequestedWorkspace = &requested
	}
	return f
}

func TestRefusalLimiter_ReportsATokensFirstRefusalAsOneEventNow(t *testing.T) {
	events := &recordingEmitter{}
	l := access.NewRefusalLimiter(events, newTestClock().now, nil)
	for _, c := range []struct {
		f    access.Refusal
		meta map[string]any // beyond the refusal's own
	}{
		{refusalOf("t1", http.MethodGet, "/api/v1"+probePattern, 404, access.SurfaceAPI, "NoSuch"), map[string]any{"requested_workspace": "NoSuch"}},
		{refusalOf("t2", http.MethodGet, "/api/v1/user", 403, access.SurfaceAPI, ""), map[string]any{}},
		{refusalOf("t3", http.MethodPost, "/git/:org/:slug.git/git-upload-pack", 404, access.SurfaceGit, "w5"), map[string]any{"requested_workspace": "w5"}},
	} {
		before := len(events.all())
		l.Refused(context.Background(), c.f)
		c.meta["token_id"], c.meta["bound_workspace"], c.meta["method"], c.meta["route"] = c.f.TokenID, "w1", c.f.Method, c.f.Route
		c.meta["status"], c.meta["surface"], c.meta["suppressed"] = c.f.Status, c.f.Surface, 0
		want := access.Event{Type: "parley.token.bound_refused", Workspace: "w1", ActorType: "pat", ActorID: "u1",
			ResourceType: "token", ResourceID: c.f.TokenID, Metadata: c.meta}
		if got := events.all()[before:]; len(got) != 1 || got[0].tx != nil || !reflect.DeepEqual(got[0].event, want) {
			t.Errorf("Refused(%+v) emitted %+v, want %+v emitted now", c.f, got, want)
		}
	}
	// A limiter built anew, as at the hub's start, knows no token.
	access.NewRefusalLimiter(events, newTestClock().now, nil).Refused(context.Background(), refusalOf("t2", http.MethodGet, "/api/v1/user", 403, access.SurfaceAPI, ""))
	if got := events.all(); len(got) != 4 || got[3].event.Metadata["suppressed"] != 0 {
		t.Errorf("events = %+v, want a fourth with suppressed 0", got)
	}
}

func TestRefusalLimiter_ReportsATokenAtMostOnceAMinuteAndCarriesTheSuppressedCount(t *testing.T) {
	events, clock := &recordingEmitter{}, newTestClock()
	l := access.NewRefusalLimiter(events, clock.now, nil)
	start := clock.now()
	// Each step refuses a token at a second after the start.
	for _, step := range []struct {
		at    int
		token string
	}{{0, "A"}, {1, "A"}, {1, "B"}, {2, "B"}, {30, "A"}, {59, "A"}, {60, "A"}, {61, "A"}, {62, "B"}, {180, "A"}, {240, "A"}} {
		clock.advance(start.Add(time.Duration(step.at) * time.Second).Sub(clock.now()))
		l.Refused(context.Background(), refusalOf(step.token, http.MethodGet, "/api/v1/user", 403, access.SurfaceAPI, ""))
	}
	var got []string
	for _, e := range events.all() {
		got = append(got, e.event.ResourceID+" "+strings.Repeat("+", e.event.Metadata["suppressed"].(int)))
	}
	// A at 0, B at 1, A at 60 with the 3 refusals since, B at 62 with its 1,
	// A at 180 with the 1 at 61, A at 240 with none.
	if want := []string{"A ", "B ", "A +++", "B +", "A +", "A "}; !reflect.DeepEqual(got, want) {
		t.Errorf("events (token, + per suppressed refusal) = %q, want %q", got, want)
	}
}

func TestRefusalLimiter_CountsConcurrentRefusalsOfATokenOnce(t *testing.T) {
	events, clock := &recordingEmitter{}, newTestClock()
	l := access.NewRefusalLimiter(events, clock.now, nil)
	f := refusalOf("t1", http.MethodGet, "/api/v1/user", 403, access.SurfaceAPI, "")
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

func TestRefusalLimiter_LeavesTheAnswerAsItIsWhenTheEmitFails(t *testing.T) {
	for _, c := range []struct {
		name   string
		events *recordingEmitter
	}{
		{"an error", &recordingEmitter{err: errors.New("log full")}},
		{"a panic", &recordingEmitter{panics: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newGuardWorld(t)
			tok := w.tok["w1"].value
			paths := []string{"/workspaces/w5/probe", "/user"}
			var want []answer
			for _, path := range paths {
				want = append(want, w.do(http.MethodGet, path, tok))
			}
			var log bytes.Buffer
			clock := newTestClock()
			w.limit(c.events, clock, slog.New(slog.NewTextHandler(&log, nil)))
			for i, path := range paths {
				if got := w.do(http.MethodGet, path, tok); got.status != want[i].status || !bytes.Equal(got.body, want[i].body) {
					t.Errorf("GET %s = %d %s, want %d %s as without the limiter", path, got.status, got.body, want[i].status, want[i].body)
				}
				clock.advance(time.Second)
			}
			if n := len(c.events.all()); n != 1 {
				t.Errorf("the emitter was called %d times, want once", n)
			}
			lines := strings.Split(strings.TrimSpace(log.String()), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || strings.Contains(log.String(), tok) {
				t.Errorf("log = %q, want one warning without the token", log.String())
			}
		})
	}
}
