package metrics_test

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/metrics"
)

// testClock is a clock a handler can move on.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestInstrument_CountsEveryRequestByMethodPatternAndStatus(t *testing.T) {
	t.Parallel()
	clock := &testClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	answer := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			clock.Advance(30 * time.Millisecond)
			w.WriteHeader(status)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/workspaces/{slug}/sync", answer(http.StatusOK))
	mux.Handle("GET /git/{org}/{repo}/info/refs", answer(http.StatusUnauthorized))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		clock.Advance(30 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	})
	reg := metrics.New(nil)
	h := metrics.Instrument(reg, clock.Now, nil, mux)

	for _, req := range []struct {
		method, target string
		status         int
	}{
		{http.MethodGet, "/api/v1/workspaces/a/sync", http.StatusOK},
		{http.MethodGet, "/api/v1/workspaces/b/sync", http.StatusOK},
		{http.MethodGet, "/git/o/r.git/info/refs", http.StatusUnauthorized},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/no/such/path", http.StatusNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{"get", "/healthz", http.StatusMethodNotAllowed},
		{"FOO1", "/healthz", http.StatusMethodNotAllowed},
		{"foo2", "/healthz", http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(req.method, req.target, nil))
		if rec.Code != req.status {
			t.Fatalf("%s %s: status = %d, want %d", req.method, req.target, rec.Code, req.status)
		}
	}

	families := reg.Gather(context.Background())
	counts := samples(family(t, families, metrics.HTTPRequestsTotal))
	for key, want := range map[string]float64{
		"method=GET,path=/api/v1/workspaces/{slug}/sync,status=200": 2,
		"method=GET,path=/git/{org}/{repo}/info/refs,status=401":    1,
		"method=GET,path=/healthz,status=200":                       1,
		"method=GET,path=unmatched,status=404":                      1,
		"method=POST,path=unmatched,status=405":                     1,
		"method=GET,path=unmatched,status=405":                      1,
		"method=OTHER,path=unmatched,status=405":                    2,
	} {
		if counts[key] != want {
			t.Errorf("requests{%s} = %v, want %v (all: %v)", key, counts[key], want, counts)
		}
	}
	if len(counts) != 7 {
		t.Errorf("requests has %d series, want 7: %v", len(counts), counts)
	}
	for key := range counts {
		if strings.Contains(key, "/a/") || strings.Contains(key, "no/such") {
			t.Errorf("series %s carries a concrete path", key)
		}
		if strings.Contains(strings.ToUpper(key), "FOO") {
			t.Errorf("series %s carries a method outside the standard set", key)
		}
	}
	durations := samples(family(t, families, metrics.HTTPRequestDurationSeconds))
	route := "method=GET,path=/api/v1/workspaces/{slug}/sync"
	for suffix, want := range map[string]float64{"|count": 2, "|le=0.05": 2, "|le=0.025": 0} {
		if got := durations[route+suffix]; got != want {
			t.Errorf("duration{%s}%s = %v, want %v", route, suffix, got, want)
		}
	}
	if got := durations["method=OTHER,path=unmatched|count"]; got != 2 {
		t.Errorf("duration{method=OTHER,path=unmatched}|count = %v, want 2", got)
	}
}

// hijackable is a response writer whose connection can be taken over.
type hijackable struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackable) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func TestInstrument_KeepsStreamingAvailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		use    func(*http.ResponseController) error
		worked func(*hijackable) bool
		// counted is the series the request is counted on: a handler that
		// takes the connection over switches protocols and writes no status.
		counted string
	}{
		{"flush", (*http.ResponseController).Flush, func(w *hijackable) bool { return w.Flushed }, "method=GET,path=unmatched,status=200"},
		{"hijack", func(rc *http.ResponseController) error {
			_, _, err := rc.Hijack()
			return err
		}, func(w *hijackable) bool { return w.hijacked }, "method=GET,path=unmatched,status=101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			reg := metrics.New(nil)
			h := metrics.Instrument(reg, time.Now, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				err = tc.use(http.NewResponseController(w))
			}))
			w := &hijackable{ResponseRecorder: httptest.NewRecorder()}
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/events", nil))
			if err != nil || !tc.worked(w) {
				t.Errorf("%s through the instrumentation: err = %v, reached the connection's writer = %v", tc.name, err, tc.worked(w))
			}
			counts := samples(family(t, reg.Gather(context.Background()), metrics.HTTPRequestsTotal))
			if counts[tc.counted] != 1 || len(counts) != 1 {
				t.Errorf("%s: requests = %v, want 1 on %s", tc.name, counts, tc.counted)
			}
		})
	}
}
