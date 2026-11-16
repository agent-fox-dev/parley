package awareness

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// actedOn is the workspace an awareness route acts on, read from the
// workspace record the access check granted :slug on: the parent for a
// task fork, whose changes target the parent and whose claims are stored
// there, and the workspace itself otherwise. Every awareness route gets
// its workspace here.
func actedOn(r *http.Request) string {
	d, ok := access.DecisionOf(r)
	if !ok {
		// Every awareness route declares an access need, so the access
		// check has run; without its decision :slug is all there is.
		return server.Param(r, "slug")
	}
	if d.Record.Kind == access.KindFork {
		return d.Record.ParentSlug
	}
	return d.Record.Slug
}
