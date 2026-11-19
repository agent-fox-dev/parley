package hub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/metrics"
)

// series is every series of the metric name in reg with a value other than
// 0, by its label values joined with ",", and, for a histogram, its count
// and sum under "count" and "sum".
func series(reg *metrics.Registry, name string) map[string]float64 {
	out := map[string]float64{}
	for _, f := range reg.Gather(context.Background()) {
		if f.Name != name {
			continue
		}
		for _, s := range f.Series {
			if f.Type == metrics.Histogram {
				out["count"], out["sum"] = float64(s.Count), s.Sum
			} else if s.Value != 0 {
				out[strings.Join(s.LabelValues, ",")] = s.Value
			}
		}
	}
	return out
}

func TestMetricSources_ExportWhatTheirProducersReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		metric  string
		connect func(*metrics.Registry) error
		want    map[string]float64
	}{
		{"git requests", metrics.GitRequestsTotal, func(reg *metrics.Registry) error {
			return reg.RegisterSource(metrics.GitRequestsTotal, hub.GitRequestsSource(func() map[gitserver.RequestLabels]uint64 {
				return map[gitserver.RequestLabels]uint64{
					{Service: gitserver.LabelInfoRefs, Result: gitserver.ResultOK}:         3,
					{Service: gitserver.LabelReceivePack, Result: gitserver.ResultRefused}: 1,
				}
			}))
		}, map[string]float64{"info-refs,ok": 3, "receive-pack,refused": 1}},
		{"open changes", metrics.ChangesOpen, func(reg *metrics.Registry) error {
			return reg.RegisterSource(metrics.ChangesOpen, hub.ChangesOpenSource(func(context.Context) (map[string]int, error) {
				return map[string]int{"demo": 2, "other": 1}, nil
			}))
		}, map[string]float64{"demo": 2, "other": 1}},
		{"active forks", metrics.ForksActive, func(reg *metrics.Registry) error {
			return reg.RegisterSource(metrics.ForksActive, hub.ForksActiveSource(func(context.Context) (int, error) { return 4, nil }))
		}, map[string]float64{"": 4}},
		{"job queue depth", metrics.JobqueueDepth, func(reg *metrics.Registry) error {
			return reg.RegisterSource(metrics.JobqueueDepth, hub.JobqueueDepthSource(func(context.Context) (map[string]map[string]int, error) {
				return map[string]map[string]int{"rebuild": {"queued": 2, "running": 1, "failed": 0}}, nil
			}))
		}, map[string]float64{"rebuild,queued": 2, "rebuild,running": 1}},
		{"source control requests", metrics.SCMRequestsTotal, func(reg *metrics.Registry) error {
			rec := hub.SCMRecorder(reg)
			rec.Observe("refs", "ok")
			rec.Observe("refs", "ok")
			rec.Observe("ref_move", "ref_moved")
			return nil
		}, map[string]float64{"refs,ok": 2, "ref_move,ref_moved": 1}},
		{"land results", metrics.LandsTotal, func(reg *metrics.Registry) error {
			hub.LandReporter(reg)(context.Background(), land.Final{Result: land.StatusLanded, Attempts: 3, Duration: 1500 * time.Millisecond})
			return nil
		}, map[string]float64{"landed": 1}},
		{"land attempts", metrics.LandAttempts, func(reg *metrics.Registry) error {
			hub.LandReporter(reg)(context.Background(), land.Final{Result: land.StatusConflict, Attempts: 3, Duration: time.Second})
			return nil
		}, map[string]float64{"count": 1, "sum": 3}},
		{"land duration", metrics.LandDurationSeconds, func(reg *metrics.Registry) error {
			hub.LandReporter(reg)(context.Background(), land.Final{Result: land.StatusLanded, Attempts: 1, Duration: 1500 * time.Millisecond})
			return nil
		}, map[string]float64{"count": 1, "sum": 1.5}},
		{"tokens of each direction above 0", metrics.AgentTokensTotal, func(reg *metrics.Registry) error {
			hub.TokenReporter(reg)("demo", "m1", 5, 0, 2)
			return nil
		}, map[string]float64{"demo,m1,input": 5, "demo,m1,cache_read": 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := metrics.New(nil)
			if err := tc.connect(reg); err != nil {
				t.Fatal(err)
			}
			if got := series(reg, tc.metric); !maps.Equal(got, tc.want) {
				t.Errorf("%s = %v, want %v", tc.metric, got, tc.want)
			}
		})
	}
}

