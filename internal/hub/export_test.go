package hub

import "github.com/agent-fox-dev/parley/internal/gitserver"

// PushChanges is the change service of the push processor's mapping stage
// of the composition c, ready once the hub has started.
func PushChanges(c *Composition) gitserver.ChangeService { return pushChanges{c.changes, c.runtime} }

// CopyTree is copyTree: it copies src to dst, leaving out the directories
// below src that deferred reports true for.
var CopyTree = copyTree
