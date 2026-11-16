package land

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/server"
)

// OverrideGuard is the request guard that decides who may land with an
// override: the workspace's owner with a credential not confined to a
// workspace, or the admin token, while the workspace's land.allow_override
// is on. An override is the one way past unmet approvals and checks, so it
// is kept out of reach of every agent token. The land route reads and
// validates the override member before it asks the guard; the guard
// judges a request that carries one, whatever the landing status of its
// changes, and lets every other request go on.
func (c *Component) OverrideGuard() OverrideGuard { return OverrideGuard{c} }

// OverrideGuard is the refusal of an override a caller may not use; see
// (*Component).OverrideGuard.
type OverrideGuard struct{ c *Component }

// Check refuses, with 403 override_not_allowed, a land request that
// carries an override, in this order: from a PAT bound to a workspace (the
// target or one of its forks), from a caller who is neither the admin
// token nor a writer of the workspace, its owner, and, for every caller,
// while the workspace's land.allow_override is off. It is asked before
// the land's preconditions, so a refusal records no attempt.
func (g OverrideGuard) Check(ctx context.Context, in RequestInput) error {
	if in.Override == nil {
		return nil
	}
	caller := in.Caller
	if caller.Bound != "" {
		return overrideNotAllowed(fmt.Sprintf("a token bound to %s may not land with an override", caller.Bound))
	}
	if caller.Credential.Type != change.CredentialAdminToken {
		owner := false
		if caller.UserID != "" {
			var err error
			if owner, err = g.c.deps.Access.CanWrite(ctx, caller.UserID, in.Workspace); err != nil {
				return err
			}
		}
		if !owner {
			return overrideNotAllowed(fmt.Sprintf("only the owner of %s or the admin token may land with an override", in.Workspace))
		}
	}
	ws, err := g.c.workspace(ctx, in.Workspace)
	if err != nil {
		return err
	}
	if !ws.AllowOverride {
		return overrideNotAllowed(fmt.Sprintf("land.allow_override is off in %s", in.Workspace))
	}
	return nil
}

// overrideNotAllowed is the refusal of an override: 403
// override_not_allowed.
func overrideNotAllowed(message string) error {
	return server.NewError("override_not_allowed", http.StatusForbidden, message)
}

// PendingOverride is the override rule: an accepted override lets a change
// whose landing is pending, held back by approvals or checks, pass the
// policy gate, and never one that is blocked, by a block requirement or a
// structural reason, nor any other answer.
type PendingOverride struct{}

// Passes reports whether answer is pending.
func (PendingOverride) Passes(_ context.Context, _ Override, answer GateAnswer) (bool, error) {
	return answer.Status == change.LandingPending, nil
}
