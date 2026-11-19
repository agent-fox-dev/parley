package hub

import (
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/metrics"
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
// SetSandboxRecorder.
func composeMetrics(c *Composition) {
	reg := c.metrics
	c.Handle("GET /metrics", metrics.Handler(reg))
	c.SetSandboxRecorder(reg)
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
