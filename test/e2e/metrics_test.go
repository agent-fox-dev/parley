package e2e

import (
	"bufio"
	"context"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/metrics"
)

// TestMetrics_RetentionPrunesOldRecordsAtStartAndTheOperatorWatchesIt is an
// operator who keeps run telemetry for a day and starts the hub on data
// holding a run event three days old: the retention run at start deletes
// it, keeps the new one, and GET /metrics, read without a credential, shows
// every metric of the catalogue, the run's time and the recounted records.
func TestMetrics_RetentionPrunesOldRecordsAtStartAndTheOperatorWatchesIt(t *testing.T) {
	parallel(t)
	// The event log may not keep an event longer than the audit record
	// does, so its retention comes down with the audit's maximum age.
	h := hubtest.StartWith(t, hubtest.Config{TOML: "[audit]\nmax_age = \"1d\"\n[events]\nretention = \"1d\"\n"})
	owner := h.CreateUser(t, "olive")
	ownerKey := h.IssueKey(t, owner)
	h.AddWorkspace(t, access.Record{Slug: "demo", OwnerID: owner, OrgID: h.CreateOrg(t, "acme"), Kind: access.KindRepository, Status: access.StatusReady})
	agent := h.IssuePAT(t, identity.PATSpec{UserID: owner, Permissions: []string{"audit:write"}, Workspace: "demo"})
	events := "/api/v1/workspaces/demo/runs/" + auditRun + "/events"
	now := time.Now().UTC()
	ids := map[string]string{"old": "0a1b2c3d-0000-4000-8000-0000000000a1", "new": "0a1b2c3d-0000-4000-8000-0000000000a2"}
	for name, at := range map[string]time.Time{"old": now.Add(-72 * time.Hour), "new": now} {
		body := map[string]any{"id": ids[name], "event_type": "tool.started", "timestamp": at.Format(time.RFC3339)}
		if status, raw := h.Call(t, http.MethodPost, events, agent, body); status != http.StatusCreated {
			t.Fatalf("ingest the %s run event = %d %s, want 201", name, status, raw)
		}
	}

	h.Restart(t)
	var families map[string]bool
	var samples map[string]float64
	var mediaType, version string
	waitUntil(10*time.Second, func() bool {
		resp, err := http.Get(h.URL("/metrics"))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		mediaType, version = contentType(resp.Header.Get("Content-Type"))
		families, samples = parseExposition(t, resp.Body)
		return resp.StatusCode == http.StatusOK && samples["parley_retention_last_run_timestamp_seconds"] > 0
	})
	if samples["parley_retention_last_run_timestamp_seconds"] <= 0 {
		t.Fatalf("no retention run reached /metrics within 10 s: %v", samples)
	}
	if mediaType != "text/plain" || version != "0.0.4" {
		t.Errorf("Content-Type media type %q, version %q; want text/plain, 0.0.4", mediaType, version)
	}
	catalogue := metrics.New(nil).Gather(context.Background())
	if len(catalogue) != 27 {
		t.Fatalf("the catalogue holds %d metrics, want 27", len(catalogue))
	}
	for _, f := range catalogue {
		if !families[f.Name] {
			t.Errorf("GET /metrics has no family %s", f.Name)
		}
	}
	for series, v := range samples {
		if strings.HasPrefix(series, "parley_retention_errors_total{") && v != 0 {
			t.Errorf("%s = %v, want 0", series, v)
		}
	}
	if got := samples[`parley_audit_records{kind="run_event"}`]; got != 1 {
		t.Errorf(`parley_audit_records{kind="run_event"} = %v, want 1`, got)
	}

	status, raw := h.Call(t, http.MethodGet, events, ownerKey, nil)
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decodeInto(t, raw, &list)
	if status != http.StatusOK || len(list.Items) != 1 || list.Items[0].ID != ids["new"] {
		t.Errorf("GET run events = %d %s, want the new run event alone", status, raw)
	}
}

// contentType is the media type of a Content-Type value and its version
// parameter.
func contentType(v string) (mediaType, version string) {
	mt, params, err := mime.ParseMediaType(v)
	if err != nil {
		return "", ""
	}
	return mt, params["version"]
}

// parseExposition reads a Prometheus text exposition: the families its TYPE
// lines name, and each sample's value by the series as written.
func parseExposition(t *testing.T, r io.Reader) (families map[string]bool, samples map[string]float64) {
	t.Helper()
	families, samples = map[string]bool{}, map[string]float64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			families[strings.Fields(name)[0]] = true
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("sample line %q: %v", line, err)
		}
		samples[line[:i]] = v
	}
	return families, samples
}
