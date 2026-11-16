package carrypatch

import (
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Deps is what the component works with, given once by Bind.
type Deps struct {
	// Store holds the workspace records, whose integration_branch the
	// set-up writes and whose mode and state the guard reads, and the
	// patch lists.
	Store *store.Store
	// Secrets reads a workspace's secrets, among them its upstream and
	// origin credentials.
	Secrets workspace.SecretReader
	// Git runs git.
	Git gitrepo.Git
	// RepoPath is the git directory of the copy of the workspace slug.
	RepoPath func(slug string) string
	// UpstreamFetchDeadline, when positive, replaces DefaultGitDeadline as
	// the deadline of each git operation of an upstream fetch; only tests
	// set it.
	UpstreamFetchDeadline time.Duration
}

// Component holds the carry-patch operations of the hub. Its zero value
// is usable once Bind has given it its Deps.
type Component struct {
	d Deps

	// mu guards rebuilds, the registered rebuild enqueuer.
	mu       sync.Mutex
	rebuilds RebuildEnqueuer
}

// Bind gives the component its Deps, before its first use.
func (c *Component) Bind(d Deps) {
	c.d = d
}
