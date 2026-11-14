package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/fork"
)

// forksHook is the name of the start hook that binds the fork component.
const forksHook = "forks"

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
func composeForks(c *Composition) {
	comp := &fork.Component{}
	c.forks = comp
	c.AddSchemaStep(fork.SchemaStep)
	c.AddSchemaStep(fork.DiscardStep)
	c.workspaces.Teardown().RegisterForkRemover(comp)
	c.workspaces.RegisterForkView(comp.WorkspaceView)
	c.workspaces.RegisterForkField("ttl", comp.TTLField)
	c.workspaces.RegisterForkField("purpose", fork.PurposeField)
	c.workspaces.RegisterForkField("auto_change", fork.AutoChangeField)
	c.workspaces.RegisterForkUpdateGuard(fork.GuardUpdate)
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
