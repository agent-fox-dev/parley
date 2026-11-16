// Package land lands changes: it records every request to land a change,
// or a stack of changes, as a durable land attempt, lands an attempt by
// replaying its changes onto the head of the target branch and moving the
// target with a compare-and-swap recorded together with the land, and
// serves the attempts read-only per change, per workspace and by id. The
// attempts run on a pool of a bounded number of workers, each under a
// deadline.
//
// The package plugs into the hub through internal/hub/land.go, which adds
// its schema step, routes, start hooks and the pool as background work,
// and binds the component to its dependencies there
// (docs/adr/01-compose-the-hub-in-one-package.md).
package land

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/store"
)

// The statuses of a land attempt: running until it ends in one of the four
// final statuses, which never change again.
const (
	StatusRunning  = "running"
	StatusLanded   = "landed"
	StatusConflict = "conflict"
	StatusBlocked  = "blocked"
	StatusFailed   = "failed"
)

// statuses are every status of an attempt, in the order the schema and
// the list's filter name them.
var statuses = []string{StatusRunning, StatusLanded, StatusConflict, StatusBlocked, StatusFailed}

// The triggers of an attempt: a caller's request, or the automatic landing
// of a change marked auto_land.
const (
	TriggerManual   = "manual"
	TriggerAutoLand = "auto_land"
)

// The strategies an attempt lands with, read once from the workspace's
// land.strategy setting when the attempt is created.
const (
	StrategyRebase = "rebase"
	StrategySquash = "squash"
)

// The values of the error of a failed attempt.
const (
	ErrorContention     = "contention"
	ErrorTimeout        = "timeout"
	ErrorOriginRejected = "origin_rejected"
	ErrorInternal       = "internal"
)

// Querier is what a read goes through: the store's *sql.DB or the caller's
// open *sql.Tx, so that a read inside a transaction sees what it wrote.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Workspace is a workspace as a land reads it: the facts the ref writer
// takes about the target, whether it may be landed on now, and its
// effective land settings.
type Workspace struct {
	Slug, Kind, Parent, DefaultBranch, Mode string
	// Ready is nil for a workspace a change may land in, and otherwise
	// its refusal, such as 409 workspace_archived.
	Ready error
	// Strategy, ByAuthor and DeleteSource are the effective land.strategy,
	// land.by_author and land.delete_source settings.
	Strategy               string
	ByAuthor, DeleteSource bool
}

// WorkspaceReader reads a workspace record through q, the caller's
// transaction or nil for the store; found is false for a slug no
// workspace has.
type WorkspaceReader interface {
	Workspace(ctx context.Context, q Querier, slug string) (w Workspace, found bool, err error)
}

// AccessRule says whether the user may write to the workspace slug.
type AccessRule interface {
	CanWrite(ctx context.Context, userID, slug string) (bool, error)
}

// EventWriter takes the land events in the caller's transaction.
type EventWriter interface {
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

// RefWriter is the ref writer a land moves its target through, so that the
// move is journalled and its push event written like every other write of
// refs (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
type RefWriter interface {
	Apply(ctx context.Context, req pushrule.ApplyRequest) (pushrule.Result, error)
	Finish(ctx context.Context, res pushrule.Result)
}

// Deps are what the component needs. Store and Changes are required;
// every other member has a default or is needed only once attempts run.
type Deps struct {
	// Store is the hub's store, which holds the attempts.
	Store *store.Store
	// Repos opens the repository of the workspace slug.
	Repos func(ctx context.Context, slug string) (*gitrepo.Repo, error)
	// Changes is the change component, which reads and lands changes.
	Changes *change.Component
	// Workspaces reads the workspace records.
	Workspaces WorkspaceReader
	// Access is the access rule asked who may land.
	Access AccessRule
	// Events takes the land events.
	Events EventWriter
	// RefWriter moves the target branch of a land.
	RefWriter RefWriter
	// Now is the clock of the stored times; nil is time.Now.
	Now func() time.Time
	// Logger is told of what fails and changes nothing; nil discards it.
	Logger *slog.Logger
	// Committer is the landing identity, the committer of every commit a
	// land makes; a member left empty is the default's, Parley
	// <parley@localhost>.
	Committer Committer
	// MaxAttempts is how many times an attempt tries to move the target
	// before it ends failed contention; 0 is DefaultMaxAttempts.
	MaxAttempts int
	// Workers is how many attempts the pool computes at once; 0 is
	// DefaultWorkers.
	Workers int
	// Timeout is how long an attempt may take from its creation, or from
	// its re-run at a start, before it ends failed timeout; 0 is
	// DefaultTimeout.
	Timeout time.Duration
	// Paused makes the pool hold every attempt handed to it: none runs and
	// none ends, as when the hub stops right after taking it, so that a
	// test keeps attempts running across a restart.
	Paused bool
}

// DefaultMaxAttempts is how many times an attempt tries to move the target
// when the hub names no limit.
const DefaultMaxAttempts = 10

// Committer is a name and e-mail address commits are made by.
type Committer struct {
	Name, Email string
}

// The landing identity of a hub that names none.
const (
	DefaultCommitterName  = "Parley"
	DefaultCommitterEmail = "parley@localhost"
)

// Component keeps the land attempts. Its zero value is ready to be
// registered; it serves once Bind has given it its dependencies, before
// the first request.
type Component struct {
	deps Deps

	mu       sync.RWMutex
	gate     Gate
	override OverrideRule

	pool  pool
	seams seams
}

// seams are where a test steers the engine; their zero value does
// nothing.
type seams struct {
	beforeSwap func(ctx context.Context, id string, try int)
	fault      func(point string) error
}

// Bind gives the component its dependencies, the defaults in place of the
// members d leaves nil.
func (c *Component) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Committer.Name == "" {
		d.Committer.Name = DefaultCommitterName
	}
	if d.Committer.Email == "" {
		d.Committer.Email = DefaultCommitterEmail
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = DefaultMaxAttempts
	}
	if d.Workers <= 0 {
		d.Workers = DefaultWorkers
	}
	if d.Timeout <= 0 {
		d.Timeout = DefaultTimeout
	}
	c.deps = d
	c.pool.jobs = map[string]*job{}
	c.pool.slots = make(chan struct{}, d.Workers)
}
