package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/jobs"
)

// jobsHook is the name of the start hook that binds the job queue and
// recovers the jobs a stop interrupted; jobsWork is the name of the
// background work that runs its dispatcher.
const (
	jobsHook = "jobs"
	jobsWork = "jobs"
)

// composeJobs adds the hub's durable job queue: the schema of the job
// table, and the queue, which its start hook binds to the store and to the
// hub's clock and logger, after every adjuster AdjustJobs added has changed
// those dependencies, and which then returns the jobs left running by the
// last stop to queued. The start hooks all run before the background work
// starts, so the recovery is done before the dispatcher's first pass. The
// queue removes a workspace's jobs in the transaction of its delete, as a
// remover of owned data (ADR 07), registered after the workspace part's. The
// dispatcher runs as background work with the queue's own grace: a stop
// lets the attempts that run end within it and settles them, and leaves
// those still running then for the next start's recovery.
//
// The queue exists from the composition on (Jobs), so that a part
// registers the handler of its job type while the hub is composed, and a
// test does so from Options.Compose; tests change its dependencies (its
// clock, its interval, its wake) with AdjustJobs.
func composeJobs(c *Composition) {
	q := &jobs.Queue{}
	c.jobs = q
	c.AddSchemaStep(jobs.SchemaStep)
	c.workspaces.Teardown().RegisterRemover(q.RemoveWorkspace)
	c.AddStartHook(jobsHook, func(ctx context.Context, rt *Runtime) error {
		d := jobs.Deps{Store: rt.Store, Clock: jobs.SystemClock(rt.Now), Logger: rt.Logger}
		for _, adjust := range c.jobsAdjusters {
			adjust(&d)
		}
		q.Bind(d)
		return q.Recover(ctx)
	})
	c.AddBackgroundWork(jobsWork, jobs.ShutdownGrace, func(ctx context.Context, _ *Runtime) { q.Run(ctx) })
}

// Jobs returns the hub's job queue, which runs jobs once its start hook has
// bound it.
func (c *Composition) Jobs() *jobs.Queue { return c.jobs }

// AdjustJobs adds fn to the functions that change the job queue's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added.
func (c *Composition) AdjustJobs(fn func(*jobs.Deps)) {
	c.jobsAdjusters = append(c.jobsAdjusters, fn)
}
