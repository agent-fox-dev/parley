package perf

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The number of requests of a request-duration scenario, and of commits
// of event_latency.
const (
	forkCreateSamples   = 50
	fileReadSamples     = 200
	commitCreateSamples = 50
	commitCreateFiles   = 100
	sandboxExecSamples  = 50
	eventLatencySamples = 50
)

// sandboxPoll is the time between two reads of a sandbox that is not ready
// yet.
const sandboxPoll = 200 * time.Millisecond

// commitEvent is the type of the hub event of a commit the source control
// API made.
const commitEvent = "parley.scm.commit"

// forkCreate creates forks of the first workspace one after the other and
// measures each request.
func forkCreate(ctx context.Context, e *env) (outcome, error) {
	samples := make([]float64, 0, forkCreateSamples)
	for range forkCreateSamples {
		_, resp, err := e.fork(ctx)
		if err != nil {
			return outcome{}, err
		}
		samples = append(samples, ms(resp.duration))
	}
	return p95Outcome(samples), nil
}

// fork is a fork of the first workspace, recorded for deletion, as its
// create request answered it.
type fork struct {
	Slug          string  `json:"slug"`
	DefaultBranch *string `json:"default_branch"`
}

// fork creates a fork of the first workspace.
func (e *env) fork(ctx context.Context) (fork, response, error) {
	path := workspacePath(e.cfg.Workspaces[0], "/forks")
	body := map[string]string{"purpose": "parley performance run " + e.runID}
	var ans struct {
		Workspace fork `json:"workspace"`
	}
	resp, err := e.client.json(ctx, http.MethodPost, path, body, &ans)
	if ans.Workspace.Slug != "" {
		e.created.remove(workspacePath(ans.Workspace.Slug, ""))
	}
	if err == nil && ans.Workspace.Slug == "" {
		err = errors.New("POST " + apiMount + path + ": the answer names no fork")
	}
	return ans.Workspace, resp, err
}

