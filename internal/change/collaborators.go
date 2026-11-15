package change

import (
	"context"
	"database/sql"
	"errors"

	"github.com/agent-fox-dev/parley/internal/event"
)

// Querier is what a read goes through: the store's *sql.DB or the caller's
// open *sql.Tx, so that a read inside a transaction sees what it wrote.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Workspace is a workspace as the component reads it.
type Workspace struct {
	Slug string
	// Fork is set for a task fork, whose parent is Parent.
	Fork   bool
	Parent string
	// CarryPatch is set for a carry-patch workspace, whose integration
	// branch is IntegrationBranch.
	CarryPatch        bool
	IntegrationBranch string
	// DefaultBranch is the workspace's default branch; a fork's is its
	// base branch.
	DefaultBranch string
	// Purpose and SessionID are a fork's, nil when it has none.
	Purpose, SessionID *string
	// RequireIntent is the effective changes.require_intent setting.
	RequireIntent bool
	// AutoRestack is the effective changes.auto_restack setting: false
	// leaves a restack of the workspace's changes to their callers, and
	// only flags a change that needs one.
	AutoRestack bool
	// Unusable is nil for a workspace a change may be opened in or from,
	// and otherwise its refusal, such as 409 workspace_busy.
	Unusable error
}

// WorkspaceReader reads the workspace records, which the workspace feature
// keeps.
type WorkspaceReader interface {
	// Workspace reads the workspace slug through q; found is false, with
	// a nil error, when there is none.
	Workspace(ctx context.Context, q Querier, slug string) (w Workspace, found bool, err error)
}

// EventWriter takes the change events in the caller's transaction; the
// hub's event component is one.
type EventWriter interface {
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

type discardEvents struct{}

func (discardEvents) Emit(context.Context, *sql.Tx, event.Input) (event.Record, error) {
	return event.Record{}, nil
}

// ErrUnknownSession is the answer of a SessionLinker's check that refuses a
// session id.
var ErrUnknownSession = errors.New("unknown session")

// SessionLinker is the session-linking rule and the session record, which
// the session feature keeps. Without one no session exists: an explicit
// session_id is refused, a defaulted one is dropped and nothing is
// recorded.
type SessionLinker interface {
	// Check judges the session id named for a change of the workspace
	// slug. It returns the id in lower case when it accepts it, an error
	// matching ErrUnknownSession when it refuses it, and any other error
	// when it cannot tell.
	Check(ctx context.Context, caller Caller, sessionID, workspace string) (string, error)
	// Record links the session to the version of the change number of the
	// workspace in tx, the transaction that records the version; its error
	// undoes the version.
	Record(ctx context.Context, tx *sql.Tx, sessionID, workspace string, number, version int64) error
}

// StackLinker keeps the stacks of changes, which the stack feature
// computes. Without one no change depends on another.
type StackLinker interface {
	// LinkVersion is told of every new version v of the change ch, in tx,
	// with the depends_on the request named (nil for none), at open and
	// for every later version.
	LinkVersion(ctx context.Context, tx *sql.Tx, ch Change, v Version, dependsOn *int64) error
	// SetDependsOn sets the change's depends_on to a number or, nil, to
	// none, as an update of the change asks.
	SetDependsOn(ctx context.Context, tx *sql.Tx, ch Change, dependsOn *int64) error
}

// LandingEvaluator answers the landing of a change that is not a draft,
// which the landing policy computes. Without one every such change is
// ready.
type LandingEvaluator interface {
	EvaluateLanding(ctx context.Context, tx *sql.Tx, ch Change, v Version) (Landing, error)
}

// VersionObserver is told of every new version, in tx, after the change's
// landing is stored, with the credential that created it (none for a
// version the hub made). Its error undoes the version.
type VersionObserver interface {
	VersionRecorded(ctx context.Context, tx *sql.Tx, ch Change, v Version, cred Credential) error
}

// AutoLandGuard judges a value of auto_land before it is stored, in tx,
// with the caller and the change as it stands; its error refuses the
// request unchanged. Without one every value is stored as given.
type AutoLandGuard interface {
	CheckAutoLand(ctx context.Context, tx *sql.Tx, caller Caller, ch Change, value bool) error
}

// ForkActivity records activity on a task fork, which the fork feature
// uses for the fork's lifetime. It is called in tx; its error is logged
// and changes nothing. Without one nothing is recorded.
type ForkActivity interface {
	RecordActivity(ctx context.Context, tx *sql.Tx, slug string) error
}

// RegisterSessionLinker makes l the session linker, replacing the one
// before; nil removes it. Collaborators are registered on the instance while
// the hub is composed, never in a registry of the package, so that every
// hub has its own.
func (c *Component) RegisterSessionLinker(l SessionLinker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = l
}

// RegisterStackLinker makes l the stack linker; nil removes it.
func (c *Component) RegisterStackLinker(l StackLinker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stack = l
}

// RegisterLandingEvaluator makes e the landing evaluator; nil removes it.
func (c *Component) RegisterLandingEvaluator(e LandingEvaluator) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evaluator = e
}

// AddVersionObserver adds o after the observers added before it; they are
// told in that order.
func (c *Component) AddVersionObserver(o VersionObserver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if o != nil {
		c.observers = append(c.observers, o)
	}
}

// RegisterAutoLandGuard makes g the auto_land guard; nil removes it.
func (c *Component) RegisterAutoLandGuard(g AutoLandGuard) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guard = g
}

// RegisterForkActivity makes a the fork activity recorder; nil removes it.
func (c *Component) RegisterForkActivity(a ForkActivity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activity = a
}

// collaborators are the registered collaborators at one moment.
type collaborators struct {
	sessions  SessionLinker
	stack     StackLinker
	evaluator LandingEvaluator
	observers []VersionObserver
	guard     AutoLandGuard
	activity  ForkActivity
	restacker Restacker
}

func (c *Component) collaborators() collaborators {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return collaborators{c.sessions, c.stack, c.evaluator, append([]VersionObserver(nil), c.observers...), c.guard, c.activity, c.restacker}
}
