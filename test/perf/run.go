package perf

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// versionWait bounds the GET /version that begins a run.
const versionWait = 10 * time.Second

// Run measures the targets of the hub cfg names and writes the report to
// cfg.ReportPath. It fails before any scenario when cfg lacks the URL, the
// token or a workspace, or when the hub does not answer GET /version with
// 200 within 10 seconds; and after the scenarios when the report cannot be
// written, returning the report it could not write. A scenario that misses
// its target, is skipped or ends in error is a result, not a failure of
// the run.
func Run(ctx context.Context, cfg Config) (Report, error) {
	cfg = cfg.withDefaults()
	switch {
	case cfg.URL == "":
		return Report{}, errors.New("no hub URL: set " + EnvURL)
	case cfg.Token == "":
		return Report{}, errors.New("no token: set " + EnvToken)
	case len(cfg.Workspaces) == 0:
		return Report{}, errors.New("no workspace: set " + EnvWorkspace)
	}
	cl := newClient(cfg)
	version, err := hubVersion(ctx, cl)
	if err != nil {
		return Report{}, fmt.Errorf("the hub at %s did not answer GET /version with 200 within %s: %s",
			cfg.URL, versionWait, cl.detail(err.Error()))
	}
	runID, err := newRunID()
	if err != nil {
		return Report{}, err
	}
	rep := Report{Hub: Hub{URL: cfg.URL, Version: version}, RunID: runID, StartedAt: time.Now().UTC(),
		Results: []Result{}, CleanupErrors: []string{}}
	for _, sc := range scenarios() {
		res, cleanupErrors := runScenario(ctx, sc, &env{client: cl, cfg: cfg, runID: runID})
		rep.Results = append(rep.Results, res)
		rep.CleanupErrors = append(rep.CleanupErrors, cleanupErrors...)
	}
	rep.FinishedAt = time.Now().UTC()
	if err := rep.write(cfg.ReportPath); err != nil {
		return rep, err
	}
	return rep, nil
}

// hubVersion is the document the hub answers GET /version with.
func hubVersion(ctx context.Context, cl *client) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, versionWait)
	defer cancel()
	resp, err := cl.send(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusOK || !json.Valid(resp.body) {
		return nil, fmt.Errorf("answered %d with no JSON document", resp.status)
	}
	return json.RawMessage(resp.body), nil
}

// newRunID is 12 random hexadecimal characters.
func newRunID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pick a run id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// runScenario runs sc under its deadline and removes what it created, and
// returns its result and the clean-up requests that failed.
func runScenario(ctx context.Context, sc scenario, e *env) (Result, []string) {
	res := Result{Name: sc.name, Target: sc.target, Status: Error}
	if v := unsetNeed(sc, e.cfg); v != "" {
		res.Status, res.Detail = Skipped, v
		return res, nil
	}
	deadline := e.cfg.ScenarioDeadline
	if sc.capacity {
		deadline = e.cfg.CapacityDeadline
	}
	sctx, cancel := context.WithTimeout(ctx, deadline)
	e.created = &created{}
	out, err := sc.body(sctx, e)
	expired := errors.Is(sctx.Err(), context.DeadlineExceeded)
	cancel()
	cleanupErrors := e.created.cleanUp(ctx, e.client)
	switch {
	case err != nil && expired:
		res.Detail = "deadline exceeded"
	case err != nil:
		res.Detail = e.client.detail(err.Error())
	case out.measured == nil:
		res.Detail = "no measurement"
		if out.detail != "" {
			res.Detail = e.client.detail(out.detail)
		}
	default:
		res.Measured, res.Samples, res.Detail = out.measured, out.samples, e.client.detail(out.detail)
		res.Status = Verdict(sc.target, out.measured)
	}
	return res, cleanupErrors
}

// unsetNeed is the detail of a skip, naming the first variable sc needs
// that cfg does not set, or empty when sc runs.
func unsetNeed(sc scenario, cfg Config) string {
	for _, v := range sc.needs {
		switch {
		case v == EnvCapacity && !cfg.Capacity:
			return EnvCapacity + " is not 1"
		case v == EnvSandboxProfile && cfg.SandboxProfile == "":
			return EnvSandboxProfile + " is not set"
		}
	}
	return ""
}
