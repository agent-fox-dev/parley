package hub

import (
	"context"
	"time"

	"github.com/agent-fox-dev/parley/internal/fork"
)

const (
	// forksHook is the name of the start hook that binds the fork
	// component.
	forksHook = "forks"
	// forkSweepWork is the name of the background work that runs the fork
	// sweep, and forkExpiryTask that of its maintenance task, under which
	// package maintenance orders it.
	forkSweepWork  = "fork_sweep"
	forkExpiryTask = "fork_expiry"
	// forkSweepGrace is how long the stop waits for the sweep: a removal
	// that has begun runs to its commit or rollback, and its discard.
	forkSweepGrace = 10 * time.Second
)

// composeForks adds the task forks: the fork fields of the workspace
// records, after the workspace part's; the fork view of the workspace
// service, through which every workspace object of a fork shows the head
// the fork component computes from the parent's repository; and, in an
// update of a fork, the handlers of ttl, purpose and auto_change and the
// guard that refuses every update of a fork whose parent is not ready; the
// handler of ttl is a method of the component, which reads the hub's clock
// and the [forks] limits once bound. Its start hook
// binds the component to the stores, the workspace service, which knows
// where the parents' and the forks' repositories are, the hub's git, and the
// access rule, which mints a fork's token and confines the fork list to what
// the caller can read.
// The component exists from the composition on, so a later part keeps it
// through Composition.Forks, and registers there, while it is composed, the
// collaborators the create asks: the session feature its session-linking
// rule (RegisterSessionLinker) and the sandbox feature its sandbox component
// (RegisterSandbox), each from its own file of this package, as ADR 07 has
// the teardown collaborators registered on the workspace service. None is
// registered here: without them every session_id is refused 400
// unknown_session and every sandbox 404 sandboxes_disabled.
//
// The component is also the one place forks are removed: it is the fork
// remover of the workspace service's teardown set, so that DELETE
// /workspaces/:slug of a fork and the delete of a parent reach it, and it
// removes what a fork owns through that set's RemoveOwned. The sandbox
// feature registers its sandbox lifecycle on it (RegisterSandboxes) from
// its own file; none is registered here, so a fork has no sandboxes to
// delete. The pending discards of removed forks' repositories are a table
// of their own, after the fork fields.
//
// The sweep that removes the forks whose lifetime has ended runs as
// background work once the component is bound, one pass at once and one
// every minute, and stops at the stop before the stores close; each pass of
// the maintenance loop runs one more as the task fork_expiry. The
// component never runs two passes at once.
//
// While [forks].expire_on_land is set, a pass also removes a fork a day
// after its work has landed. It asks the change feature for the fork's
// changes through the change reader that feature registers on the
// component (RegisterChangeReader) from its own file; the hub has no change
// feature yet, so none is registered here, and no fork is removed before
// its expires_at. The day counts from the fork's last activity, a column of
// its own after the pending discards. The features that see an activity (a
// push to the fork, an API write to it, a change of it opened, versioned,
// landed, abandoned or restored: the push rules, the change feature and
// the landing) register the component's RecordActivity with their own
// activity hook in this function. The ref writer's is registered here: the
// writer tells the component of every accepted write to a fork through git
// or the source control API, after the refs moved and outside any store
// transaction, since RecordActivity opens its own. The change feature and
// the landing are not in the hub yet, so theirs are not.
func composeForks(c *Composition) {
	comp := &fork.Component{}
	c.forks = comp
	c.AddSchemaStep(fork.SchemaStep)
	c.AddSchemaStep(fork.DiscardStep)
	c.AddSchemaStep(fork.ActivityStep)
	c.workspaces.Teardown().RegisterForkRemover(comp)
	c.workspaces.RegisterForkView(comp.WorkspaceView)
	c.workspaces.RegisterForkField("ttl", comp.TTLField)
	c.workspaces.RegisterForkField("purpose", fork.PurposeField)
	c.workspaces.RegisterForkField("auto_change", fork.AutoChangeField)
	c.workspaces.RegisterForkUpdateGuard(fork.GuardUpdate)
	c.refWriter.RegisterForkActivity(comp)
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.AddStartHook(forksHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := fork.Deps{Store: rt.Store, Workspaces: c.workspaces, Git: git, Events: c.Events(), Access: c.access,
			Readable: c.access, Limits: rt.Config.Forks, Now: rt.Now, CreateRepo: git.CreateFork, Transact: rt.Store.Tx,
			Logger: rt.Logger}
		for _, adjust := range c.forkAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
	c.AddBackgroundWork(forkSweepWork, forkSweepGrace, func(ctx context.Context, _ *Runtime) { comp.RunSweep(ctx) })
	c.AddMaintenanceTask(forkExpiryTask, func(ctx context.Context, _ *Runtime) error {
		comp.RunPass(ctx)
		return nil
	})
}

// Forks returns the hub's fork component: the ref view of a fork, its head,
// the check of a write to one of its branches, and the registration points
// of the create's session linker and sandbox component. A part keeps it
// when it is composed and registers its collaborators then; the component
// serves once its start hook has run.
func (c *Composition) Forks() *fork.Component { return c.forks }

// AdjustForks adds fn to the functions that change the fork component's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added. Tests replace a collaborator with it,
// such as the name generator or the repository creator.
func (c *Composition) AdjustForks(fn func(*fork.Deps)) {
	c.forkAdjusters = append(c.forkAdjusters, fn)
}
