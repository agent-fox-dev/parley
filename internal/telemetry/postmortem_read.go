package telemetry

import (
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// servePostmortem answers the postmortem of the path's workspace and run
// with every field as stored. A run without one in this workspace, also
// when another workspace holds one for the same run id, is 404
// postmortem_not_found: a postmortem belongs to its workspace.
func (c *Component) servePostmortem(w http.ResponseWriter, r *http.Request) error {
	if _, ok := access.DecisionOf(r); !ok {
		return errNoDecision
	}
	slug, runID := server.Param(r, "slug"), server.Param(r, "run_id")
	if !ValidRunID(runID) {
		return server.InvalidRequest(msgRunID)
	}
	p, found, err := getPostmortem(r.Context(), c.deps.DB, slug, runID)
	if err != nil {
		return fmt.Errorf("read the postmortem of run %s in workspace %s: %w", runID, slug, err)
	}
	if !found {
		return server.NewError("postmortem_not_found", http.StatusNotFound,
			fmt.Sprintf("run %s has no postmortem in workspace %s", runID, slug))
	}
	server.WriteJSON(w, http.StatusOK, p)
	return nil
}
