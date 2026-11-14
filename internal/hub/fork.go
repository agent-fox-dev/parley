package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/fork"
)

// forksHook is the name of the start hook that binds the fork component.
const forksHook = "forks"

// composeForks adds the task forks: the fork fields of the workspace
// records, after the workspace part's, and the fork view of the workspace
// service, through which every workspace object of a fork shows the head
// the fork component computes from the parent's repository. Its start hook
// binds the component to the stores, the workspace service, which knows
// where the parents' and the forks' repositories are, and the hub's git.
// The component exists from the composition on, so a later part keeps it
// through Composition.Forks.
func composeForks(c *Composition) {
	comp := &fork.Component{}
	c.forks = comp
	c.AddSchemaStep(fork.SchemaStep)
	c.workspaces.RegisterForkView(comp.WorkspaceView)
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.AddStartHook(forksHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := fork.Deps{Store: rt.Store, Workspaces: c.workspaces, Git: git, Events: c.Events(), Access: c.access,
			Limits: rt.Config.Forks, Now: rt.Now, CreateRepo: git.CreateFork, Transact: rt.Store.Tx}
		for _, adjust := range c.forkAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// Forks returns the hub's fork component: the ref view of a fork, its head
// and the check of a write to one of its branches. A part keeps it when it
// is composed; the component serves once its start hook has run.
func (c *Composition) Forks() *fork.Component { return c.forks }

// AdjustForks adds fn to the functions that change the fork component's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added. Tests replace a collaborator with it,
// such as the name generator or the repository creator.
func (c *Composition) AdjustForks(fn func(*fork.Deps)) {
	c.forkAdjusters = append(c.forkAdjusters, fn)
}
