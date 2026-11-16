package land

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// conflictAnswer is the answer of GET …/changes/:number/conflict: the
// change's mergeable as it is now, clean or conflict, and its conflict
// report, null when clean.
type conflictAnswer struct {
	Mergeable string                  `json:"mergeable"`
	Conflict  *gitrepo.ConflictReport `json:"conflict"`
}

// conflictRoute answers the current conflict of the change :number of the
// workspace :slug (currentConflict). The change is resolved as every
// change route resolves it, so that a :number that names no change of the
// workspace, and, for a token bound to a fork calling on the fork's
// parent, a change whose source is not the fork, are both 404
// change_not_found. Unlike the land routes, which tell a bound token why
// they refuse it, a read of a change refuses a change the token may not
// see as if it did not exist.
func (c *Component) conflictRoute(w http.ResponseWriter, r *http.Request) error {
	ch, err := c.deps.Changes.RequestedChange(r)
	if err != nil {
		return err
	}
	report, err := c.currentConflict(r.Context(), ch)
	if err != nil {
		return err
	}
	answer := conflictAnswer{Mergeable: change.MergeableClean}
	if report != nil {
		answer = conflictAnswer{Mergeable: change.MergeableConflict, Conflict: report}
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}
