package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

const jsonContentType = "application/json; charset=utf-8"

type response struct {
	status      int
	contentType string
	body        []byte
}

// get sends a GET request for path through h and returns what h answered.
func get(t *testing.T, h http.Handler, path string, header http.Header) response {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body of GET %s: %v", path, err)
	}
	return response{status: res.StatusCode, contentType: res.Header.Get("Content-Type"), body: body}
}

// decodeObject parses body as one JSON object.
func decodeObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body %q is not a JSON object: %v", body, err)
	}
	return doc
}

// credentialVariants are the request headers every unauthenticated route
// must answer the same way: none at all, and a credential that means nothing.
var credentialVariants = []struct {
	name   string
	header http.Header
}{
	{name: "no authorization header", header: nil},
	{name: "nonsense bearer token", header: http.Header{"Authorization": {"Bearer nonsense"}}},
}

func TestHealthz_AnswersOKWithOrWithoutCredential(t *testing.T) {
	t.Parallel()
	h := New(Options{}).Handler()
	for _, cv := range credentialVariants {
		t.Run(cv.name, func(t *testing.T) {
			t.Parallel()
			r := get(t, h, "/healthz", cv.header)

			if r.status != http.StatusOK {
				t.Errorf("status = %d, want %d", r.status, http.StatusOK)
			}
			if r.contentType != jsonContentType {
				t.Errorf("Content-Type = %q, want %q", r.contentType, jsonContentType)
			}
			if got, want := decodeObject(t, r.body), map[string]any{"status": "ok"}; !reflect.DeepEqual(got, want) {
				t.Errorf("body = %v, want %v", got, want)
			}
		})
	}
}

// countingProbe returns a probe that answers err and counts its calls.
func countingProbe(err error) (Probe, *atomic.Int64) {
	var calls atomic.Int64
	return func(context.Context) error {
		calls.Add(1)
		return err
	}, &calls
}

func TestReadyz_ReportsReadinessFromEveryProbe(t *testing.T) {
	t.Parallel()
	errDiskGone := errors.New("disk gone")
	tests := []struct {
		name       string
		probeErrs  []error
		wantStatus int
		wantBody   map[string]any
	}{
		{name: "no probe registered", probeErrs: nil, wantStatus: http.StatusOK, wantBody: map[string]any{"status": "ready"}},
		{name: "one probe that succeeds", probeErrs: []error{nil}, wantStatus: http.StatusOK, wantBody: map[string]any{"status": "ready"}},
		{name: "two probes that succeed", probeErrs: []error{nil, nil}, wantStatus: http.StatusOK, wantBody: map[string]any{"status": "ready"}},
		{name: "one probe that fails", probeErrs: []error{errDiskGone}, wantStatus: http.StatusServiceUnavailable, wantBody: map[string]any{"status": "not ready"}},
		{name: "a succeeding probe then a failing one", probeErrs: []error{nil, errDiskGone}, wantStatus: http.StatusServiceUnavailable, wantBody: map[string]any{"status": "not ready"}},
		{name: "a failing probe then a succeeding one", probeErrs: []error{errDiskGone, nil}, wantStatus: http.StatusServiceUnavailable, wantBody: map[string]any{"status": "not ready"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var probes []Probe
			var counters []*atomic.Int64
			for _, err := range tt.probeErrs {
				p, calls := countingProbe(err)
				probes = append(probes, p)
				counters = append(counters, calls)
			}
			h := New(Options{ReadinessProbes: probes}).Handler()

			for i, cv := range credentialVariants {
				r := get(t, h, "/readyz", cv.header)

				if r.status != tt.wantStatus {
					t.Errorf("%s: status = %d, want %d", cv.name, r.status, tt.wantStatus)
				}
				if r.contentType != jsonContentType {
					t.Errorf("%s: Content-Type = %q, want %q", cv.name, r.contentType, jsonContentType)
				}
				if got := decodeObject(t, r.body); !reflect.DeepEqual(got, tt.wantBody) {
					t.Errorf("%s: body = %v, want %v", cv.name, got, tt.wantBody)
				}
				if strings.Contains(string(r.body), errDiskGone.Error()) {
					t.Errorf("%s: body %q carries the probe's error text", cv.name, r.body)
				}
				for j, calls := range counters {
					if got, want := calls.Load(), int64(i+1); got != want {
						t.Errorf("%s: probe %d called %d times after %d requests, want once per request", cv.name, j, got, want)
					}
				}
			}

			if r := get(t, h, "/healthz", nil); r.status != http.StatusOK {
				t.Errorf("GET /healthz status = %d, want %d whatever the probes answer", r.status, http.StatusOK)
			}
		})
	}
}

