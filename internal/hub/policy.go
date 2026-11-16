package hub

import (
	"context"
	"errors"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// policyHook is the name of the start hook that binds the policy
// component.
const policyHook = "policy"

// composePolicy adds the landing policy: the schema of the policy
// revisions, the policy routes, whose scope, binding and access the shared
// checks decide, and the component, bound in a start hook to the store,
// to the workspace records, to the users of the identity records, to the
// event component, to the hub's clock and to its logger. Its
// evidence source is the one that reads none until a part registers
// another (RegisterEvidence).
//
// The teardown of a workspace deletes its policy revisions in the delete's
// transaction (docs/adr/07-register-the-teardown-collaborators-on-the-workspace-service.md).
//
// The component exists from the composition on (Policy); tests change its
// dependencies with AdjustPolicy.
func composePolicy(c *Composition) {
	comp := &policy.Component{}
	c.policy = comp
	c.AddSchemaStep(policy.SchemaStep)
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.workspaces.Teardown().RegisterRemover(comp.WorkspaceDeleted)
	c.AddStartHook(policyHook, func(_ context.Context, rt *Runtime) error {
		d := policy.Deps{
			Store: rt.Store, Workspaces: policyWorkspaces{}, Users: policyUsers{q: rt.Store.Main()},
			Events: c.events, Now: rt.Now, Logger: rt.Logger,
		}
		for _, adjust := range c.policyAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// policyWorkspaces reads the workspace records for the policy component.
type policyWorkspaces struct{}

func (policyWorkspaces) Workspace(ctx context.Context, q policy.Querier, slug string) (policy.Workspace, bool, error) {
	w, found, err := workspace.BySlug(ctx, q, slug)
	if err != nil || !found {
		return policy.Workspace{}, found, err
	}
	out := policy.Workspace{Kind: w.Kind, Status: string(w.Status)}
	if w.DefaultBranch != nil {
		out.DefaultBranch = *w.DefaultBranch
	}
	return out, true, nil
}

// policyUsers answers whether the users a policy document names exist,
// from the identity records: a user with that username, whatever its
// status, since disabling or deleting a user later does not invalidate the
// document either.
type policyUsers struct{ q identity.Querier }

func (u policyUsers) UserExists(ctx context.Context, username string) (bool, error) {
	_, err := identity.UserBySelector(ctx, u.q, username)
	if errors.Is(err, identity.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Policy returns the hub's policy component, which serves once its start
// hook has run.
func (c *Composition) Policy() *policy.Component { return c.policy }

// AdjustPolicy adds fn to the functions that change the policy component's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added.
func (c *Composition) AdjustPolicy(fn func(*policy.Deps)) {
	c.policyAdjusters = append(c.policyAdjusters, fn)
}
