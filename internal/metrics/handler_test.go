package metrics_test

import (
	"mime"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/metrics"
)

// TestMain runs the package's tests at most hubtest.HubParallel at a time:
// some start a hub, and hubs contend for one lock past that (see
// hubtest.HubParallel).
func TestMain(m *testing.M) {
	hubtest.LimitParallel(hubtest.HubParallel)
	os.Exit(m.Run())
}

// scrape gets /metrics from h with credential and parses the exposition.
func scrape(t *testing.T, h *hubtest.Hub, credential string) []parsed {
	t.Helper()
	status, header, body := h.Exchange(t, http.MethodGet, "/metrics", credential, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics: status = %d, want 200 (body %s)", status, body)
	}
	mt, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mt != "text/plain" || params["version"] != "0.0.4" {
		t.Errorf("Content-Type = %q, want text/plain with version=0.0.4", header.Get("Content-Type"))
	}
	families, err := parseText(string(body))
	if err != nil {
		t.Fatalf("the exposition does not parse: %v\n%s", err, body)
	}
	return families
}

// withoutHTTP is families with the samples of the HTTP request metrics
// left out.
func withoutHTTP(families []parsed) []parsed {
	out := slices.Clone(families)
	for i, f := range out {
		if f.name == metrics.HTTPRequestsTotal || f.name == metrics.HTTPRequestDurationSeconds {
			out[i].samples = nil
		}
	}
	return out
}

func TestMetricsEndpoint_AnswersAnyClientWithTheWholeCatalogue(t *testing.T) {
	t.Parallel()
	h := hubtest.Start(t, nil)
	key := h.IssueKey(t, h.CreateUser(t, "scraper"))
	want := make([]string, len(documented))
	for i, m := range documented {
		want[i] = m.name
	}
	for _, tc := range []struct{ name, credential string }{
		{"no credential", ""},
		{"a credential that is not one", "garbage"},
		{"a valid API key", key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			families := scrape(t, h, tc.credential)
			names := make([]string, len(families))
			for i, f := range families {
				names[i] = f.name
				if len(f.help) != 1 || len(f.typ) != 1 {
					t.Errorf("%s has %d HELP and %d TYPE lines, want one each", f.name, len(f.help), len(f.typ))
				}
				if f.name != metrics.SCMRequestsTotal {
					continue
				}
				if !slices.IsSortedFunc(f.samples, func(a, b sample) int {
					return slices.Compare([]string{a.label("op"), a.label("result")}, []string{b.label("op"), b.label("result")})
				}) {
					t.Errorf("the samples of %s are not sorted by (op, result)", f.name)
				}
			}
			if !slices.Equal(names, want) {
				t.Errorf("families = %v, want the catalogue %v", names, want)
			}
		})
	}

	first, second := scrape(t, h, ""), scrape(t, h, "")
	if !reflect.DeepEqual(withoutHTTP(first), withoutHTTP(second)) {
		t.Errorf("two scrapes with no report between them differ outside the HTTP request metrics")
	}
	counted := false
	for _, f := range second {
		for _, s := range f.samples {
			if f.name == metrics.HTTPRequestsTotal && s.label("method") == "GET" && s.label("path") == "/metrics" &&
				s.label("status") == "200" && s.value >= 1 {
				counted = true
			}
		}
	}
	if !counted {
		t.Errorf("the second scrape does not count the first as GET /metrics 200")
	}
	if status, body := h.Call(t, http.MethodGet, "/api/v1/metrics", "", nil); status != http.StatusNotFound {
		t.Errorf("GET /api/v1/metrics: status = %d, want 404 (body %s)", status, body)
	}
}

func TestMetricsEndpoint_RefusesMethodsOtherThanGET(t *testing.T) {
	t.Parallel()
	h := hubtest.Start(t, nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			status, header, body := h.Exchange(t, method, "/metrics", "", nil, nil)
			if status != http.StatusMethodNotAllowed || !strings.Contains(header.Get("Allow"), http.MethodGet) {
				t.Errorf("%s /metrics: status = %d, Allow = %q, want 405 naming GET", method, status, header.Get("Allow"))
			}
			if strings.Contains(string(body), "# TYPE") {
				t.Errorf("%s /metrics wrote the exposition: %s", method, body)
			}
		})
	}
}
