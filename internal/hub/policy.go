package hub

import (
	"context"
	"errors"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// policyHook is the name of the start hook that binds the policy
// component.
const policyHook = "policy"

// composePolicy adds the landing policy: the schema of the policy
// revisions and of the verdicts and check results it counts, the policy
// routes, whose scope, binding and access the shared checks decide, and
// the component, bound in a start hook to the store, to the workspace
// records, to the users of the identity records, to the event component,
// to the change component, to the workspaces' repositories, to the hub's
// clock and to its logger. Its evidence source is the one that reads none
// until a part registers another (RegisterEvidence).
//
// It comes before the change component's part, so that the component is
// bound before the change component's own start hooks record versions,
// which ask the landing evaluator composePolicyEvaluation registers once
// the change and land components exist.
//
// The teardown of a workspace deletes its policy revisions, verdicts and
// check results in the delete's transaction
// (docs/adr/07-register-the-teardown-collaborators-on-the-workspace-service.md).
//
// The component exists from the composition on (Policy); tests change its
// dependencies with AdjustPolicy.
func composePolicy(c *Composition) {
	comp := &policy.Component{}
	c.policy = comp
	c.AddSchemaStep(policy.SchemaStep)
	c.AddSchemaStep(policy.ReviewStep)
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.workspaces.Teardown().RegisterRemover(comp.WorkspaceDeleted)
	c.AddStartHook(policyHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := policy.Deps{
			Store: rt.Store, Workspaces: policyWorkspaces{}, Users: policyUsers{q: rt.Store.Main()},
			Events: c.events, Changes: c.changes,
			Repos: func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			Now: rt.Now, Logger: rt.Logger,
		}
		for _, adjust := range c.policyAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// composePolicyEvaluation registers the policy's evaluation where the
// landing of a change is decided, in place of the defaults that let every
// change land: as the change component's landing evaluator, asked for
// every new version, retarget, move between draft and open and restore,
// and as the land component's policy gate. Tests register their own on
// the instances after it.
func composePolicyEvaluation(c *Composition) {
	c.changes.RegisterLandingEvaluator(c.policy)
	c.land.RegisterGate(policyGate{c.policy})
}

// policyGate is the policy gate of the lands: the version the attempt
// lands judged by JudgeVersion, which reads the change in the transaction
// it judges in. A land of the change's current version stores the landing
// and announces the change when it is landable; one of an older version,
// as a land whose change moved on since it was asked, evaluates that
// version's commits and stores nothing. A change that closed since is the
// gate's error.
type policyGate struct{ policy *policy.Component }

func (g policyGate) Evaluate(ctx context.Context, in land.GateInput) (land.GateAnswer, error) {
	res, rev, err := g.policy.JudgeVersion(ctx, in.Workspace, in.Change.Number, in.Version, in.Base, in.Head)
	if err != nil {
		return land.GateAnswer{}, err
	}
	l, err := res.Landing()
	if err != nil {
		return land.GateAnswer{}, err
	}
	return land.GateAnswer{Status: l.Status, Reason: l.Reason, Requirements: l.Requirements, Revision: rev}, nil
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
// document either. The lookup is by username alone, never by id or e-mail,
// so that a reader trying out a candidate document cannot probe the hub's
// e-mails and ids through it.
type policyUsers struct{ q identity.Querier }

func (u policyUsers) UserExists(ctx context.Context, username string) (bool, error) {
	_, err := identity.UserByUsername(ctx, u.q, username)
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
