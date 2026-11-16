package land

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
)

// Guards, hooks and observers are registered on the component by the
// features that judge, follow up or count a land, so that landing depends
// on none of them
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md). Each
// is registered while the hub is composed, and each is asked or called in
// the order it was registered.

// RequestInput is what a request guard is asked about: the caller of a land
// request, the workspace that holds the change, the changes the attempt
// would cover, bottom first, each at its current version, and the override
// the request carries, nil for none.
type RequestInput struct {
	Caller    change.Caller
	Workspace string
	Changes   []CoveredChange
	Override  *Override
}

// RequestGuard judges a land request once the land route has admitted its
// caller and read its body, before the land's preconditions. A nil error
// lets the request go on; any other error refuses it, and is the answer of
// the land call as the guard gives it: a *server.Error with its status,
// error_type and members, such as the path a token may not reach.
type RequestGuard interface {
	Check(ctx context.Context, in RequestInput) error
}

// TargetInput is what a target guard is asked about: the move of the target
// branch of the workspace from Old, the head the changes were replayed
// onto, to New, the head the land would make it.
type TargetInput struct {
	Workspace, Branch, Old, New string
}

// TargetAnswer is a target guard's answer: the zero value allows the move;
// Refuse, the reason, refuses it, which ends the attempt blocked; and
// OriginRejected, origin's message, reports that origin rejected what the
// move needs, which ends the attempt failed origin_rejected.
type TargetAnswer struct {
	Refuse         string
	OriginRejected string
}

// TargetGuard judges the move of a land's target once the changes are
// replayed and before the target is first swapped. An error in place of an
// answer ends the attempt failed internal: a guard that cannot decide
// never allows.
type TargetGuard interface {
	Check(ctx context.Context, in TargetInput) (TargetAnswer, error)
}

// RefUpdate is the move of a ref: Ref, a full ref name, from Before to
// After; equal for a land that moved nothing.
type RefUpdate struct {
	Ref, Before, After string
}

// LandedChange is one change a land landed: its number, its source and the
// commit the target held once it had landed, its last landed commit, or for
// a change that brought nothing the commit the target held before it.
type LandedChange struct {
	Number                        int64
	SourceWorkspace, SourceBranch string
	LandedSHA                     string
}

// AfterLanding is what an after-landing hook is told of a land: the landed
// attempt, the slug of the target workspace, the move of the target
// branch, the session the land was asked in (nil for none) and the changes
// it landed, bottom first.
type AfterLanding struct {
	Attempt   Attempt
	Workspace string
	Update    RefUpdate
	SessionID *string
	Changes   []LandedChange
}

// AfterLandingHook is called once for every land recorded, a land that
// moved nothing included, after the record is committed and outside any
// store transaction, so that it may open its own. Its error is logged; it
// and a panic change neither the land nor what is told of it.
type AfterLandingHook func(ctx context.Context, in AfterLanding) error

// Final is what a final observer is told of an attempt that reached a final
// status: the attempt, its final status as Result, how many times it tried
// to move the target, and the time from its creation to its end.
type Final struct {
	Attempt  Attempt
	Result   string
	Attempts int
	Duration time.Duration
}

// FinalObserver is told once of every attempt that reached a final status,
// after that status is committed. A panic changes nothing.
type FinalObserver func(ctx context.Context, f Final)

// registered are the guards, hooks and observers of a component.
type registered struct {
	requestGuards []RequestGuard
	targetGuards  []TargetGuard
	afterLanding  []AfterLandingHook
	observers     []FinalObserver
}

// AddRequestGuard adds g to the request guards.
func (c *Component) AddRequestGuard(g RequestGuard) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks.requestGuards = append(c.hooks.requestGuards, g)
}

// AddTargetGuard adds g to the target guards.
func (c *Component) AddTargetGuard(g TargetGuard) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks.targetGuards = append(c.hooks.targetGuards, g)
}

// AddAfterLanding adds h to the after-landing hooks.
func (c *Component) AddAfterLanding(h AfterLandingHook) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks.afterLanding = append(c.hooks.afterLanding, h)
}

// AddFinalObserver adds o to the final observers.
func (c *Component) AddFinalObserver(o FinalObserver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks.observers = append(c.hooks.observers, o)
}

// registrations is a copy of what is registered, so that none is called
// with the lock held.
func (c *Component) registrations() registered {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return registered{
		requestGuards: slices.Clone(c.hooks.requestGuards), targetGuards: slices.Clone(c.hooks.targetGuards),
		afterLanding: slices.Clone(c.hooks.afterLanding), observers: slices.Clone(c.hooks.observers),
	}
}

// MergeabilityScheduler schedules the mergeability check of a change in
// the caller's transaction.
type MergeabilityScheduler interface {
	ScheduleMergeable(ctx context.Context, tx *sql.Tx, workspace string, number int64) error
}

// errGuardPanicked is the error of a target guard that panicked, which
// ends the attempt as any guard error does.
func errGuardPanicked(p any) error { return fmt.Errorf("a target guard panicked: %v", p) }
