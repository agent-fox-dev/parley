package hub

import (
	"context"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/metrics"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/server"
)

// composeMetrics adds the hub's metrics: the registry the composition was
// created with (Metrics), served as GET /metrics outside the REST API and
// without a credential, and the counting of every request the server
// answers, wrapped around its whole handler so that the REST API, the git
// and webhook surfaces, the probes and /metrics itself are all counted. The
// time is taken from the hub's clock.
//
// The registry is the sandbox feature's recorder unless a part composed
// later, such as a test's Options.Compose, sets another with
// SetSandboxRecorder; the sandbox packages report their metrics to it
// themselves. The values the other packages export are connected to it by
// connectProducers.
func composeMetrics(c *Composition) {
	reg := c.metrics
	c.Handle("GET /metrics", metrics.Handler(reg))
	c.SetSandboxRecorder(reg)
	connectProducers(c, reg)
	// The runtime's clock is set once the stores are open, before the
	// server's handler is built.
	now := func() time.Time { return c.runtime.Now() }
	c.wrapHandler = func(h http.Handler) http.Handler {
		return metrics.Instrument(reg, now, server.MatchedPattern, h)
	}
}

// Metrics returns the hub's metrics registry, one per composition, so that
// hubs in one process never share series. A part reports its metrics to it
// or registers the sources of its metrics with it while it composes.
func (c *Composition) Metrics() *metrics.Registry { return c.metrics }

// connectProducers connects to reg the values the hub's packages export for
// their metrics, one reporter per metric, since none of them depends on the
// registry: the counters, stores and queue it reads at every scrape as
// sources, and the observers and recorder they call as reports. The
// observers count from the composition on; a report the registry refuses
// is dropped, since a metric never stops the feature it counts. A source
// the registry refuses stops the start.
//
// The source reads go to components the hub binds in its start hooks,
// which have all run before the first scrape can be served.
func connectProducers(c *Composition, reg *metrics.Registry) {
	agent := &tally{}
	c.Telemetry().OnRunEvent(agent.add)
	c.Sessions().OnUsage(tokenReporter(reg))
	c.Land().AddFinalObserver(landReporter(reg))
	// Added now, an adjuster of a part composed later, such as a test's
	// recorder, still replaces it.
	c.AdjustSCM(func(d *scm.Deps) { d.Recorder = scmRecorder{reg} })
	for _, s := range []struct {
		name string
		src  metrics.Source
	}{
		{metrics.GitRequestsTotal, gitRequests(c.GitServer().Requests)},
		{metrics.ChangesOpen, changesOpen(c.Changes().OpenCounts)},
		{metrics.ForksActive, forksActive(c.Forks().CountTaskForks)},
		{metrics.JobqueueDepth, jobqueueDepth(c.Jobs().Depth)},
		{metrics.AuditEventsTotal, auditEvents(c.Events().Counters, agent)},
		{metrics.EventsSeq, eventCounter(c.Events().Counters, func(n event.Counters) float64 { return float64(n.LastSeq) })},
		{metrics.SSEConnections, eventCounter(c.Events().Counters, func(n event.Counters) float64 { return float64(n.OpenStreams) })},
	} {
		if err := reg.RegisterSource(s.name, s.src); err != nil {
			c.errs = append(c.errs, err)
		}
	}
}

// tally counts the run events stored since the start, by type; its zero
// value is ready and safe for concurrent use.
type tally struct {
	mu sync.Mutex
	n  map[string]int64
}

func (t *tally) add(eventType string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n == nil {
		t.n = map[string]int64{}
	}
	t.n[eventType]++
}

func (t *tally) snapshot() map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return maps.Clone(t.n)
}

// auditEvents is the source of parley_audit_events_total, which has two
// producers and takes one source: the hub events the event component
// counted as written, under source hub, and the run events of agent, under
// source agent.
func auditEvents(counters func() event.Counters, agent *tally) metrics.Source {
	return func(context.Context) ([]metrics.Sample, error) {
		var out []metrics.Sample
		for source, byType := range map[string]map[string]int64{
			"hub": counters().WrittenByType, "agent": agent.snapshot(),
		} {
			for typ, n := range byType {
				out = append(out, metrics.Sample{Labels: map[string]string{"source": source, "type": typ}, Value: float64(n)})
			}
		}
		return out, nil
	}
}

