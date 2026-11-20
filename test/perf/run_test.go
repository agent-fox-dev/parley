package perf_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/test/perf"
)

// reportDoc is the report file as JSON, read without the harness's types.
type reportDoc struct {
	Hub struct {
		URL     string         `json:"url"`
		Version map[string]any `json:"version"`
	} `json:"hub"`
	RunID      string `json:"run_id"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Results    []struct {
		Name   string `json:"name"`
		Target struct {
			Comparison string   `json:"comparison"`
			Value      *float64 `json:"value"`
			Unit       string   `json:"unit"`
		} `json:"target"`
		Measured *float64 `json:"measured"`
		Samples  *int     `json:"samples"`
		Status   string   `json:"status"`
		Detail   *string  `json:"detail"`
	} `json:"results"`
	CleanupErrors []string `json:"cleanup_errors"`
}

// readReport reads the report at path and checks that it holds no token.
func readReport(t *testing.T, path, token string) reportDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the report: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Errorf("the report holds the token: %s", raw)
	}
	var doc reportDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, raw)
	}
	var members map[string]json.RawMessage
	_ = json.Unmarshal(raw, &members)
	for _, name := range []string{"hub", "run_id", "started_at", "finished_at", "results", "cleanup_errors"} {
		if _, ok := members[name]; !ok {
			t.Errorf("the report has no member %q: %s", name, raw)
		}
	}
	if string(members["cleanup_errors"]) == "null" {
		t.Errorf("cleanup_errors is null, want a list")
	}
	return doc
}

func TestRun_FailsWithoutRunningAScenarioWhenTheHubDoesNotAnswerVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// arrange returns the hub's URL and, when the test can see them,
		// the requests it was sent.
		arrange func(t *testing.T) (url string, requests func() []string)
	}{
		{"answered 503", func(t *testing.T) (string, func() []string) {
			s := newStub(t, map[string]http.HandlerFunc{"GET /version": failWith(503, "unavailable", "starting")})
			return s.URL, s.requests
		}},
		{"not answered", func(t *testing.T) (string, func() []string) {
			closed := httptest.NewServer(http.NotFoundHandler())
			closed.Close()
			return closed.URL, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			url, requests := tc.arrange(t)
			cfg := perf.Config{URL: url, Token: stubToken, Workspaces: []string{"w"},
				ReportPath: filepath.Join(t.TempDir(), "report.json")}
			_, err := perf.Run(t.Context(), cfg)
			if err == nil || !strings.Contains(err.Error(), url) || strings.Contains(err.Error(), stubToken) {
				t.Errorf("err = %v, want one naming %s without the token", err, url)
			}
			if requests != nil {
				if got := requests(); !slices.Equal(got, []string{"GET /version"}) {
					t.Errorf("the hub was sent %q, want GET /version alone", got)
				}
			}
		})
	}
}

func TestRun_FailsNamingAReportPathItCannotWrite(t *testing.T) {
	t.Parallel()
	s := newStub(t, nil)
	cfg := s.config(t)
	cfg.ReportPath = filepath.Join(t.TempDir(), "missing", "report.json")
	rep, err := perf.Run(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), cfg.ReportPath) || strings.Contains(err.Error(), stubToken) {
		t.Errorf("err = %v, want one naming %s without the token", err, cfg.ReportPath)
	}
	if len(rep.Results) != len(scenarioNames) {
		t.Errorf("the run has %d results, want %d", len(rep.Results), len(scenarioNames))
	}
}

func TestRun_SkipsTheScenariosWhoseVariableIsUnset(t *testing.T) {
	t.Parallel()
	s := newStub(t, nil)
	rep, err := perf.Run(t.Context(), s.config(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, variable string }{
		{"active_forks", "PARLEY_PERF_CAPACITY"},
		{"open_changes", "PARLEY_PERF_CAPACITY"},
		{"event_streams", "PARLEY_PERF_CAPACITY"},
		{"sandboxes_running", "PARLEY_PERF_CAPACITY"},
		{"sandbox_ready", "PARLEY_PERF_SANDBOX_PROFILE"},
		{"sandbox_exec", "PARLEY_PERF_SANDBOX_PROFILE"},
	}
	for _, tt := range tests {
		r := result(t, rep, tt.name)
		if r.Status != perf.Skipped || !strings.Contains(r.Detail, tt.variable) || r.Measured != nil || r.Samples != 0 {
			t.Errorf("%s = %+v, want skipped naming %s with no measurement and no sample", tt.name, r, tt.variable)
		}
	}
}

func TestRun_EndsAScenarioTheHubFailsInErrorAndGoesOn(t *testing.T) {
	t.Parallel()
	const exec = "POST /api/v1/workspaces/{slug}/sandboxes/{id}/exec"
	execAnswers := func(answer map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, answer) }
	}
	for _, tc := range []struct {
		name, scenario, route string
		handler               http.HandlerFunc
		// parts are what the scenario's detail holds.
		parts []string
	}{
		{"a request answered 500", "fork_create", "POST /api/v1/workspaces/{slug}/forks",
			failWith(500, "internal", "internal error"), []string{"POST", "/workspaces/w/forks", "500", "internal"}},
		{"true exits with 3", "sandbox_exec", exec,
			execAnswers(map[string]any{"exit_code": 3, "stdout": "", "stderr": ""}), []string{"exit code 3"}},
		{"an answer with no exit code", "sandbox_exec", exec,
			execAnswers(map[string]any{"stdout": "", "stderr": ""}), []string{"exit code none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t, map[string]http.HandlerFunc{tc.route: tc.handler})
			cfg := s.config(t)
			cfg.SandboxProfile = "agent"
			rep, err := perf.Run(t.Context(), cfg)
			if err != nil {
				t.Fatalf("Run = %v, want no error", err)
			}
			r := result(t, rep, tc.scenario)
			if r.Status != perf.Error || r.Measured != nil {
				t.Errorf("%s = %+v, want error with no measurement", tc.scenario, r)
			}
			for _, part := range tc.parts {
				if !strings.Contains(r.Detail, part) {
					t.Errorf("%s's detail %q does not hold %q", tc.scenario, r.Detail, part)
				}
			}
			if len(rep.Results) != len(scenarioNames) || result(t, rep, "file_read").Status == "" {
				t.Errorf("results = %+v, want all %d, file_read's with a status", rep.Results, len(scenarioNames))
			}
		})
	}
}

func TestRun_EndsAScenarioAtItsDeadline(t *testing.T) {
	t.Parallel()
	s := newStub(t, map[string]http.HandlerFunc{"GET /api/v1/workspaces/{slug}/scm/file": hang})
	cfg := s.config(t)
	cfg.ScenarioDeadline = 100 * time.Millisecond
	start := time.Now()
	rep, err := perf.Run(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Run = %v, want no error", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the run took %v, want under 5s", took)
	}
	if r := result(t, rep, "file_read"); r.Status != perf.Error || !strings.Contains(r.Detail, "deadline exceeded") || r.Measured != nil {
		t.Errorf("file_read = %+v, want error saying deadline exceeded", r)
	}
	if len(rep.Results) != len(scenarioNames) {
		t.Errorf("the run has %d results, want %d", len(rep.Results), len(scenarioNames))
	}
}

func TestRun_WritesTheReport(t *testing.T) {
	t.Parallel()
	// The token comes first, where cutting the text to 200 bytes keeps it.
	long := stubToken + strings.Repeat("x", 5000-len(stubToken))
	s := newStub(t, map[string]http.HandlerFunc{
		"GET /api/v1/workspaces/{slug}/scm/file": failWith(500, "internal", long),
	})
	cfg := s.config(t)
	before := time.Now()
	rep, err := perf.Run(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Run = %v, want no error", err)
	}
	doc := readReport(t, cfg.ReportPath, stubToken)
	var names []string
	for _, r := range doc.Results {
		names = append(names, r.Name)
		if !slices.Contains([]string{"lt", "le", "ge"}, r.Target.Comparison) || r.Target.Value == nil || r.Target.Unit == "" {
			t.Errorf("%s has target %+v, want a comparison, a value and a unit", r.Name, r.Target)
		}
		if !slices.Contains([]string{"met", "missed", "skipped", "error"}, r.Status) {
			t.Errorf("%s has status %q", r.Name, r.Status)
		}
		if r.Detail == nil || len(*r.Detail) > 200 || r.Samples == nil {
			t.Errorf("%s has detail %v and samples %v, want a detail of at most 200 bytes and a count", r.Name, r.Detail, r.Samples)
		}
	}
	if !slices.Equal(names, scenarioNames) {
		t.Errorf("results = %q, want %q", names, scenarioNames)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(stubVersion), &want)
	if doc.Hub.URL != s.URL || !reflect.DeepEqual(doc.Hub.Version, want) {
		t.Errorf("hub = %+v, want %s with %s", doc.Hub, s.URL, stubVersion)
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(doc.RunID) {
		t.Errorf("run_id = %q, want 12 hexadecimal characters", doc.RunID)
	}
	started, err1 := time.Parse(time.RFC3339Nano, doc.StartedAt)
	finished, err2 := time.Parse(time.RFC3339Nano, doc.FinishedAt)
	if err1 != nil || err2 != nil || started.After(finished) || started.Before(before.Add(-time.Second)) {
		t.Errorf("started_at %q, finished_at %q, want two instants of the run in order", doc.StartedAt, doc.FinishedAt)
	}
	table := rep.Table()
	for _, name := range scenarioNames {
		if !strings.Contains(table, name) {
			t.Errorf("the table has no row for %s:\n%s", name, table)
		}
	}
	if strings.Contains(table, stubToken) {
		t.Errorf("the table holds the token:\n%s", table)
	}
}

func TestRun_MeasuresARealHubAndPasses(t *testing.T) {
	t.Parallel()
	h := hubtest.Start(t, nil)
	const token = "parley_perf_smoke_token"
	cfg := perf.Config{URL: h.URL(""), Token: token, Workspaces: []string{"perf-smoke"},
		ReportPath: filepath.Join(t.TempDir(), "r.json")}
	rep, err := perf.Run(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Run = %v, want no error", err)
	}
	doc := readReport(t, cfg.ReportPath, token)

	resp, err := http.Get(h.URL("/version"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var version map[string]any
	if err := json.Unmarshal(body, &version); err != nil || !reflect.DeepEqual(doc.Hub.Version, version) {
		t.Errorf("hub.version = %v, want GET /version's %s", doc.Hub.Version, body)
	}
	if len(doc.Results) != len(scenarioNames) {
		t.Fatalf("the report has %d results, want %d", len(doc.Results), len(scenarioNames))
	}
	// None is met: a scenario that measures sends the token first, and the
	// hub refuses it.
	for i, r := range doc.Results {
		if r.Name != scenarioNames[i] || !slices.Contains([]string{"missed", "skipped", "error"}, r.Status) {
			t.Errorf("result %d = %s %s, want %s missed, skipped or in error, not met", i, r.Name, r.Status, scenarioNames[i])
		}
	}
	// The hub knows no such token, so it refuses the first request of a
	// scenario that sends one.
	if r := result(t, rep, "fork_create"); r.Status != perf.Error || !strings.Contains(r.Detail, "401") {
		t.Errorf("fork_create = %+v, want error naming the hub's 401", r)
	}
	t.Log("\n" + rep.Table())
}
