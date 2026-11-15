package hub

import "github.com/agent-fox-dev/parley/internal/gitserver"

// PushChanges is the change service of the push processor's mapping stage
// of the composition c, ready once the hub has started.
func PushChanges(c *Composition) gitserver.ChangeService { return pushChanges{c.changes, c.runtime} }
