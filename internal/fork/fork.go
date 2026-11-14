package fork

import (
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Deps are what the fork component works with.
type Deps struct {
	// Store holds the fork records, in the workspace table.
	Store *store.Store
	// Workspaces is the workspace service, which knows where the
	// repository of every workspace, parent or fork, is (RepoPath).
	Workspaces *workspace.Service
	// Git runs git on the parents' and the forks' repositories.
	Git gitrepo.Git
}

// Component is the fork feature of one hub. The hub uses its zero value and
// binds it once at start with Bind, before it serves a request.
type Component struct {
	deps Deps
}

// Bind binds the component to d. It is called once, before the hub serves.
func (c *Component) Bind(d Deps) { c.deps = d }
