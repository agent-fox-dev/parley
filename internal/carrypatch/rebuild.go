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

// The reasons of a failed attempt. A workspace that is not ready reuses
// the names of the workspace routes' refusals.
const (
	reasonConflict            = "conflict"
	reasonUpstreamFetchFailed = "upstream_fetch_failed"
	reasonUpstreamUnknown     = "upstream_unknown"
	reasonGitError            = "git_error"
	reasonTimeout             = "timeout"
	reasonShutdown            = "shutdown"
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
	// conflict is the conflict report of the patch a fail_fast run stopped
	// at, nil for any other outcome.
	conflict json.RawMessage
	// secrets are the values of the credentials the attempt read, which
	// must not reach the job's error or an event.
	secrets []string
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
// with each and the head under construction; once it published, how many
// patches the publish removed as merged upstream.
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
	removed int

	// secrets are the values of the workspace's upstream and origin
	// credentials as the attempt read them.
	secrets []string
}

// runRebuildAttempt runs one attempt of the rebuild job j, which the queue
// has set running, and returns its outcome; it never ends the process. The
// attempt reads the workspace first and goes on only when it is ready; it
// then empties the job's result, fetches upstream and stores the upstream
// base it starts from. It lists the patches with the tips of their
// branches, builds the new head from the base and those tips alone in a
// scratch working tree, and publishes it as the integration branch, which
// it pushes to origin when the workspace asks for it. Last, it stores the
// counts over what it did with the patches.
func (c *Component) runRebuildAttempt(ctx context.Context, j jobs.Job) attemptOutcome {
	r := &rebuildRun{c: c, job: j}
	// A payload that does not decode leaves the options at their defaults,
	// as the job's record shows them.
	_ = json.Unmarshal(j.Payload, &r.payload)
	out := attemptOutcome{kind: attemptCompleted}
	for _, step := range []func(context.Context) *attemptOutcome{r.start, r.listPatches, r.build, r.publish, r.push, r.complete} {
		if stop := step(ctx); stop != nil {
			out = *stop
			break
		}
	}
	out.secrets = r.secrets
	return out
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
	r.readSecrets(ctx)
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
		return r.failed(ctx, err)
	}
	r.base, r.result.UpstreamHeadSHA = base, &base
	return r.store(ctx)
}

// readSecrets keeps the values of the workspace's upstream and origin
// credentials, which the attempt presents to git, so that the text of its
// failure is cleaned of them whatever git wrote. A credential that cannot
// be read is not kept: the attempt cannot present it either.
func (r *rebuildRun) readSecrets(ctx context.Context) {
	if r.c.d.Secrets == nil {
		return
	}
	upstream, err := ResolveUpstreamCredential(ctx, r.c.d.Secrets, r.ws.Slug)
	if err == nil && upstream.Password != "" {
		r.secrets = append(r.secrets, upstream.Password)
	}
	origin, err := workspace.ResolveOriginCredential(ctx, r.c.d.Secrets, r.ws.Slug)
	if err == nil && origin.Password != "" {
		r.secrets = append(r.secrets, origin.Password)
	}
}

// complete stores, on the job's result, the counts over the patch results
// and the number of patches the publish removed, before the attempt
// returns its completed outcome to the queue.
func (r *rebuildRun) complete(ctx context.Context) *attemptOutcome {
	r.result.PatchesApplied, r.result.PatchesSkipped, r.result.PatchesConflicted = 0, 0, 0
	for _, res := range r.results {
		switch res.Status {
		case resultSuccess:
			r.result.PatchesApplied++
		case resultSkipped:
			r.result.PatchesSkipped++
		case resultConflict:
			r.result.PatchesConflicted++
		}
	}
	r.result.PatchesRemoved = r.removed
	return r.store(ctx)
}

// readWorkspace reads the job's workspace and ends the attempt unless it is
// ready: an archived or failed workspace fails the job for good, one that
// is being cloned fails the attempt, and one that no longer exists ends it.
// The attempt reads it at its start, before every patch and just before
// the publish's swap.
func (r *rebuildRun) readWorkspace(ctx context.Context) *attemptOutcome {
	if r.c.d.Store == nil {
		return attemptFailed(attemptRetryable, reasonGitError, errNotBound)
	}
	// A delete of the workspace cancels the attempt with its job: the read
	// must still tell that the workspace is gone.
	ws, found, err := workspace.BySlug(context.WithoutCancel(ctx), r.c.d.Store.Main(), r.job.Workspace)
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("read the workspace %s: %w", r.job.Workspace, err))
	}
	return r.admit(ws, found)
}

// admit keeps ws, the job's workspace as just read, found false when it no
// longer exists, and ends the attempt unless it is ready, as readWorkspace
// says.
func (r *rebuildRun) admit(ws workspace.Workspace, found bool) *attemptOutcome {
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
		return r.failed(ctx, fmt.Errorf("store the result of the rebuild job %s: %w", r.job.ID, err))
	}
	return nil
}

// failed is the outcome of an attempt whose git operation or store access
// failed with err, not for a conflict: a retryable failure, shutdown when
// the attempt's context was cancelled, as at a stop of the hub, timeout for
// a git operation that outlasted its deadline, git_error otherwise. A
// workspace deleted meanwhile, which cancels its jobs' attempts too, ends
// the attempt instead.
func (r *rebuildRun) failed(ctx context.Context, err error) *attemptOutcome {
	switch {
	case ctx.Err() != nil:
		if out := r.readWorkspace(ctx); out != nil && out.kind == attemptEnded {
			return out
		}
		return attemptFailed(attemptRetryable, reasonShutdown, err)
	case errors.Is(err, ErrGitTimeout):
		return attemptFailed(attemptRetryable, reasonTimeout, err)
	}
	return attemptFailed(attemptRetryable, reasonGitError, err)
}
