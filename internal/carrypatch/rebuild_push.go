package carrypatch

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// EventRebuildPushFailed is the event of a push of the integration branch
// to origin that failed after a rebuild published it.
const EventRebuildPushFailed = "parley.rebuild.push_failed"

// pushIntegrationBranchKey is the workspace setting that tells whether a
// rebuild pushes the integration branch it published to origin.
const pushIntegrationBranchKey = "rebuild_push_integration_branch"

// DefaultRebuildPushDeadline is the deadline of the push of the integration
// branch to origin after a rebuild's publish, the same as the mirror's push
// of a patch branch: a push that hangs must not hold the rebuild job.
const DefaultRebuildPushDeadline = 120 * time.Second

// push pushes the head the run published to the integration branch on
// origin when the workspace, read now, asks for it, and marks the result
// pushed. It is a forced push under an explicit lease, the commit the hub
// last saw on origin's branch or that origin has no such branch when the
// hub saw none, so that it never overwrites what another writer put there;
// once it succeeded, the pushed head is what the hub last saw. A push that
// fails does not fail the rebuild, whose publish stands: it is reported
// with parley.rebuild.push_failed. Only a stop of the hub, or a delete of
// the workspace, during the push ends the attempt as at any other step.
func (r *rebuildRun) push(ctx context.Context) *attemptOutcome {
	ws, found, err := workspace.BySlug(ctx, r.c.d.Store.Main(), r.job.Workspace)
	switch {
	case err == nil && !found:
		return &attemptOutcome{kind: attemptEnded}
	case err == nil:
		r.ws = ws
	case ctx.Err() != nil:
		return r.failed(ctx, err)
	default:
		// The workspace as the publish read it, a moment ago, decides.
		r.c.logger().Warn("rebuild could not read its workspace again before the push",
			"workspace", r.ws.Slug, "job_id", r.job.ID, "error", err)
	}
	if !r.c.pushIntegrationBranch(r.ws) {
		return nil
	}
	err = r.pushToOrigin(ctx)
	if err != nil && ctx.Err() != nil {
		return r.failed(ctx, err)
	}
	if err != nil {
		r.pushFailed(ctx, err)
		return nil
	}
	r.result.IntegrationBranchPushed = true
	return nil
}

// pushIntegrationBranch reports whether the effective
// rebuild_push_integration_branch setting of ws is on; off when the
// component knows no settings registry, the setting's default.
func (c *Component) pushIntegrationBranch(ws workspace.Workspace) bool {
	if c.d.Settings == nil {
		return false
	}
	on, _ := c.d.Settings.EffectiveSettings(ws.Settings, ws.DefaultBranch)[pushIntegrationBranchKey].(bool)
	return on
}

// pushToOrigin pushes the run's head to the integration branch on the
// workspace's origin with its origin credential, under the lease of the
// hub's refs/remotes/origin/<branch>, within the push's deadline, and then
// moves that ref to the head.
func (r *rebuildRun) pushToOrigin(ctx context.Context) error {
	branch := r.payload.IntegrationBranch
	if r.ws.GitURL == nil || *r.ws.GitURL == "" {
		return fmt.Errorf("push %s: the workspace %s has no origin", branch, r.ws.Slug)
	}
	cred, err := workspace.ResolveOriginCredential(ctx, r.c.d.Secrets, r.ws.Slug)
	if err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	var lease string
	err = r.git(ctx, func(ctx context.Context) error {
		id, found, err := r.repo.OriginHead(ctx, branch)
		if found {
			lease = id
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("push %s: read origin's head: %w", branch, err)
	}
	push := r.c.d.Push
	if push == nil {
		push = r.c.d.Git.Push
	}
	req := gitrepo.PushRequest{
		Dir: r.repo.Dir(), URL: *r.ws.GitURL, Credential: cred,
		Commit: r.head, Branch: branch, Force: true, Lease: lease,
	}
	err = RunGit(ctx, r.c.pushDeadline(), cred, func(ctx context.Context, _ time.Duration) error {
		_, err := push(ctx, req)
		return err
	})
	if err != nil {
		return err
	}
	err = r.git(ctx, func(ctx context.Context) error { return r.repo.SetOriginHead(ctx, branch, r.head) })
	if err != nil {
		// Origin holds the head: the push stands, and the next push's lease
		// meets the branch where the hub did not see it.
		r.c.logger().Warn("rebuild pushed the integration branch and could not record origin's head",
			"workspace", r.ws.Slug, "job_id", r.job.ID, "branch", branch, "error", err)
	}
	return nil
}

// pushDeadline is the deadline of the push of the integration branch:
// RebuildPushDeadline when it is positive and shorter, else
// DefaultRebuildPushDeadline.
func (c *Component) pushDeadline() time.Duration {
	if d := c.d.RebuildPushDeadline; d > 0 && d < DefaultRebuildPushDeadline {
		return d
	}
	return DefaultRebuildPushDeadline
}

// pushFailed writes parley.rebuild.push_failed for the push that failed
// with err, in a store transaction of its own, with the failure's text
// cleaned of the attempt's credentials as its reason. An event that cannot
// be written is logged: the rebuild completes all the same.
func (r *rebuildRun) pushFailed(ctx context.Context, err error) {
	slug := r.job.Workspace
	in := event.Input{
		Type: EventRebuildPushFailed, Workspace: &slug, ActorType: event.ActorSystem,
		ResourceType: rebuildResource, ResourceID: r.job.ID, SessionID: r.payload.SessionID,
		Metadata: map[string]any{
			"integration_branch": r.payload.IntegrationBranch,
			"reason":             r.c.scrub(err.Error(), r.secrets...),
		},
	}
	if r.c.d.Events == nil {
		return
	}
	werr := r.c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := r.c.d.Events.Emit(ctx, tx, in)
		return err
	})
	if werr != nil {
		r.c.logger().Warn("rebuild could not report its failed push",
			"workspace", slug, "job_id", r.job.ID, "error", werr)
	}
}
