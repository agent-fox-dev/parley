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
	status int
	header http.Header
	body   string
}

func get(t *testing.T, h http.Handler, path string, header http.Header) response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return response{status: rec.Code, header: rec.Header(), body: string(body)}
}

func decodeObject(t *testing.T, body string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("body %q is not a JSON object: %v", body, err)
	}
	return doc
}

var authHeaders = []struct {
	name   string
	header http.Header
}{
	{"no credential", nil},
	{"nonsense bearer", http.Header{"Authorization": {"Bearer nonsense"}}},
}

func TestHealthz_AnswersOKWithOrWithoutCredential(t *testing.T) {
	h := New(Options{}).Handler()
	for _, tt := range authHeaders {
		t.Run(tt.name, func(t *testing.T) {
			r := get(t, h, "/healthz", tt.header)
			if r.status != http.StatusOK {
				t.Fatalf("status = %d, want 200", r.status)
			}
			if ct := r.header.Get("Content-Type"); ct != jsonContentType {
				t.Errorf("Content-Type = %q, want %q", ct, jsonContentType)
			}
			if doc := decodeObject(t, r.body); !reflect.DeepEqual(doc, map[string]any{"status": "ok"}) {
				t.Errorf("body = %v, want {status: ok}", doc)
			}
		})
	}
}

type countingProbe struct {
	calls atomic.Int32
	err   error
}

func (p *countingProbe) probe(context.Context) error {
	p.calls.Add(1)
	return p.err
}

func TestReadyz_ReadyWhenEveryProbeSucceeds(t *testing.T) {
	tests := []struct {
		name   string
		probes int
	}{
		{"no probe", 0},
		{"one probe returning nil", 1},
		{"two probes returning nil", 2},
	}
	for _, tt := range tests {
		for _, auth := range authHeaders {
			t.Run(tt.name+"/"+auth.name, func(t *testing.T) {
				var counters []*countingProbe
				var probes []Probe
				for range tt.probes {
					c := &countingProbe{}
					counters = append(counters, c)
					probes = append(probes, c.probe)
				}
				h := New(Options{ReadinessProbes: probes}).Handler()
				r := get(t, h, "/readyz", auth.header)
				if r.status != http.StatusOK {
					t.Fatalf("status = %d, want 200", r.status)
				}
				if ct := r.header.Get("Content-Type"); ct != jsonContentType {
					t.Errorf("Content-Type = %q, want %q", ct, jsonContentType)
				}
				if doc := decodeObject(t, r.body); !reflect.DeepEqual(doc, map[string]any{"status": "ready"}) {
					t.Errorf("body = %v, want {status: ready}", doc)
				}
				for i, c := range counters {
					if n := c.calls.Load(); n != 1 {
						t.Errorf("probe %d called %d times, want 1", i, n)
					}
				}
			})
		}
	}
}

func TestReadyz_NotReadyWhenAnyProbeFails(t *testing.T) {
	failing := func(context.Context) error { return errors.New("disk gone") }
	ok := func(context.Context) error { return nil }
	tests := []struct {
		name   string
		probes []Probe
	}{
		{"one failing", []Probe{failing}},
		{"nil then failing", []Probe{ok, failing}},
		{"failing then nil", []Probe{failing, ok}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New(Options{ReadinessProbes: tt.probes}).Handler()
			r := get(t, h, "/readyz", nil)
			if r.status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", r.status)
			}
			if ct := r.header.Get("Content-Type"); ct != jsonContentType {
				t.Errorf("Content-Type = %q, want %q", ct, jsonContentType)
			}
			if doc := decodeObject(t, r.body); !reflect.DeepEqual(doc, map[string]any{"status": "not ready"}) {
				t.Errorf("body = %v, want exactly {status: not ready}", doc)
			}
			if strings.Contains(r.body, "disk gone") {
				t.Errorf("body %q carries the probe's error text", r.body)
			}
			if hz := get(t, h, "/healthz", nil); hz.status != http.StatusOK {
				t.Errorf("/healthz status = %d while not ready, want 200", hz.status)
			}
		})
	}
}

func TestVersion_ReportsBuildValuesAndToolchain(t *testing.T) {
	oldV, oldC, oldB := buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime = oldV, oldC, oldB })
	buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime = "1.4.0", "3f2a1bc", "2026-01-02T03:04:05Z"

	h := New(Options{}).Handler()
	for _, auth := range authHeaders {
		t.Run(auth.name, func(t *testing.T) {
			r := get(t, h, "/version", auth.header)
			if r.status != http.StatusOK {
				t.Fatalf("status = %d, want 200", r.status)
			}
			if ct := r.header.Get("Content-Type"); ct != jsonContentType {
				t.Errorf("Content-Type = %q, want %q", ct, jsonContentType)
			}
			want := map[string]any{
				"version":    "1.4.0",
				"commit":     "3f2a1bc",
				"build_time": "2026-01-02T03:04:05Z",
				"go_version": runtime.Version(),
			}
			if doc := decodeObject(t, r.body); !reflect.DeepEqual(doc, want) {
				t.Errorf("body = %v, want %v", doc, want)
			}
		})
	}
}
