package cli

import (
	"net/http"

	"github.com/spf13/cobra"
)

// newWorkspacePatchStatusCommand builds parley workspace patch-status.
func newWorkspacePatchStatusCommand() *cobra.Command {
	return declare(command{
		Name:        "patch-status",
		Description: "Show a carry-patch workspace's integration branch, its last rebuilds, each patch with the last rebuild's result for it, and the counts of its patches and remembered resolutions",
		Method:      http.MethodGet,
		Path:        "/workspaces/:slug/patch-status",
		Args:        []argSpec{slugArg},
		Auth:        authUser,
	})
}
