package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// composeAccess adds the workspace records the access rule reads to the
// transactional store, adds the rule's workspace access check to the access
// checks, after identity's admin check, and binds the rule in a start hook,
// before the hub serves its first request, to the records and to the
// memberships of users in active orgs, which the identity service keeps.
// No remover is registered on the identity service: a workspace outlives
// its owner's and its org's deletion, and a deleted org grants nothing
// because its memberships go with it.
func composeAccess(c *Composition) {
	svc := &access.Service{}
	c.AddSchemaStep(access.SchemaStep)
	c.AddAccessCheck(svc.Check)
	c.AddStartHook("access", func(_ context.Context, rt *Runtime) error {
		db := rt.Store.Main()
		svc.Bind(access.Deps{
			Records: access.StoreRecords(db),
			Memberships: access.MembershipsFunc(func(ctx context.Context, userID string) ([]string, error) {
				return identity.ActiveOrgIDs(ctx, db, userID)
			}),
		})
		return nil
	})
}
