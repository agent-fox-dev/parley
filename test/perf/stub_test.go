package perf_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/test/perf"
)

// stubToken is the credential the harness sends to the stub; a test looks
// for it in everything the harness writes.
const stubToken = "parley_perf_stub_token_5f0c2a9e"

// stubVersion is the document the stub answers GET /version with.
const stubVersion = `{"version":"1.4.0","commit":"3f2a1bc","build_time":"2026-11-20T00:00:00Z"}`

// scenarioNames are the fourteen scenarios in the order of the report.
var scenarioNames = []string{
	"fork_create", "file_read", "commit_create", "land", "land_throughput", "land_concurrency", "mergeability",
	"event_latency", "active_forks", "open_changes", "event_streams", "sandboxes_running", "sandbox_ready", "sandbox_exec",
}

// stub stands in for a hub: it answers GET /version and the routes the
// scenarios call, each with a success unless the test overrides it, and
// records what it was sent.
type stub struct {
	*httptest.Server
	mu sync.Mutex
	// seen is "METHOD path" of every request, in the order they came.
	seen []string
	// forks are the forks the stub created and nobody deleted yet.
	forks    map[string]bool
	nextFork int
}

// newStub serves a stub until the test ends. override replaces the
// default handler of a route, keyed by its http.ServeMux pattern.
func newStub(t *testing.T, override map[string]http.HandlerFunc) *stub {
	t.Helper()
	s := &stub{forks: map[string]bool{}}
	routes := map[string]http.HandlerFunc{
		"GET /version": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, stubVersion)
		},
		"POST /api/v1/workspaces/{slug}/forks": s.createFork,
		"DELETE /api/v1/workspaces/{slug}":     s.deleteWorkspace,
		"GET /api/v1/workspaces/{slug}/scm/tree": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{
				{"path": "src", "name": "src", "type": "tree", "mode": "040000", "sha": strings.Repeat("a", 40)},
				{"path": "README.md", "name": "README.md", "type": "blob", "mode": "100644", "sha": strings.Repeat("b", 40), "size": 6},
			}, "next_cursor": nil})
		},
		"GET /api/v1/workspaces/{slug}/scm/file": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "hello\n")
		},
	}
	for pattern, h := range override {
		if _, ok := routes[pattern]; !ok {
			t.Fatalf("the stub has no route %q to override", pattern)
		}
		routes[pattern] = h
	}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stub) createFork(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.nextFork++
	slug := fmt.Sprintf("%s-f%d", r.PathValue("slug"), s.nextFork)
	s.forks[slug] = true
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"workspace": map[string]any{"slug": slug}, "remote": nil})
}

func (s *stub) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.forks[r.PathValue("slug")] {
		writeError(w, http.StatusNotFound, "not_found", "no such workspace")
		return
	}
	delete(s.forks, r.PathValue("slug"))
	w.WriteHeader(http.StatusNoContent)
}

// requests returns "METHOD path" of every request the stub was sent.
func (s *stub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// config is a configuration for a run against s, with the report in a
// directory of the test's own.
func (s *stub) config(t *testing.T) perf.Config {
	return perf.Config{URL: s.URL, Token: stubToken, Workspaces: []string{"w"},
		ReportPath: filepath.Join(t.TempDir(), "report.json")}
}

// failWith answers every request with status and the error envelope.
func failWith(status int, errorType, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeError(w, status, errorType, message) }
}

// hang never answers: it holds the request until the client gives up.
func hang(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": message, "error_type": errorType}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// result is the result of the scenario name in rep.
func result(t *testing.T, rep perf.Report, name string) perf.Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("the report has no result %q: %+v", name, rep.Results)
	return perf.Result{}
}
