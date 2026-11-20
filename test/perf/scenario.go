package perf

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"
)

// The number of requests of a request-duration scenario.
const (
	forkCreateSamples = 50
	fileReadSamples   = 200
)

// cleanupWait bounds each clean-up request, which runs after the
// scenario's own deadline.
const cleanupWait = 30 * time.Second

// scenario is one target of the table and the way it is measured.
type scenario struct {
	name   string
	target Target
	// needs are the variables the scenario runs only with, in the order a
	// skip names them: EnvCapacity, EnvSandboxProfile.
	needs []string
	// capacity gives the scenario the capacity deadline instead of the
	// scenario deadline.
	capacity bool
	// body runs the scenario and returns what it measured. It ends when
	// ctx, which carries the scenario's deadline, ends; an error it
	// returns (a *requestError for a request the hub refused) ends the
	// scenario in error. A capacity scenario's expected stop is no error:
	// the body returns the count it reached. nil is a scenario the harness
	// does not measure yet.
	body func(ctx context.Context, e *env) (outcome, error)
}

// env is what a scenario's body works with.
type env struct {
	client *client
	cfg    Config
	// runID tells this run's work apart from another's on the same hub.
	runID string
	// created is where the body records what it creates, which the run
	// removes when the body has ended, whatever its verdict.
	created *created
}

// outcome is what a body measured: the value compared with the target,
// nil for none, the number of samples it took and a detail, empty unless
// the measurement needs one.
type outcome struct {
	measured *float64
	samples  int
	detail   string
}

// p95Outcome is the outcome of request durations in milliseconds: their
// p95, no measurement for none.
func p95Outcome(samples []float64) outcome {
	v, ok := P95(samples)
	if !ok {
		return outcome{}
	}
	return outcome{measured: &v, samples: len(samples)}
}

// ms is d in milliseconds.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// created records what a scenario created on the hub: the API paths whose
// DELETE removes a fork or a sandbox.
type created struct {
	mu      sync.Mutex
	deletes []string
}

// remove records that path, below the REST API's mount, is deleted when
// the scenario ends.
func (c *created) remove(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, path)
}

// cleanUp deletes what was created, the last first, each request bounded
// by cleanupWait and outliving ctx's cancellation. It returns a line for
// each deletion that failed.
func (c *created) cleanUp(ctx context.Context, cl *client) []string {
	c.mu.Lock()
	deletes := c.deletes
	c.deletes = nil
	c.mu.Unlock()
	var failed []string
	for _, path := range slices.Backward(deletes) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
		_, err := cl.do(rctx, http.MethodDelete, path, nil)
		cancel()
		if err != nil {
			failed = append(failed, cl.detail(err.Error()))
		}
	}
	return failed
}

// scenarios are the fourteen scenarios in the order of the report.
func scenarios() []scenario {
	capacity := []string{EnvCapacity}
	sandbox := []string{EnvSandboxProfile}
	return []scenario{
		{name: "fork_create", target: Target{LT, 1000, "ms"}, body: forkCreate},
		{name: "file_read", target: Target{LT, 100, "ms"}, body: fileRead},
		{name: "commit_create", target: Target{LT, 500, "ms"}},
		{name: "land", target: Target{LT, 2000, "ms"}},
		{name: "land_throughput", target: Target{GE, 60, "per_minute"}},
		{name: "land_concurrency", target: Target{LE, 0, "count"}},
		{name: "mergeability", target: Target{LE, 60, "s"}},
		{name: "event_latency", target: Target{LE, 1000, "ms"}},
		{name: "active_forks", target: Target{GE, 5000, "count"}, needs: capacity, capacity: true},
		{name: "open_changes", target: Target{GE, 2000, "count"}, needs: capacity, capacity: true},
		{name: "event_streams", target: Target{GE, 500, "count"}, needs: capacity, capacity: true},
		{name: "sandboxes_running", target: Target{GE, 200, "count"}, needs: []string{EnvCapacity, EnvSandboxProfile}, capacity: true},
		{name: "sandbox_ready", target: Target{LE, 60, "s"}, needs: sandbox},
		{name: "sandbox_exec", target: Target{LT, 200, "ms"}, needs: sandbox},
	}
}

// workspacePath is the API path of the workspace slug, followed by rest.
func workspacePath(slug, rest string) string { return "/workspaces/" + url.PathEscape(slug) + rest }

// forkCreate creates forks of the first workspace one after the other and
// measures each request.
func forkCreate(ctx context.Context, e *env) (outcome, error) {
	path := workspacePath(e.cfg.Workspaces[0], "/forks")
	body := map[string]string{"purpose": "parley performance run " + e.runID}
	samples := make([]float64, 0, forkCreateSamples)
	for range forkCreateSamples {
		var ans struct {
			Workspace struct {
				Slug string `json:"slug"`
			} `json:"workspace"`
		}
		resp, err := e.client.json(ctx, http.MethodPost, path, body, &ans)
		if ans.Workspace.Slug != "" {
			e.created.remove(workspacePath(ans.Workspace.Slug, ""))
		}
		if err != nil {
			return outcome{}, err
		}
		samples = append(samples, ms(resp.duration))
	}
	return p95Outcome(samples), nil
}

// fileRead reads, alternately, one file of the first workspace's root tree
// and that tree, and measures each request.
func fileRead(ctx context.Context, e *env) (outcome, error) {
	scm := workspacePath(e.cfg.Workspaces[0], "/scm")
	var tree struct {
		Items []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"items"`
	}
	if _, err := e.client.json(ctx, http.MethodGet, scm+"/tree", nil, &tree); err != nil {
		return outcome{}, err
	}
	file := ""
	for _, it := range tree.Items {
		if it.Type == "blob" {
			file = it.Path
			break
		}
	}
	if file == "" {
		return outcome{}, errors.New("the root tree of workspace " + e.cfg.Workspaces[0] + " holds no file to read")
	}
	reads := [2]string{scm + "/file?path=" + url.QueryEscape(file), scm + "/tree"}
	samples := make([]float64, 0, fileReadSamples)
	for i := range fileReadSamples {
		resp, err := e.client.do(ctx, http.MethodGet, reads[i%2], nil)
		if err != nil {
			return outcome{}, err
		}
		samples = append(samples, ms(resp.duration))
	}
	return p95Outcome(samples), nil
}
