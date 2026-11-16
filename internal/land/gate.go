package land

import (
	"context"
	"encoding/json"

	"github.com/agent-fox-dev/parley/internal/change"
)

// GateInput is what the policy gate is asked about: one change of an
// attempt, of the workspace, with the version the attempt lands and the
// caller who asked for the land.
type GateInput struct {
	Workspace string
	Change    change.Change
	Version   int64
	Caller    change.Caller
}

// GateAnswer is the policy gate's answer for one change: its status, one
// of change.LandingReady, change.LandingPending and change.LandingBlocked,
// the reason of an answer that is not ready (nil for none), the landing
// requirements, each a JSON object the policy writes, and the revision of
// the policy that answered.
type GateAnswer struct {
	Status       string
	Reason       *string
	Requirements []json.RawMessage
	Revision     int64
}

// Gate is the policy gate a land asks once per change before it replays
// anything. The policy is another feature's, which registers its
// evaluator here, so that landing depends on no policy package.
type Gate interface {
	Evaluate(ctx context.Context, in GateInput) (GateAnswer, error)
}

// OverrideRule says whether the override of a land request lets a gate
// answer that is not ready pass. Who may override, and what an override
// skips, is another feature's rule, registered here.
type OverrideRule interface {
	Passes(ctx context.Context, override Override, answer GateAnswer) (bool, error)
}

// readyGate is the gate of a hub with no policy: every change is ready, by
// revision 0.
type readyGate struct{}

func (readyGate) Evaluate(context.Context, GateInput) (GateAnswer, error) {
	return GateAnswer{Status: change.LandingReady}, nil
}

// noOverride is the override rule of a hub with none: no override lets
// anything pass.
type noOverride struct{}

func (noOverride) Passes(context.Context, Override, GateAnswer) (bool, error) { return false, nil }

// RegisterGate makes g the policy gate, replacing the one registered
// before; nil restores the gate that answers every change ready.
func (c *Component) RegisterGate(g Gate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gate = g
}

// RegisterOverrideRule makes r the override rule, replacing the one
// registered before; nil restores the rule that passes nothing.
func (c *Component) RegisterOverrideRule(r OverrideRule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.override = r
}

// policy is the registered gate and override rule, the defaults in place
// of those not registered.
func (c *Component) policy() (Gate, OverrideRule) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	g, r := c.gate, c.override
	if g == nil {
		g = readyGate{}
	}
	if r == nil {
		r = noOverride{}
	}
	return g, r
}
