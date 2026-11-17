package carrypatch

import (
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/jobs"
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
	// UpstreamFetchDeadline, when positive and shorter, replaces
	// DefaultGitDeadline as the one deadline of a whole upstream fetch;
	// only tests set it.
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
	// BeforeRefSwap, when set, is called by a reset to origin with the
	// workspace and the branch after it compared the branch's tip with
	// origin's and before it records the reset, writes the backup ref and
	// moves the branch, outside any store transaction; only tests set it,
	// to move the branch or change the patch in between as another writer
	// would.
	BeforeRefSwap func(slug, branch string)
	// CommitterName and CommitterEmail are the landing identity, the
	// committer of every commit a rebuild makes; empty, they are
	// DefaultCommitterName and DefaultCommitterEmail.
	CommitterName, CommitterEmail string
	// BeforeRebuildPatch, when set, is called by an attempt of the rebuild
	// job with the job's id and the index, from 0, of a patch of the run's
	// list before the run takes it up, outside any store transaction; only
	// tests set it, to look at the repository and the job between patches
	// or to change them as another writer would.
	BeforeRebuildPatch func(job string, i int)
	// BeforeRebuildPublish, when set, is called by an attempt of the
	// rebuild job with the job's id once every patch is processed and
	// before the publish reads the integration branch, outside any store
	// transaction; only tests set it.
	BeforeRebuildPublish func(job string)
	// Events writes the events of the patch list in the transaction of the
	// change they report.
	Events Emitter
	// Jobs is the hub's job queue, which holds the rebuild jobs.
	Jobs *jobs.Queue
	// Now is the clock of the patch list's times; nil is time.Now.
	Now func() time.Time
	// Logger receives what fails after an answer is decided, such as the
	// deletion of a removed patch's backup ref; nil discards it.
	Logger *slog.Logger
}

// The landing identity of a hub that names none, the same as the land
// component's.
const (
	DefaultCommitterName  = "Parley"
	DefaultCommitterEmail = "parley@localhost"
)

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