// eventCounter is the source of a metric without labels that value reads
// from the event component's counters.
func eventCounter(counters func() event.Counters, value func(event.Counters) float64) metrics.Source {
	return func(context.Context) ([]metrics.Sample, error) {
		return []metrics.Sample{{Value: value(counters())}}, nil
	}
}

// tokenReporter reports each usage record to parley_agent_tokens_total,
// once per direction whose count is above 0.
func tokenReporter(rec metrics.Recorder) func(workspace, model string, input, output, cacheRead int64) {
	return func(workspace, model string, input, output, cacheRead int64) {
		for direction, n := range map[string]int64{"input": input, "output": output, "cache_read": cacheRead} {
			if n > 0 {
				_ = rec.Add(metrics.AgentTokensTotal,
					map[string]string{"workspace": workspace, "model": model, "direction": direction}, float64(n))
			}
		}
	}
}

// landReporter reports each land that reached its final status: its result
// to parley_lands_total, its attempts to parley_land_attempts and the time
// from its request to its end to parley_land_duration_seconds.
func landReporter(rec metrics.Recorder) land.FinalObserver {
	return func(_ context.Context, f land.Final) {
		_ = rec.Add(metrics.LandsTotal, map[string]string{"result": f.Result}, 1)
		_ = rec.Observe(metrics.LandAttempts, nil, float64(f.Attempts))
		_ = rec.Observe(metrics.LandDurationSeconds, nil, f.Duration.Seconds())
	}
}

// scmRecorder is the request recorder of the source control API, which
// counts each request in parley_scm_requests_total.
type scmRecorder struct{ rec metrics.Recorder }

func (r scmRecorder) Observe(op, result string) {
	_ = r.rec.Add(metrics.SCMRequestsTotal, map[string]string{"op": op, "result": result}, 1)
}

// gitRequests is the source of parley_git_requests_total: the git server's
// request counter, as requests reads it.
func gitRequests(requests func() map[gitserver.RequestLabels]uint64) metrics.Source {
	return func(context.Context) ([]metrics.Sample, error) {
		var out []metrics.Sample
		for l, n := range requests() {
			out = append(out, metrics.Sample{
				Labels: map[string]string{"service": l.Service, "result": string(l.Result)}, Value: float64(n),
			})
		}
		return out, nil
	}
}

// changesOpen is the source of parley_changes_open: the draft and open
// changes of each workspace that has any, as openCounts reads them.
func changesOpen(openCounts func(context.Context) (map[string]int, error)) metrics.Source {
	return func(ctx context.Context) ([]metrics.Sample, error) {
		counts, err := openCounts(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]metrics.Sample, 0, len(counts))
		for slug, n := range counts {
			out = append(out, metrics.Sample{Labels: map[string]string{"workspace": slug}, Value: float64(n)})
		}
		return out, nil
	}
}

// forksActive is the source of parley_forks_active: the task forks that
// exist, as count reads them.
func forksActive(count func(context.Context) (int, error)) metrics.Source {
	return func(ctx context.Context) ([]metrics.Sample, error) {
		n, err := count(ctx)
		if err != nil {
			return nil, err
		}
		return []metrics.Sample{{Value: float64(n)}}, nil
	}
}

// jobqueueDepth is the source of parley_jobqueue_depth: the durable jobs
// of each type in each status, as depth reads them.
func jobqueueDepth(depth func(context.Context) (map[string]map[string]int, error)) metrics.Source {
	return func(ctx context.Context) ([]metrics.Sample, error) {
		byType, err := depth(ctx)
		if err != nil {
			return nil, err
		}
		var out []metrics.Sample
		for typ, byStatus := range byType {
			for status, n := range byStatus {
				out = append(out, metrics.Sample{Labels: map[string]string{"type": typ, "status": status}, Value: float64(n)})
			}
		}
		return out, nil
	}
}
