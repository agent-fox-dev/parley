package hub

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// composeAccess adds the workspace records the access rule reads, with the
// creating token of a fork, to the transactional store, adds the rule's workspace access check to the access
// checks, after identity's admin check, and binds the rule in a start hook,
// before the hub serves its first request, to the records and to the
// memberships of users in active orgs, which the identity service keeps.
// It also refuses, at the start, a route that is public and declares a
// workspace need, since no access check would run on it.
// No remover is registered on the identity service: a workspace outlives
// its owner's and its org's deletion, and a deleted org grants nothing
// because its memberships go with it.
// The service is kept on the composition, so that the event component
// decides with the same rule and the same memberships.
//
// The service's binding guard is installed in the binding-guard position of
// the pipeline, so that it runs for every REST route, and the start hook
// gives the event component the covered set of a workspace-bound token, from
// which the event routes confine such a token's events to what its binding
// covers. The guard's refusals go to a refusal limiter, which reports them
// as rate-limited events through the event component; the limiter is built
// in the start hook, so each start of the hub begins with none counted.
//
// The bound-token routes are registered here, with the subset rule as an
// access check before the workspace access check, so that a PAT asking for
// more than it holds is refused before the workspace is looked up; the
// service mints in the hub's store and records each binding through the
// event component in the minting transaction.
func composeAccess(c *Composition) {
	svc := &access.Service{}
	c.access = svc
	c.AddSchemaStep(access.SchemaStep)
	c.AddSchemaStep(access.CreatingTokenStep)
	c.SetBindingGuard(svc.Guard)
	c.AddAccessCheck(svc.SubsetCheck)
	c.AddAccessCheck(svc.Check)
	for _, r := range svc.TokenRoutes() {
		c.AddRoute(r)
	}
	c.addRouteRule(refusePublicNeed)
	c.AddStartHook("access", func(_ context.Context, rt *Runtime) error {
		db := rt.Store.Main()
		events := accessEvents{c}
		svc.Bind(access.Deps{
			Records: access.StoreRecords(db), Memberships: activeOrgs(db),
			Refusals: access.NewRefusalLimiter(events, rt.Now, rt.Logger), Events: events,
			Store: rt.Store, Now: rt.Now,
		})
		c.Events().SetCoverage(coverage(svc))
		return nil
	})
}

// coverage is the covered set of svc's workspace-bound tokens as the event
// component asks it: whether the binding of the caller, whose credential id
// is the PAT's token id, covers a workspace.
func coverage(svc *access.Service) event.Coverage {
	return event.CoverageFunc(func(ctx context.Context, ec event.Caller, slug string) (bool, error) {
		covered, err := svc.CoveredSet(ctx, access.Caller{
			Admin: ec.Admin, UserID: ec.UserID, TokenID: ec.CredentialID, Workspace: ec.Workspace,
		})
		return slices.Contains(covered, slug), err
	})
}

// accessEvents is the hub's event component as the access service emits
// through it (access.EventEmitter). The access package cannot import the
// event component, which imports it, so this converts its events. It reads
// the component from the composition at each emit, since the events part is
// composed after the access part.
type accessEvents struct{ c *Composition }

func (e accessEvents) Emit(ctx context.Context, tx *sql.Tx, ev access.Event) error {
	_, err := e.c.Events().Emit(ctx, tx, eventInput(ev))
	return err
}

func (e accessEvents) EmitNow(ctx context.Context, ev access.Event) error {
	_, err := e.c.Events().EmitNow(ctx, eventInput(ev))
	return err
}

// eventInput is ev as the event component takes it: an empty workspace or
// actor is none.
func eventInput(ev access.Event) event.Input {
	in := event.Input{Type: ev.Type, ActorType: ev.ActorType, ResourceType: ev.ResourceType, ResourceID: ev.ResourceID, Metadata: ev.Metadata}
	if ev.Workspace != "" {
		in.Workspace = &ev.Workspace
	}
	if ev.ActorID != "" {
		in.ActorID = &ev.ActorID
	}
	return in
}

// activeOrgs is the membership question of the hub: the active orgs of a
// user, which the identity service keeps in db.
func activeOrgs(db *sql.DB) access.Memberships {
	return access.MembershipsFunc(func(ctx context.Context, userID string) ([]string, error) {
		return identity.ActiveOrgIDs(ctx, db, userID)
	})
}

// errPublicNeed is a route that is public and declares a workspace need.
// The pipeline runs no access check on a public route, so its handler would
// serve every caller, the anonymous one included, without an access
// decision.
var errPublicNeed = errors.New("a public route cannot declare the workspace need " + access.MarkNeed + ": the pipeline runs no access check on it")

// refusePublicNeed is the route rule that refuses errPublicNeed.
func refusePublicNeed(r server.Route) error {
	if _, marked := r.Marks[access.MarkNeed]; marked && r.Public {
		return errPublicNeed
	}
	return nil
}
