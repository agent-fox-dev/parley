package carrypatch

import "context"

// RebuildOptions are what a source of a rebuild asks for.
type RebuildOptions struct {
	// Strategy and FailMode are the rebuild's strategy and fail mode; nil
	// leaves the workspace's settings to decide.
	Strategy *string
	FailMode *string
	// SubmittedBy names who or what submitted the rebuild, such as a
	// user's id or the hub feature that triggered it.
	SubmittedBy string
	// ActorType and ActorID are the actor of the job's events, as
	// event.Input takes them; SessionID is the agent session they belong
	// to, nil for none.
	ActorType string
	ActorID   *string
	SessionID *string
	// FollowUpOf is the id of the job this rebuild follows up, nil for
	// none.
	FollowUpOf *string
}

// RebuildResult is the answer of EnqueueRebuild: whether a job was queued
// and its id, or, when an equal job was waiting already, that job's id in
// ExistingJobID and no error.
type RebuildResult struct {
	Queued        bool
	JobID         string
	ExistingJobID string
}

// RebuildEnqueuer queues a rebuild of a carry-patch workspace's integration
// branch. Every source of a rebuild (a patch change, a rebuild's follow-up,
// a sync, a push) calls it; the feature that runs rebuilds implements it
// and registers it with RegisterRebuildEnqueuer when the hub is composed,
// so that those sources do not depend on that feature. Until then no
// rebuild is queued.
type RebuildEnqueuer interface {
	EnqueueRebuild(ctx context.Context, slug string, opts RebuildOptions) (RebuildResult, error)
}

// noRebuilds is the enqueuer of a hub without rebuilds: it queues nothing
// and reports no job.
type noRebuilds struct{}

func (noRebuilds) EnqueueRebuild(context.Context, string, RebuildOptions) (RebuildResult, error) {
	return RebuildResult{}, nil
}

// RegisterRebuildEnqueuer makes e the component's rebuild enqueuer,
// replacing the one registered before; nil restores the one that queues
// nothing. It is called while the hub is composed, before any request.
func (c *Component) RegisterRebuildEnqueuer(e RebuildEnqueuer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rebuilds = e
}

// Rebuilds is the component's rebuild enqueuer: the one registered, or one
// that queues nothing.
func (c *Component) Rebuilds() RebuildEnqueuer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rebuilds == nil {
		return noRebuilds{}
	}
	return c.rebuilds
}
