package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

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
