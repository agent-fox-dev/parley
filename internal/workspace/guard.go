package workspace

import (
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// RequireReady is the state check of every route that needs a workspace's
// repository: nil when w is ready and no maintenance operation runs on it,
// and otherwise the 409 the route answers without acting. The marker is read
// first, because a reclone or a delete of a failed or archived workspace is
// work in progress, which the caller should wait out rather than repair.
// The guard reads only w and takes no lock, so that a ready workspace serves
// any number of requests at once.
func RequireReady(w Workspace) error {
	if w.Marker != MarkerNone {
		return busy(fmt.Sprintf("workspace %q is busy: a %s is running; try again later", w.Slug, w.Marker))
	}
	switch w.Status {
	case StatusReady:
		return nil
	case StatusPending, StatusCloning:
		return busy(fmt.Sprintf("workspace %q is busy: its clone has not finished; try again later", w.Slug))
	case StatusArchived:
		return server.WorkspaceArchived(fmt.Sprintf("workspace %q is archived", w.Slug))
	default:
		return server.NewError("workspace_not_ready", http.StatusConflict,
			fmt.Sprintf("workspace %q is not ready: its clone failed; reclone it", w.Slug))
	}
}

// busy is the answer for a workspace that will be able to serve the request
// once the work running on it ends.
func busy(message string) *server.Error {
	return server.NewError("workspace_busy", http.StatusConflict, message)
}
