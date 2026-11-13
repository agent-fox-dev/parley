package hub

import "github.com/agent-fox-dev/parley/internal/access"

// composeAccess adds the workspace records the access rule reads to the
// transactional store. No remover is registered on the identity service: a
// workspace outlives its owner's and its org's deletion, and a deleted org
// grants nothing because its memberships go with it.
func composeAccess(c *Composition) {
	c.AddSchemaStep(access.SchemaStep)
}
