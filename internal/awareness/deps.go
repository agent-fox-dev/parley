// Package awareness keeps the hub's picture of who works on which files: an
// index of the draft and open changes of each workspace and target branch
// by the files they touch, which a worker brings up to date from the event
// log after each new version, retarget, land, abandon and restore, without
// delaying the request that caused it.
//
// The package needs three things of others, which it defines as narrow
// interfaces that the hub wires: the event log it follows (EventSource),
// the change records it reads (ChangeReader), and the repositories it lists
// a version's files in (Deps.Repos).
package awareness

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/store"
)

// DefaultIndexInterval is how long the index worker waits between two
// looks at the event log when Deps.IndexInterval is zero.
const DefaultIndexInterval = time.Second

// Querier is what a read goes through: the store's *sql.DB or the caller's
// open *sql.Tx, so that a read inside a transaction sees what it wrote.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// EventSource is the event log the index worker follows: the events with a
// seq higher than after, ascending, at most limit of them, and the position
// the read examined. A position the log no longer holds the events after
// fails with an error matching event.ErrPositionExpired.
type EventSource interface {
	ReadAfter(ctx context.Context, after int64, limit int) ([]event.Record, int64, error)
}

// ChangeReader reads one change of a workspace by its number, through q
// (the caller's transaction, or nil for the store). A change the workspace
// does not have is ErrChangeNotFound.
type ChangeReader interface {
	ReadChange(ctx context.Context, q Querier, workspace string, number int64) (Change, error)
}

// ErrChangeNotFound is the error of a ChangeReader for a change that does
// not exist.
var ErrChangeNotFound = errors.New("no such change")

// Change is a change as the awareness reads it: its state, where it goes,
// its current version and how it landed, and the members of its summary.
type Change struct {
	Workspace    string
	Number       int64
	Ref          string
	Title        string
	Intent       string
	State        string
	TargetBranch string
	Version      int64
	HeadSHA      string
	// BaseSHA is the commit the version is measured from; nil when it has
	// none, and the version is measured from the empty tree.
	BaseSHA   *string
	DependsOn *int64
	Author    Actor
	SessionID *string
	Mergeable string
	// LandingStatus is the status of the change's landing.
	LandingStatus string
	// Landed is how the change landed, nil while it has not.
	Landed    *Landed
	UpdatedAt time.Time
}

// Actor is who did something: the credential type (or system), the user's
// id and username.
type Actor struct {
	ActorType string
	ActorID   *string
	Username  *string
}

// Landed is the commit a change's landing made the target's head, and
// when.
type Landed struct {
	SHA string
	At  time.Time
}

// The states of a change the index acts on.
const (
	StateDraft     = "draft"
	StateOpen      = "open"
	StateLanded    = "landed"
	StateAbandoned = "abandoned"
)

// Deps are what the component needs from the hub.
type Deps struct {
	// Store holds the index and the worker's position.
	Store *store.Store
	// Events is the event log the index worker follows.
	Events EventSource
	// Changes reads the change records the events name.
	Changes ChangeReader
	// Repos opens the repository of the workspace slug, where a change's
	// versions are listed.
	Repos func(ctx context.Context, slug string) (*gitrepo.Repo, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger is told of what fails and stops nothing, such as a version
	// whose files cannot be listed; nil discards it.
	Logger *slog.Logger
	// IndexInterval is the time between two looks of the index worker at
	// the event log; zero is DefaultIndexInterval.
	IndexInterval time.Duration
}

// Component is the awareness feature: the index of the changes by touched
// file and the worker that keeps it.
type Component struct {
	deps Deps

	// pass is held by a worker pass, so that two passes never process the
	// same event at once.
	pass sync.Mutex
	// beforeCommit, when set, is called at the end of an event's
	// transaction, and its error rolls the transaction back; only tests set
	// it.
	beforeCommit func() error
}

// errNotBound is the error of a component used before Bind.
var errNotBound = errors.New("awareness: the component is not bound")

// Bind gives the component its dependencies, the defaults in place of the
// members d leaves zero.
func (c *Component) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.IndexInterval <= 0 {
		d.IndexInterval = DefaultIndexInterval
	}
	c.pass.Lock()
	defer c.pass.Unlock()
	c.deps = d
}

// querier is q, or the store for nil.
func (c *Component) querier(q Querier) Querier {
	if q == nil {
		return c.deps.Store.Main()
	}
	return q
}
