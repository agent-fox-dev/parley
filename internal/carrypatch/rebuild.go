package carrypatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// attemptKind is how an attempt of a rebuild job ended.
type attemptKind int

const (
	// attemptCompleted completes the job.
	attemptCompleted attemptKind = iota
	// attemptRetryable fails the attempt; the queue tries the job again.
	attemptRetryable
	// attemptPermanent fails the job for good: no retry can cure it.
	attemptPermanent
	// attemptEnded ends an attempt whose workspace no longer exists: its
	// job went with it, so there is nothing to record and no event to emit.
	attemptEnded
)

// The reasons of a failed attempt that the start of an attempt gives. A
// workspace that is not ready reuses the names of the workspace routes'
// refusals.
const (
	reasonUpstreamFetchFailed = "upstream_fetch_failed"
	reasonUpstreamUnknown     = "upstream_unknown"
	reasonGitError            = "git_error"
	reasonRefMoved            = "ref_moved"
	reasonWorkspaceBusy       = "workspace_busy"
	reasonWorkspaceNotReady   = "workspace_not_ready"
	reasonWorkspaceArchived   = "workspace_archived"
)

// attemptOutcome is the outcome of one attempt of a rebuild job: its kind
// and, for a failure, the reason word and the text the job's error shows.
type attemptOutcome struct {
	kind   attemptKind
	reason string
	text   string
}

// attemptFailed is the outcome of an attempt that failed with reason, retryable
// or permanent as kind says, and the text of err.
func attemptFailed(kind attemptKind, reason string, err error) *attemptOutcome {
	return &attemptOutcome{kind: kind, reason: reason, text: err.Error()}
}

// rebuildRun is one attempt of a rebuild job: the job, what it was queued
// with, the workspace as the attempt last read it and the result document
// it keeps whole in memory, since every store of it replaces the job's.
// Once the upstream base is resolved, it also holds the workspace's
// repository, the base, the patches as the run listed them, what it did
// with each and the head under construction.
type rebuildRun struct {
	c       *Component
	job     jobs.Job
	payload rebuildPayload
	ws      workspace.Workspace
	result  rebuildResult
	results []patchResult

	repo    *gitrepo.Repo
	base    string
	patches []runPatch
	head    string
}

// runRebuildAttempt runs one attempt of the rebuild job j, which the queue
// has set running, and returns its outcome; it never ends the process. The
// attempt reads the workspace first and goes on only when it is ready; it
// then empties the job's result, fetches upstream and stores the upstream
// base it starts from. It lists the patches with the tips of their
// branches, builds the new head from the base and those tips alone in a
// scratch working tree, and publishes it as the integration branch.
func (c *Component) runRebuildAttempt(ctx context.Context, j jobs.Job) attemptOutcome {
	r := &rebuildRun{c: c, job: j}
	// A payload that does not decode leaves the options at their defaults,
	// as the job's record shows them.
	_ = json.Unmarshal(j.Payload, &r.payload)
	for _, step := range []func(context.Context) *attemptOutcome{r.start, r.listPatches, r.build, r.publish} {
		if out := step(ctx); out != nil {
			return *out
		}
	}
	return attemptOutcome{kind: attemptCompleted}
}

// start begins the attempt: it checks the workspace, empties the job's
// result, repeats the workspace's set-up, which fetches upstream, and
// stores the upstream base. A non-nil outcome ends the attempt.
func (r *rebuildRun) start(ctx context.Context) *attemptOutcome {
	if out := r.readWorkspace(ctx); out != nil {
		return out
	}
	r.result, r.results = rebuildResult{}, []patchResult{}
	if out := r.store(ctx); out != nil {
		return out
	}
	if err := r.c.EnsureSetup(ctx, r.ws); err != nil {
		if errors.Is(err, ErrUpstreamFetch) {
			return attemptFailed(attemptRetryable, reasonUpstreamFetchFailed, err)
		}
		// Another step of the set-up failing does not keep the attempt from
		// building on what the fetch brought.
		r.c.logger().Warn("carry-patch set-up incomplete at the start of a rebuild",
			"workspace", r.ws.Slug, "job_id", r.job.ID, "error", err)
	}
	base, err := r.c.UpstreamBase(ctx, r.ws)
	if errors.Is(err, ErrNoUpstreamBase) {
		return attemptFailed(attemptRetryable, reasonUpstreamUnknown, err)
	}
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, err)
	}
	r.base, r.result.UpstreamHeadSHA = base, &base
	return r.store(ctx)
}

// readWorkspace reads the job's workspace and ends the attempt unless it is
// ready: an archived or failed workspace fails the job for good, one that
// is being cloned fails the attempt, and one that no longer exists ends it.
func (r *rebuildRun) readWorkspace(ctx context.Context) *attemptOutcome {
	if r.c.d.Store == nil {
		return attemptFailed(attemptRetryable, reasonGitError, errNotBound)
	}
	ws, found, err := workspace.BySlug(ctx, r.c.d.Store.Main(), r.job.Workspace)
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("read the workspace %s: %w", r.job.Workspace, err))
	}
	if !found {
		return &attemptOutcome{kind: attemptEnded}
	}
	r.ws = ws
	if r.payload.IntegrationBranch == "" {
		// The job holds the name its enqueue read; one without it was queued
		// before the name was kept.
		r.payload.IntegrationBranch = IntegrationBranch(ws)
	}
	switch ws.Status {
	case workspace.StatusReady:
		return nil
	case workspace.StatusArchived:
		return attemptFailed(attemptPermanent, reasonWorkspaceArchived, fmt.Errorf("rebuild of %s: the workspace is archived", ws.Slug))
	case workspace.StatusPending, workspace.StatusCloning:
		return attemptFailed(attemptRetryable, reasonWorkspaceBusy, fmt.Errorf("rebuild of %s: the workspace is %s", ws.Slug, ws.Status))
	default:
		return attemptFailed(attemptPermanent, reasonWorkspaceNotReady, fmt.Errorf("rebuild of %s: the workspace is %s, not ready", ws.Slug, ws.Status))
	}
}

// store replaces the job's result with the attempt's. A job that is no
// longer running was deleted with its workspace, which ends the attempt.
func (r *rebuildRun) store(ctx context.Context) *attemptOutcome {
	if r.c.d.Jobs == nil {
		return attemptFailed(attemptRetryable, reasonGitError, errNotBound)
	}
	results, err := json.Marshal(r.results)
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, err)
	}
	r.result.PatchResults = results
	doc, err := json.Marshal(r.result)
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, err)
	}
	err = r.c.d.Jobs.SetResult(ctx, r.job.ID, doc)
	if errors.Is(err, jobs.ErrNotRunning) {
		return &attemptOutcome{kind: attemptEnded}
	}
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("store the result of the rebuild job %s: %w", r.job.ID, err))
	}
	return nil
}