// fileRead reads, alternately, the first workspace's root tree and one file
// of it, and measures each request. The first read of the tree is also the
// one that picks the file.
func fileRead(ctx context.Context, e *env) (outcome, error) {
	scm := workspacePath(e.cfg.Workspaces[0], "/scm")
	var tree struct {
		Items []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"items"`
	}
	resp, err := e.client.json(ctx, http.MethodGet, scm+"/tree", nil, &tree)
	if err != nil {
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
	samples := make([]float64, 0, fileReadSamples)
	samples = append(samples, ms(resp.duration))
	reads := [2]string{scm + "/tree", scm + "/file?path=" + url.QueryEscape(file)}
	for i := 1; i < fileReadSamples; i++ {
		resp, err := e.client.do(ctx, http.MethodGet, reads[i%2], nil)
		if err != nil {
			return outcome{}, err
		}
		samples = append(samples, ms(resp.duration))
	}
	return p95Outcome(samples), nil
}

// commitCreate makes commits of commitCreateFiles files each, one after
// the other, on a branch of a fork of its own, and measures each request.
func commitCreate(ctx context.Context, e *env) (outcome, error) {
	f, _, err := e.fork(ctx)
	if err != nil {
		return outcome{}, err
	}
	if f.DefaultBranch == nil || *f.DefaultBranch == "" {
		return outcome{}, errors.New("fork " + f.Slug + " has no default branch to start a branch from")
	}
	b := &branchHead{slug: f.Slug, branch: e.sourceBranch("commit_create", 0), base: *f.DefaultBranch}
	samples := make([]float64, 0, commitCreateSamples)
	for i := range commitCreateSamples {
		ops := make([]fileOp, commitCreateFiles)
		for j := range ops {
			ops[j] = put(fmt.Sprintf("perf/%s/commit_create/%d.txt", e.runID, j), fmt.Sprintf("commit %d, file %d\n", i, j))
		}
		_, resp, err := e.commit(ctx, b, fmt.Sprintf("perf commit_create: commit %d", i), ops)
		if err != nil {
			return outcome{}, err
		}
		samples = append(samples, ms(resp.duration))
	}
	return p95Outcome(samples), nil
}

// sandboxExec creates a sandbox of the configured profile in the first
// workspace, waits until it is ready, and runs true in it again and
// again, measuring each request. A client cannot tell the hub's share of a
// request from the runtime's, so the whole request is an upper bound of
// the hub's overhead.
func sandboxExec(ctx context.Context, e *env) (outcome, error) {
	id, err := e.sandbox(ctx)
	if err != nil {
		return outcome{}, err
	}
	if _, err := e.waitReady(ctx, id); err != nil {
		return outcome{}, err
	}
	path := workspacePath(e.cfg.Workspaces[0], "/sandboxes/"+url.PathEscape(id)+"/exec")
	body := map[string]any{"command": []string{"true"}}
	samples := make([]float64, 0, sandboxExecSamples)
	for range sandboxExecSamples {
		var ans struct {
			ExitCode *int `json:"exit_code"`
		}
		resp, err := e.client.json(ctx, http.MethodPost, path, body, &ans)
		if err != nil {
			return outcome{}, err
		}
		if ans.ExitCode == nil || *ans.ExitCode != 0 {
			return outcome{}, fmt.Errorf("true in sandbox %s did not exit with 0: exit code %v", id, ans.ExitCode)
		}
		samples = append(samples, ms(resp.duration))
	}
	return p95Outcome(samples), nil
}

// sandbox creates a sandbox of the configured profile in the first
// workspace, recorded for deletion, and returns its id.
func (e *env) sandbox(ctx context.Context) (string, error) {
	path := workspacePath(e.cfg.Workspaces[0], "/sandboxes")
	body := map[string]string{"profile": e.cfg.SandboxProfile, "purpose": "parley performance run " + e.runID}
	var sb struct {
		ID string `json:"id"`
	}
	_, err := e.client.json(ctx, http.MethodPost, path, body, &sb)
	if sb.ID != "" {
		e.created.remove(workspacePath(e.cfg.Workspaces[0], "/sandboxes/"+url.PathEscape(sb.ID)))
	}
	if err == nil && sb.ID == "" {
		err = errors.New("POST " + apiMount + path + ": the answer names no sandbox")
	}
	return sb.ID, err
}

// waitReady reads the sandbox id until it shows state ready and returns
// when that read was answered; a sandbox that fails is an error naming
// its state_reason.
func (e *env) waitReady(ctx context.Context, id string) (time.Time, error) {
	path := workspacePath(e.cfg.Workspaces[0], "/sandboxes/"+url.PathEscape(id))
	for {
		var sb struct {
			State       string  `json:"state"`
			StateReason *string `json:"state_reason"`
		}
		if _, err := e.client.json(ctx, http.MethodGet, path, nil, &sb); err != nil {
			return time.Time{}, err
		}
		switch sb.State {
		case "ready":
			return time.Now(), nil
		case "failed":
			reason := "no state_reason"
			if sb.StateReason != nil {
				reason = *sb.StateReason
			}
			return time.Time{}, fmt.Errorf("sandbox %s failed: %s", id, reason)
		}
		t := time.NewTimer(sandboxPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return time.Time{}, ctx.Err()
		case <-t.C:
		}
	}
}

// eventLatency subscribes to the first workspace's commit events, makes
// commits one after the other on a branch of its own, and measures for
// each the time from its answer to the arrival of its event: 0 for an
// event that came first. It reports the largest, and ends in error when an
// event has not come EventWait after its commit's answer.
func eventLatency(ctx context.Context, e *env) (outcome, error) {
	target, err := e.targetBranch(ctx)
	if err != nil {
		return outcome{}, err
	}
	slug := e.cfg.Workspaces[0]
	watch, err := e.watchCommits(ctx, slug)
	if err != nil {
		return outcome{}, err
	}
	b := &branchHead{slug: slug, branch: e.sourceBranch("event_latency", 0), base: target}
	shas := make([]string, 0, eventLatencySamples)
	answered := make(map[string]time.Time, eventLatencySamples)
	for i := range eventLatencySamples {
		msg := fmt.Sprintf("perf event_latency: commit %d", i)
		sha, _, err := e.commit(ctx, b, msg, []fileOp{put(fmt.Sprintf("perf/%s/event_latency/%d.txt", e.runID, i), msg+"\n")})
		if err != nil {
			return outcome{}, err
		}
		answered[sha] = time.Now()
		shas = append(shas, sha)
	}
	var largest float64
	for _, sha := range shas {
		at, err := watch.wait(ctx, sha, answered[sha].Add(e.cfg.EventWait))
		if err != nil {
			return outcome{}, err
		}
		largest = max(largest, ms(max(at.Sub(answered[sha]), 0)))
	}
	return outcome{measured: &largest, samples: len(shas)}, nil
}

// commitWatch records when the event of each commit arrived on a stream.
type commitWatch struct {
	mu      sync.Mutex
	arrived map[string]time.Time
	// changed is closed, and replaced, at each arrival and when the stream
	// ends.
	changed chan struct{}
	// ended is why the stream ended, nil while it is open.
	ended error
	// eventWait is how long an event may take, for the error that says it
	// did not come.
	eventWait time.Duration
}

// watchCommits opens GET /events for the commit events of the workspace
// slug and reads it until the scenario ends; the stream is closed with the
// scenario's clean-up.
func (e *env) watchCommits(ctx context.Context, slug string) (*commitWatch, error) {
	sctx, cancel := context.WithCancel(ctx)
	body, err := e.client.stream(sctx, "/events?workspace="+url.QueryEscape(slug)+"&types="+url.QueryEscape(commitEvent))
	if err != nil {
		cancel()
		return nil, err
	}
	w := &commitWatch{arrived: map[string]time.Time{}, changed: make(chan struct{}), eventWait: e.cfg.EventWait}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.read(body)
	}()
	e.created.close(func() {
		cancel()
		_ = body.Close()
		<-done
	})
	return w, nil
}

// read reads the server-sent events of r until it ends, noting when each
// commit event arrived.
func (w *commitWatch) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if event == "hub_event" {
				var ev struct {
					Type       string `json:"type"`
					ResourceID string `json:"resource_id"`
				}
				if json.Unmarshal([]byte(data), &ev) == nil && ev.Type == commitEvent {
					w.arrive(ev.ResourceID, time.Now())
				}
			}
			event, data = "", ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ended = err
	close(w.changed)
	w.changed = make(chan struct{})
}

func (w *commitWatch) arrive(sha string, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.arrived[sha]; !ok {
		w.arrived[sha] = at
		close(w.changed)
		w.changed = make(chan struct{})
	}
}

// wait returns when the event of the commit sha arrived, waiting for it
// until deadline.
func (w *commitWatch) wait(ctx context.Context, sha string, deadline time.Time) (time.Time, error) {
	for {
		w.mu.Lock()
		at, ok := w.arrived[sha]
		ended, changed := w.ended, w.changed
		w.mu.Unlock()
		switch {
		case ok:
			return at, nil
		case ended != nil:
			return time.Time{}, fmt.Errorf("the event stream ended before the event of commit %s arrived: %v", sha, ended)
		}
		t := time.NewTimer(time.Until(deadline))
		select {
		case <-ctx.Done():
			t.Stop()
			return time.Time{}, ctx.Err()
		case <-t.C:
			return time.Time{}, fmt.Errorf("the event of commit %s did not arrive within %s of its answer", sha, w.eventWait)
		case <-changed:
			t.Stop()
		}
	}
}

// stream sends GET path below the REST API's mount and returns the body of
// a 200 answer, open for reading; the caller closes it. An answer of 400
// or more is returned as a *requestError.
func (c *client) stream(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+apiMount+path, nil)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %s", urlPath(apiMount+path), c.scrub(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError(http.MethodGet, apiMount+path, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return nil, c.refusal(http.MethodGet, apiMount+path, response{status: resp.StatusCode, body: b})
	}
	return resp.Body, nil
}
