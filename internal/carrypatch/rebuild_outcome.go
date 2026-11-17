package carrypatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/scrub"
)

// The events of the outcome of a rebuild job's attempt.
const (
	EventRebuildComplete = "parley.rebuild.complete"
	EventRebuildFail     = "parley.rebuild.fail"
)

// RebuildHandler is the handler of the rebuild jobs of the hub's job queue
// (the hub registers it for RebuildJobType). Its Run makes one attempt of a
// rebuild job and returns its outcome to the queue: nil when the job is
// completed, the failure's text as a retryable error, marked permanent for
// a failure no retry can cure, and with the conflict report as its detail
// when the run stopped at a conflict, so that the queue's settlement
// stores them as the job's error and conflict. Its Settled writes, in the
// settlement's transaction, parley.rebuild.complete or parley.rebuild.fail,
// and parley.rebuild.followup when the attempt queued a follow-up, so that
// the events and the job's new status are seen together or not at all.
func (c *Component) RebuildHandler() jobs.Handler { return rebuildHandler{c} }

// rebuildHandler runs the attempts of rebuild jobs with its component,
// which keeps the outcome of each attempt for its settlement.
type rebuildHandler struct{ c *Component }

func (h rebuildHandler) Run(ctx context.Context, j jobs.Job) error {
	_, err := h.run(ctx, j)
	return err
}

// run runs one attempt of j, keeps its outcome for the settlement and
// returns it with the error the queue settles it with. The failure's text
// and reason are scrubbed first, with the values of the credentials the
// attempt read, which the kept outcome then leaves out: the outcome of a
// job deleted before its settlement stays kept, and must hold no secret.
// An attempt whose workspace was deleted with its jobs keeps nothing, as
// no settlement will find the job.
func (h rebuildHandler) run(ctx context.Context, j jobs.Job) (attemptOutcome, error) {
	// An earlier attempt's outcome the queue never settled (its job was
	// deleted in between) does not stand for this attempt, which may panic.
	h.c.takeOutcome(j.ID)
	out := h.c.runRebuildAttempt(ctx, j)
	if out.kind == attemptEnded {
		return out, fmt.Errorf("rebuild of %s ended: the workspace no longer exists", j.Workspace)
	}
	out.text = h.c.scrub(out.text, out.secrets...)
	out.reason = h.c.scrub(out.reason, out.secrets...)
	out.secrets = nil
	h.c.keepOutcome(j.ID, out)
	err := errors.New(out.text)
	switch out.kind {
	case attemptCompleted:
		return out, nil
	case attemptPermanent:
		err = jobs.Permanent(err)
	}
	if out.conflict != nil {
		err = jobs.WithDetail(err, out.conflict)
	}
	return out, err
}

// Settled emits, in tx, the event of the outcome the settlement recorded
// for j: parley.rebuild.complete with the heads and counts of the job's
// result when it is completed, else parley.rebuild.fail with the reason of
// the attempt, whether the job will be retried, its retry count after the
// settlement and the conflict report it stopped at. A settlement for which
// no outcome was kept, that of an attempt that panicked, fails with reason
// git_error. When the attempt queued a follow-up rebuild, it also emits
// parley.rebuild.followup with the follow-up's job id and the stale
// inputs that queued it. Every event is the system's, about the job, in
// the agent session the job was queued in.
func (h rebuildHandler) Settled(ctx context.Context, tx *sql.Tx, j jobs.Job, willRetry bool) error {
	out, kept := h.c.takeOutcome(j.ID)
	if h.c.d.Events == nil {
		return errNotBound
	}
	var p rebuildPayload
	if j.Payload != nil {
		_ = json.Unmarshal(j.Payload, &p)
	}
	slug := j.Workspace
	in := event.Input{
		Workspace: &slug, ActorType: event.ActorSystem,
		ResourceType: rebuildResource, ResourceID: j.ID, SessionID: p.SessionID,
	}
	if j.Status == jobs.StatusCompleted {
		var res rebuildResult
		if j.Result != nil && json.Unmarshal(j.Result, &res) != nil {
			res = rebuildResult{}
		}
		in.Type = EventRebuildComplete
		in.Metadata = map[string]any{
			"upstream_head_sha":             res.UpstreamHeadSHA,
			"integration_head_sha":          res.IntegrationHeadSHA,
			"previous_integration_head_sha": res.PreviousIntegrationHeadSHA,
			"patches_applied":               res.PatchesApplied,
			"patches_skipped":               res.PatchesSkipped,
			"patches_conflicted":            res.PatchesConflicted,
			"patches_removed":               res.PatchesRemoved,
		}
	} else {
		reason := reasonGitError
		if kept && out.reason != "" {
			reason = out.reason
		}
		var conflict any
		if reason == reasonConflict && j.Detail != nil {
			conflict = j.Detail
		}
		in.Type = EventRebuildFail
		in.Metadata = map[string]any{
			"reason":      reason,
			"will_retry":  willRetry,
			"retry_count": j.RetryCount,
			"conflict":    conflict,
		}
	}
	if _, err := h.c.d.Events.Emit(ctx, tx, in); err != nil {
		return err
	}
	if !kept || out.followUp == nil {
		return nil
	}
	in.Type = EventRebuildFollowUp
	in.Metadata = map[string]any{
		"follow_up_job_id": out.followUp.jobID,
		"stale_patches":    append([]string{}, out.followUp.stalePatches...),
		"upstream_stale":   out.followUp.upstreamStale,
	}
	_, err := h.c.d.Events.Emit(ctx, tx, in)
	return err
}

// keepOutcome keeps out, the outcome of the attempt of the job id that just
// ran, for the job's settlement.
func (c *Component) keepOutcome(id string, out attemptOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outcomes == nil {
		c.outcomes = map[string]attemptOutcome{}
	}
	c.outcomes[id] = out
}

// takeOutcome returns and forgets the kept outcome of the job id, false
// when none is kept.
func (c *Component) takeOutcome(id string) (attemptOutcome, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, ok := c.outcomes[id]
	delete(c.outcomes, id)
	return out, ok
}

// noSecrets scrubs for a component bound without the hub's scrubber.
var noSecrets = scrub.New()

// scrub is text without credentials: the hub's secret values and
// credential formats, the values given, and the user information of URLs.
func (c *Component) scrub(text string, values ...string) string {
	s := c.d.Scrubber
	if s == nil {
		s = noSecrets
	}
	return s.Scrub(text, values...)
}
