package fork

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Events is the event component as the fork component uses it: it writes an
// event in the caller's transaction. *event.Component satisfies it.
type Events interface {
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

// Minter mints a token bound to a workspace in the caller's transaction,
// with its parley.token.bind event. *access.Service satisfies it.
type Minter interface {
	MintBound(ctx context.Context, tx *sql.Tx, in access.MintInput) (identity.PAT, string, error)
}

// ReadableSets builds the set of workspaces a caller can read, with the
// caller's level on each, cut to the binding's covered set for a bound
// token. *access.Service satisfies it.
type ReadableSets interface {
	Readable(ctx context.Context, c access.Caller) (access.ReadableSet, error)
}

// Deps are what the fork component works with. The hub builds them in its
// start hook; a test changes them through the hub's composition before they
// are bound.
type Deps struct {
	// Store holds the fork records, in the workspace table.
	Store *store.Store
	// Workspaces is the workspace service, which knows where the
	// repository of every workspace, parent or fork, is (RepoPath), and
	// renders the workspace object of a fork.
	Workspaces *workspace.Service
	// Git runs git on the parents' and the forks' repositories.
	Git gitrepo.Git
	// Events writes the parley.workspace.fork event of a create.
	Events Events
	// Access mints the token a create asks for, bound to the fork.
	Access Minter
	// Readable confines the fork list to the forks the caller can read and
	// gives the caller's level on each.
	Readable ReadableSets
	// Limits are [forks]: the lifetime of a new fork and how many forks a
	// workspace may have.
	Limits config.Forks
	// Now is the hub's clock; nil means time.Now.
	Now func() time.Time
	// NewName draws the name of a fork created without one; nil means 8
	// characters from lower-case letters and digits, drawn from a
	// cryptographic random source.
	NewName func() string
	// CreateRepo makes the repository of a new fork at forkDir, sharing the
	// history of the parent's repository at parentDir; nil means
	// Git.CreateFork. A test makes it fail to see the create clean up.
	CreateRepo func(ctx context.Context, parentDir, forkDir string) error
	// Transact runs fn in one store transaction, committed when fn returns
	// nil; nil means Store.Tx. A test makes the commit fail to see the
	// create clean up.
	Transact func(ctx context.Context, fn func(*sql.Tx) error) error
	// RemoveRepo removes the repository of a removed fork at dir, with its
	// own branches; nil means gitrepo.Remove. A test makes it fail to see
	// the discard recorded for later.
	RemoveRepo func(dir string) error
	// Logger logs what a removal cannot report to its caller, such as a
	// discard that failed or a fork the sweep could not remove; nil
	// discards it.
	Logger *slog.Logger
	// SweepTicks starts the ticker of the sweep and returns its ticks and
	// the function that stops it; nil means a time.Ticker of
	// SweepInterval. A test replaces it to drive the sweep.
	SweepTicks func() (<-chan time.Time, func())
}

// Component is the fork feature of one hub. The hub uses its zero value and
// binds it once at start with Bind, before it serves a request.
type Component struct {
	deps Deps
	// createLocks holds the create lock of each parent slug a create holds
	// or waits for, held by a create of a fork of that parent from its name
	// check to its commit; locksMu guards the map and the counts in it.
	locksMu     sync.Mutex
	createLocks map[string]*parentLock
	// linker and sandbox are the collaborators other features register,
	// nil while none is: no session exists, and sandboxes are disabled.
	linker  SessionLinker
	sandbox SandboxCreator
	// sandboxes is the sandbox lifecycle a removal asks, nil while none is
	// registered: a fork then has no sandboxes.
	sandboxes SandboxLifecycle
	// passMu is held by the sweep pass that runs, so that two never run at
	// once.
	passMu sync.Mutex
}

// Bind binds the component to d. It is called once, before the hub serves.
func (c *Component) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.NewName == nil {
		d.NewName = randomName
	}
	if d.CreateRepo == nil {
		d.CreateRepo = d.Git.CreateFork
	}
	if d.Transact == nil && d.Store != nil {
		d.Transact = d.Store.Tx
	}
	if d.RemoveRepo == nil {
		d.RemoveRepo = gitrepo.Remove
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.SweepTicks == nil {
		d.SweepTicks = realTicks
	}
	c.deps = d
}
