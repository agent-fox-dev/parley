package carrypatch

import (
	"log/slog"
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
	// Settings is the registry of the workspace settings, through which
	// ResolveBranch reads a workspace's patch_branch_source; nil reads it
	// as hub for every workspace.
	Settings *workspace.SettingsRegistry
	// BranchFetchDeadline, when positive, replaces
	// DefaultBranchFetchDeadline as the deadline of the single-branch fetch
	// from origin of ResolveBranch; only tests set it.
	BranchFetchDeadline time.Duration
	// BeforeBranchCreate, when set, is called by ResolveBranch with the
	// branch after its lookups and before it creates the branch; only tests
	// set it, to write the branch in between as another writer would.
	BeforeBranchCreate func(branch string)
	// Events writes the events of the patch list in the transaction of the
	// change they report.
	Events Emitter
	// Now is the clock of the patch list's times; nil is time.Now.
	Now func() time.Time
	// Logger receives what fails after an answer is decided, such as the
	// deletion of a removed patch's backup ref; nil discards it.
	Logger *slog.Logger
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

// now is the time of the component's clock.
func (c *Component) now() time.Time {
	if c.d.Now == nil {
		return time.Now()
	}
	return c.d.Now()
}

// logger is the component's logger, one that discards when none is bound.
func (c *Component) logger() *slog.Logger {
	if c.d.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.d.Logger
}
