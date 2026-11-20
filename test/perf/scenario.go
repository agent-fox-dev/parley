package perf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

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
	// the body returns the count it reached.
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

// scenarios are the fourteen scenarios in the order of the report.
func scenarios() []scenario {
	capacity := []string{EnvCapacity}
	sandbox := []string{EnvSandboxProfile}
	return []scenario{
		{name: "fork_create", target: Target{LT, 1000, "ms"}, body: forkCreate},
		{name: "file_read", target: Target{LT, 100, "ms"}, body: fileRead},
		{name: "commit_create", target: Target{LT, 500, "ms"}, body: commitCreate},
		{name: "land", target: Target{LT, 2000, "ms"}, body: land},
		{name: "land_throughput", target: Target{GE, 60, "per_minute"}, body: landThroughput},
		{name: "land_concurrency", target: Target{LE, 0, "count"}, body: landConcurrency},
		{name: "mergeability", target: Target{LE, 60, "s"}, body: mergeability},
		{name: "event_latency", target: Target{LE, 1000, "ms"}, body: eventLatency},
		{name: "active_forks", target: Target{GE, activeForksCount, "count"}, needs: capacity, capacity: true, body: activeForks},
		{name: "open_changes", target: Target{GE, openChangesCount, "count"}, needs: capacity, capacity: true, body: openChanges},
		{name: "event_streams", target: Target{GE, eventStreamsCount, "count"}, needs: capacity, capacity: true, body: eventStreams},
		{name: "sandboxes_running", target: Target{GE, sandboxesRunningCount, "count"}, needs: []string{EnvCapacity, EnvSandboxProfile},
			capacity: true, body: sandboxesRunning},
		{name: "sandbox_ready", target: Target{LE, 60, "s"}, needs: sandbox, body: sandboxReady},
		{name: "sandbox_exec", target: Target{LT, 200, "ms"}, needs: sandbox, body: sandboxExec},
	}
}

// workspacePath is the API path of the workspace slug, followed by rest.
func workspacePath(slug, rest string) string { return "/workspaces/" + url.PathEscape(slug) + rest }

// targetBranch is the branch every change of the run targets: perf/<run
// id> in the first workspace, made at the head of its default branch by the
// first scenario that needs it. The default branch itself is never written.
func (e *env) targetBranch(ctx context.Context) (string, error) {
	slug := e.cfg.Workspaces[0]
	var ws struct {
		DefaultBranch *string `json:"default_branch"`
	}
	if _, err := e.client.json(ctx, http.MethodGet, workspacePath(slug, ""), nil, &ws); err != nil {
		return "", err
	}
	if ws.DefaultBranch == nil || *ws.DefaultBranch == "" {
		return "", errors.New("workspace " + slug + " has no default branch to start the run's target branch from")
	}
	name := "perf/" + e.runID
	_, err := e.client.do(ctx, http.MethodPost, workspacePath(slug, "/scm/refs"), map[string]string{"name": name, "ref": *ws.DefaultBranch})
	// An earlier scenario of the run made it.
	if re, ok := errors.AsType[*requestError](err); ok && re.status == http.StatusConflict && re.errorType == "ref_exists" {
		err = nil
	}
	return name, err
}

// sourceBranch is the n-th branch the scenario commits to. Its prefix is
// not perf/<run id>, which a branch below the target's name would clash
// with.
func (e *env) sourceBranch(scenario string, n int) string {
	return fmt.Sprintf("perf-%s/%s/%d", e.runID, scenario, n)
}

// fileOp is a file operation of a commit request.
type fileOp struct {
	Op      string `json:"op"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// put is the operation that writes content to path.
func put(path, content string) fileOp { return fileOp{Op: "put", Path: path, Content: content} }

// branchHead is a branch of a workspace the harness commits to, and the
// commit it is at: empty before the first commit, which creates the branch
// at base.
type branchHead struct {
	slug, branch, base string
	head               string
}

// commit makes one commit of ops on b and moves b's head to it. It returns
// the commit's id and the answer, whose duration is the request's.
func (e *env) commit(ctx context.Context, b *branchHead, message string, ops []fileOp) (string, response, error) {
	body := map[string]any{"branch": b.branch, "message": message, "operations": ops}
	if b.head == "" {
		body["base_ref"] = b.base
	} else {
		body["expected_head_sha"] = b.head
	}
	var ans struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	resp, err := e.client.json(ctx, http.MethodPost, workspacePath(b.slug, "/scm/commits"), body, &ans)
	if err != nil {
		return "", resp, err
	}
	if ans.Commit.SHA == "" {
		return "", resp, errors.New("POST " + apiMount + workspacePath(b.slug, "/scm/commits") + ": the answer names no commit")
	}
	b.head = ans.Commit.SHA
	return b.head, resp, nil
}

// openChange makes a change of commits commits, each writing one file of
// its own, on the n-th source branch of the scenario, and opens it against
// target. It returns the change's number.
func (e *env) openChange(ctx context.Context, target, scenario string, n, commits int) (int64, error) {
	slug := e.cfg.Workspaces[0]
	b := &branchHead{slug: slug, branch: e.sourceBranch(scenario, n), base: target}
	for i := range commits {
		path := fmt.Sprintf("perf/%s/%s/%d/%d.txt", e.runID, scenario, n, i)
		msg := fmt.Sprintf("perf %s: change %d, commit %d", scenario, n, i)
		if _, _, err := e.commit(ctx, b, msg, []fileOp{put(path, msg+"\n")}); err != nil {
			return 0, err
		}
	}
	body := map[string]any{"source": map[string]string{"branch": b.branch}, "target_branch": target,
		"title": fmt.Sprintf("perf %s %d (run %s)", scenario, n, e.runID)}
	var ch struct {
		Number int64 `json:"number"`
	}
	if _, err := e.client.json(ctx, http.MethodPost, workspacePath(slug, "/changes"), body, &ch); err != nil {
		return 0, err
	}
	if ch.Number <= 0 {
		return 0, errors.New("POST " + apiMount + workspacePath(slug, "/changes") + ": the answer names no change number")
	}
	return ch.Number, nil
}
