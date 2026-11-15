package pushrule

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Forward is a pending forward of an update to the workspace's origin. It
// returns nil once origin took the update, or an error whose text is the
// message the update is refused with. The ref writer runs it after every
// update of the request is allowed and before any ref is written, with a
// context that ends at its time limit; Evaluate never runs it.
type Forward func(ctx context.Context) error

// answerKind tells the answers of a controller apart. The zero value is no
// answer at all, which refuses the update as internal rather than allowing
// it.
type answerKind int

const (
	answerAllow answerKind = iota + 1
	answerRefuse
	answerForward
)

// Answer is a carry-patch controller's answer about one update, made by
// Allow, Refuse or AllowAfterForward. The zero Answer refuses the update as
// internal.
type Answer struct {
	kind               answerKind
	message, errorType string
	forward            Forward
}

// Allow answers that carry-patch push control lets the update go on to the
// rules after it.
func Allow() Answer { return Answer{kind: answerAllow} }

// Refuse answers that the update is refused with message and errorType,
// ErrorTypeIntegrationBranch or ErrorTypePatchBranch.
func Refuse(message, errorType string) Answer {
	return Answer{kind: answerRefuse, message: message, errorType: errorType}
}

// AllowAfterForward answers that the update goes on to the rules after it
// and, when they allow it too, is written only once f has forwarded it to
// origin. A nil f refuses the update as internal.
func AllowAfterForward(f Forward) Answer { return Answer{kind: answerForward, forward: f} }

// Controller is carry-patch push control: it decides how the refs of a
// workspace in mode carry_patch may move. Decide is asked once for every
// update of such a workspace that the rules for hub-owned refs, protected
// branches, the default branch and tracking branches let through, with the
// facts of the workspace and of the caller; an error refuses the update as
// internal.
type Controller interface {
	Decide(ctx context.Context, ws Workspace, caller Caller, u Update) (Answer, error)
}

// RegisterController makes c the engine's carry-patch controller, replacing
// the one registered before; nil removes it. The feature that owns
// carry-patch push control registers it while the hub is composed. Without
// one, every update of a carry_patch workspace that reaches the rule is
// refused as internal, since allowing it would let the integration branch
// and the patch branches move unchecked.
func (e *Engine) RegisterController(c Controller) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.controller = c
}

// carryPatch hands an update of a carry_patch workspace to the registered
// controller and takes its answer; it leaves every other workspace alone. A
// forward it is answered with waits on the update being decided until the
// later rules have spoken.
func carryPatch(ctx context.Context, ev *evaluation, u Update) *refusal {
	if ev.req.Workspace.Mode != workspace.ModeCarryPatch {
		return nil
	}
	if ev.controller == nil {
		return internal
	}
	a, err := ev.controller.Decide(ctx, ev.req.Workspace, ev.req.Caller, u)
	if err != nil {
		return internal
	}
	switch a.kind {
	case answerAllow:
		return nil
	case answerRefuse:
		return &refusal{message: a.message, errorType: a.errorType}
	case answerForward:
		if a.forward == nil {
			return internal
		}
		ev.cur.forward = a.forward
		return nil
	}
	return internal
}
