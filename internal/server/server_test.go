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

// startServer serves srv on 127.0.0.1:0 until the returned cancel is called
// and returns the bound address and a channel that receives Serve's result.
func startServer(t *testing.T, srv *Server) (addr string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	bound := make(chan string, 1)
	result := make(chan error, 1)
	go func() { result <- srv.Serve(ctx, func(a net.Addr) { bound <- a.String() }) }()
	select {
	case addr = <-bound:
	case err := <-result:
		cancel()
		t.Fatalf("Serve returned before listening: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not report its address within 5 s")
	}
	t.Cleanup(cancel)
	return addr, cancel, result
}

func TestServe_ReportsTheBoundPortAndServes(t *testing.T) {
	addr, cancel, done := startServer(t, New(Options{Addr: "127.0.0.1:0", ShutdownGrace: time.Second}))
	if _, port, _ := net.SplitHostPort(addr); port == "0" {
		t.Fatalf("bound address %q still carries port 0", addr)
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v after cancel, want nil", err)
	}
}

func TestServe_ReturnsTheBindError(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	err = New(Options{Addr: held.Addr().String()}).Serve(context.Background(), func(net.Addr) {
		t.Error("onListen called for an address that cannot be bound")
	})
	if err == nil {
		t.Fatal("Serve returned nil for an address in use")
	}
}

func TestServe_LetsAnInFlightRequestFinishWithinTheGrace(t *testing.T) {
	srv := New(Options{Addr: "127.0.0.1:0", ShutdownGrace: 2 * time.Second})
	started := make(chan struct{})
	srv.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "done")
	})
	addr, cancel, done := startServer(t, srv)

	type result struct {
		status int
		body   string
		err    error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		got <- result{status: resp.StatusCode, body: string(b), err: err}
	}()
	<-started
	t0 := time.Now()
	cancel()

	refused := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			refused = true
			break
		}
		c.Close()
	}
	if !refused {
		t.Error("new connections were still accepted after the stop began")
	}

	r := <-got
	if r.err != nil || r.status != http.StatusOK || r.body != "done" {
		t.Fatalf("slow request = (%d, %q, %v), want (200, done, nil)", r.status, r.body, r.err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within the grace")
	}
	if elapsed := time.Since(t0); elapsed >= 2*time.Second {
		t.Errorf("Serve took %v after the stop, want under 2 s", elapsed)
	}
}

func TestServe_ClosesARequestStillRunningWhenTheGracePasses(t *testing.T) {
	srv := New(Options{Addr: "127.0.0.1:0", ShutdownGrace: 100 * time.Millisecond})
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv.HandleFunc("GET /stuck", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	})
	addr, cancel, done := startServer(t, srv)

	clientErr := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/stuck")
		if err == nil {
			resp.Body.Close()
		}
		clientErr <- err
	}()
	<-started
	t0 := time.Now()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
	elapsed := time.Since(t0)
	if elapsed < 100*time.Millisecond || elapsed >= 1100*time.Millisecond {
		t.Errorf("Serve returned %v after the stop, want between 100 ms and 1.1 s", elapsed)
	}
	select {
	case err := <-clientErr:
		if err == nil {
			t.Error("the stuck request got a response, want a closed connection")
		}
	case <-time.After(time.Second):
		t.Error("the stuck request's connection was not closed")
	}
}

func TestServe_AConnectionWithoutARequestDoesNotHoldTheStop(t *testing.T) {
	addr, cancel, done := startServer(t, New(Options{Addr: "127.0.0.1:0", ShutdownGrace: 5 * time.Second}))
	idle, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	time.Sleep(20 * time.Millisecond) // let the server accept it

	t0 := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Serve did not return")
	}
	if elapsed := time.Since(t0); elapsed >= time.Second {
		t.Errorf("Serve took %v to stop with no request in flight, want under 1 s", elapsed)
	}
}