// The test sets the build values of internal/buildinfo, which are package
// globals, so it must not run in parallel.
func TestVersion_AnswersTheBuildValuesAndGoVersion(t *testing.T) {
	oldVersion, oldCommit, oldBuildTime := buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime = oldVersion, oldCommit, oldBuildTime
	})
	buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime = "1.4.0", "3f2a1bc", "2026-01-02T03:04:05Z"
	h := New(Options{}).Handler()

	for _, cv := range credentialVariants {
		t.Run(cv.name, func(t *testing.T) {
			r := get(t, h, "/version", cv.header)

			if r.status != http.StatusOK {
				t.Errorf("status = %d, want %d", r.status, http.StatusOK)
			}
			if r.contentType != jsonContentType {
				t.Errorf("Content-Type = %q, want %q", r.contentType, jsonContentType)
			}
			want := map[string]any{
				"version":    "1.4.0",
				"commit":     "3f2a1bc",
				"build_time": "2026-01-02T03:04:05Z",
				"go_version": runtime.Version(),
			}
			if got := decodeObject(t, r.body); !reflect.DeepEqual(got, want) {
				t.Errorf("body = %v, want %v", got, want)
			}
		})
	}
}

// serving is a server started by startServing on 127.0.0.1:0.
type serving struct {
	addr   string
	cancel context.CancelFunc
	// done receives what Serve returned, once.
	done chan error
}

// startServing registers routes on a new server with the given grace, runs
// Serve in a goroutine and waits until it has bound its listener.
func startServing(t *testing.T, grace time.Duration, routes map[string]http.Handler) *serving {
	t.Helper()
	bound := make(chan net.Addr, 1)
	s := New(Options{Addr: "127.0.0.1:0", ShutdownGrace: grace, OnListen: func(a net.Addr) { bound <- a }})
	for pattern, h := range routes {
		s.Handle(pattern, h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sv := &serving{cancel: cancel, done: make(chan error, 1)}
	go func() { sv.done <- s.Serve(ctx) }()
	t.Cleanup(cancel)

	select {
	case a := <-bound:
		sv.addr = a.String()
	case err := <-sv.done:
		t.Fatalf("Serve returned %v before it reported a bound address", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not report a bound address within 5 s")
	}
	return sv
}

type getResult struct {
	status int
	body   string
	err    error
}

// getAsync sends GET http://addr+path on a client of its own and delivers the
// outcome on the returned channel.
func getAsync(t *testing.T, addr, path string) <-chan getResult {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr}
	out := make(chan getResult, 1)
	go func() {
		res, err := client.Get("http://" + addr + path)
		if err != nil {
			out <- getResult{err: err}
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		out <- getResult{status: res.StatusCode, body: string(body), err: err}
	}()
	return out
}

// blockingHandler answers 200 "done" once release is closed, and closes
// started when it has been called.
func blockingHandler(started chan<- struct{}, release <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})
}

func waitFor[T any](t *testing.T, ch <-chan T, within time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(within):
		t.Fatalf("%s: nothing within %v", what, within)
		var zero T
		return zero
	}
}

func TestServe_StopsAcceptingAndLetsAnInFlightRequestFinish(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	sv := startServing(t, 2*time.Second, map[string]http.Handler{"GET /slow": blockingHandler(started, release)})

	slow := getAsync(t, sv.addr, "/slow")
	waitFor(t, started, 5*time.Second, "slow handler start")

	t0 := time.Now()
	sv.cancel()

	// The listener closes once the context is cancelled: a new connection is
	// refused while the slow request is still being answered.
	deadline := time.Now().Add(time.Second)
	for {
		conn, err := net.Dial("tcp", sv.addr)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("new connections still accepted 1 s after the stop")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-sv.done:
		t.Fatalf("Serve returned %v while a request was in flight", err)
	case r := <-slow:
		t.Fatalf("the slow request finished before its handler was released: %+v", r)
	default:
	}

	close(release)
	r := waitFor(t, slow, 2*time.Second, "slow response")
	if r.err != nil {
		t.Fatalf("slow request: %v", r.err)
	}
	if r.status != http.StatusOK || r.body != "done" {
		t.Errorf("slow response = %d %q, want 200 %q", r.status, r.body, "done")
	}
	if err := waitFor(t, sv.done, 2*time.Second, "Serve return"); err != nil {
		t.Errorf("Serve returned %v, want nil", err)
	}
	if elapsed := time.Since(t0); elapsed >= 2*time.Second {
		t.Errorf("Serve returned %v after the stop, want under 2 s", elapsed)
	}
}

func TestServe_ClosesAStuckRequestWhenTheGraceExpires(t *testing.T) {
	t.Parallel()
	const grace = 100 * time.Millisecond
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	sv := startServing(t, grace, map[string]http.Handler{"GET /stuck": blockingHandler(started, release)})

	stuck := getAsync(t, sv.addr, "/stuck")
	waitFor(t, started, 5*time.Second, "stuck handler start")

	t0 := time.Now()
	sv.cancel()
	err := waitFor(t, sv.done, 5*time.Second, "Serve return")
	elapsed := time.Since(t0)

	if err != nil {
		t.Errorf("Serve returned %v, want nil", err)
	}
	if elapsed < grace || elapsed >= grace+time.Second {
		t.Errorf("Serve returned %v after the stop, want between %v and %v", elapsed, grace, grace+time.Second)
	}
	r := waitFor(t, stuck, 5*time.Second, "stuck request outcome")
	if r.err == nil {
		t.Errorf("stuck request got a response %d %q, want its connection closed", r.status, r.body)
	}
}
