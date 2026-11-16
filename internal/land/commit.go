package land

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

// EventLand is the event of a change that landed.
const EventLand = "parley.change.land"

// errTargetMoved is commit's error when the target was no longer at the
// head the commits were built on: nothing moved and nothing was recorded.
var errTargetMoved = errors.New("the target moved since it was read")

// commit lands what b built for the attempt a of the target workspace ws:
// it moves the target from b.onto to b.head with a compare-and-swap, then
// records the land in one store transaction (settle), then has the push
// event of the move written. A land with nothing left moves nothing and
// writes no push event; it is recorded all the same, on the head it was
// built on.
//
// The target moves through the ref writer with a verdict built here that
// allows the one update, without asking the push rules, since a land is
// not a push and a protected target moves as any other
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
// moved reports whether the target moved; from then on the record and
// the push event are written whatever becomes of ctx, and an error leaves
// the attempt running.
func (c *Component) commit(ctx context.Context, repo *gitrepo.Repo, ws Workspace, a Attempt, b built) (moved bool, err error) {
	a.Status, a.OntoSHA, a.LandedSHA, a.Noop, a.Commits = StatusLanded, new(b.onto), new(b.head), b.noop(), b.commits
	a.FinishedAt = new(c.now())
	if a.Noop {
		return false, c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.settle(ctx, tx, a) })
	}
	ref := "refs/heads/" + a.TargetBranch
	verdict, err := pushrule.AllowedVerdict(ctx, repo, []pushrule.Update{{Ref: ref, Before: b.onto, After: b.head}})
	if err != nil {
		return false, err
	}
	caller := callerOf(a)
	res, err := c.deps.RefWriter.Apply(ctx, pushrule.ApplyRequest{
		Verdict: verdict, Repo: repo, SessionID: a.SessionID, Via: pushrule.ViaLand, VersionKind: pushrule.VersionKindNone,
		Workspace: pushrule.Workspace{Slug: ws.Slug, Kind: ws.Kind, Parent: ws.Parent, DefaultBranch: ws.DefaultBranch, Mode: ws.Mode},
		Caller:    pushrule.Caller{CredentialType: caller.Credential.Type, CredentialID: caller.Credential.ID, UserID: caller.UserID},
	})
	switch res.Outcome {
	case pushrule.OutcomeAccepted:
	case pushrule.OutcomeRefMoved:
		return false, errTargetMoved
	default:
		if err == nil {
			err = errors.New("the ref writer moved nothing")
		}
		return false, err
	}
	ctx = context.WithoutCancel(ctx)
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.settle(ctx, tx, a) })
	c.deps.RefWriter.Finish(ctx, res)
	return true, err
}

// settle records, in tx, the land of the attempt a, final with status
// landed and its result members: each change it covers moves to landed,
// with its own landed commits mapped to it, the attempt is finished and
// each change gets its parley.change.land event, by whoever asked for the
// land, in the session they named.
func (c *Component) settle(ctx context.Context, tx *sql.Tx, a Attempt) error {
	for _, cov := range a.Covered {
		l := change.Landed{SHA: *a.LandedSHA, At: *a.FinishedAt, By: a.RequestedBy, LandID: a.ID}
		for _, cm := range a.Commits {
			if cm.Change == cov.Number {
				l.Commits = append(l.Commits, change.LandedCommit{VersionSHA: cm.VersionSHA, LandedSHA: cm.LandedSHA})
			}
		}
		if err := c.deps.Changes.SetLanded(ctx, tx, a.Workspace, cov.Number, l); err != nil {
			return err
		}
	}
	if err := c.finish(ctx, tx, a); err != nil {
		return err
	}
	for _, cov := range a.Covered {
		if _, err := c.deps.Events.Emit(ctx, tx, event.Input{
			Type: EventLand, Workspace: &a.Workspace, ActorType: a.RequestedBy.ActorType, ActorID: a.RequestedBy.ActorID,
			ResourceType: "change", ResourceID: strconv.FormatInt(cov.Number, 10), SessionID: a.SessionID,
			Metadata: map[string]any{
				"land_id": a.ID, "target_branch": a.TargetBranch, "onto_sha": a.OntoSHA, "landed_sha": a.LandedSHA,
				"strategy": a.Strategy, "attempts": a.Attempts, "noop": a.Noop, "trigger": a.Trigger, "override": a.Override,
				"policy_revision": a.PolicyRevision, "number": cov.Number, "ref": change.Ref(a.Workspace, cov.Number),
			},
		}); err != nil {
			return err
		}
	}
	return nil
}