// seedDeliveries stores a switched-off event webhook subscription with an
// ended delivery record last attempted at each of times, and returns their
// ids in that order.
func seedDeliveries(t *testing.T, h *hubtest.Hub, times ...time.Time) []string {
	t.Helper()
	db := h.Store.Main()
	if _, err := db.Exec(`INSERT INTO webhook_subscriptions (id, workspace, url, types, include_forks, description, state,
		created_at, updated_at, secret_sealed, position) VALUES ('s1', 'demo', 'https://ci.example/x', '["*"]', 1, '',
		'disabled', 0, 0, x'00', 0)`); err != nil {
		t.Fatalf("store a subscription: %v", err)
	}
	ids := make([]string, len(times))
	for i, at := range times {
		ids[i] = fmt.Sprintf("d%d", i)
		if _, err := db.Exec(`INSERT INTO webhook_deliveries (id, subscription_id, seq, type, status, attempts, created_at,
			first_attempt_at, last_attempt_at) VALUES (?, 's1', 1, 'parley.test', 'succeeded', 1, ?, ?, ?)`,
			ids[i], at.UnixNano(), at.UnixNano(), at.UnixNano()); err != nil {
			t.Fatalf("store a delivery record: %v", err)
		}
	}
	return ids
}

func TestMetrics_ConnectWhatTheHubsPackagesReportAndRunRetentionAtTheStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	var reg *metrics.Registry
	var events *event.Component
	h := hubtest.StartWith(t, hubtest.Config{Now: func() time.Time { return at }, Compose: func(c *hub.Composition) {
		reg, events = c.Metrics(), c.Events()
	}})

	// The hub runs the retention run once it listens; its end sets the
	// last-run time. Its webhook_deliveries step prunes the event webhooks'
	// ended delivery records last attempted more than 7 days before the
	// hub's time, so the restart's run deletes the first seeded record and
	// keeps the one attempted exactly 7 days before.
	cutoff := at.Add(-7 * 24 * time.Hour)
	seeded := seedDeliveries(t, h, cutoff.Add(-time.Second), cutoff)
	h.Restart(t)
	deadline := time.Now().Add(5 * time.Second)
	for series(reg, metrics.RetentionLastRunTimestamp)[""] == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := series(reg, metrics.RetentionLastRunTimestamp)[""]; got != float64(at.Unix()) {
		t.Fatalf("last retention run = %v, want %d", got, at.Unix())
	}
	var left []string
	rows, err := h.Store.Main().Query(`SELECT id FROM webhook_deliveries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(left, seeded[1:]) {
		t.Errorf("delivery records after the retention run = %v, want %v", left, seeded[1:])
	}

	alice, acme := h.CreateUser(t, "alice"), h.CreateOrg(t, "acme")
	h.AddWorkspace(t, access.Record{Slug: "demo", OwnerID: alice, OrgID: acme, Kind: access.KindRepository, Status: access.StatusReady})
	h.AddWorkspace(t, access.Record{Slug: "demo--task", OwnerID: alice, OrgID: acme, Kind: access.KindFork,
		ParentSlug: "demo", Status: access.StatusReady})
	call := func(method, path string, body any, want int) []byte {
		t.Helper()
		status, raw := h.Call(t, method, path, h.Token, body)
		if want != 0 && status != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, status, raw, want)
		}
		return raw
	}
	call(http.MethodPost, "/api/v1/workspaces/demo/runs/20260601_110000_abcdef/events", map[string]any{
		"id": "0a1b2c3d-0000-4000-8000-000000000001", "timestamp": "2026-06-01T11:00:00Z", "event_type": "run.start",
	}, http.StatusCreated)
	var opened struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(call(http.MethodPost, "/api/v1/sessions", map[string]any{"workspace_slug": "demo"}, http.StatusCreated),
		&opened); err != nil {
		t.Fatal(err)
	}
	call(http.MethodPost, "/api/v1/sessions/"+opened.ID+"/usage", map[string]any{
		"id": "0a1b2c3d-0000-4000-8000-000000000002", "model": "m1", "input_tokens": 5, "cache_read_tokens": 2,
	}, http.StatusCreated)
	call(http.MethodGet, "/git/acme/demo/info/refs?service=git-upload-pack", nil, 0)
	for range 2 {
		if _, err := events.EmitNow(ctx, event.Input{Type: "parley.test.counted", ActorType: event.ActorSystem,
			ResourceType: "workspace", ResourceID: "demo"}); err != nil {
			t.Fatal(err)
		}
	}

	bounds, err := events.Bounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	infoRefs := 0.0
	for labels, n := range series(reg, metrics.GitRequestsTotal) {
		if strings.HasPrefix(labels, gitserver.LabelInfoRefs+",") {
			infoRefs += n
		}
	}
	for _, c := range []struct {
		what      string
		got, want float64
	}{
		{"run events of type run.start", series(reg, metrics.AuditEventsTotal)["agent,run.start"], 1},
		{"hub events of type parley.test.counted", series(reg, metrics.AuditEventsTotal)["hub,parley.test.counted"], 2},
		{"last seq", series(reg, metrics.EventsSeq)[""], float64(bounds.LastSeq)},
		{"input tokens", series(reg, metrics.AgentTokensTotal)["demo,m1,input"], 5},
		{"cache read tokens", series(reg, metrics.AgentTokensTotal)["demo,m1,cache_read"], 2},
		{"output tokens", series(reg, metrics.AgentTokensTotal)["demo,m1,output"], 0},
		{"active forks", series(reg, metrics.ForksActive)[""], 1},
		{"git ref discoveries", infoRefs, 1},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.what, c.got, c.want)
		}
	}
}
