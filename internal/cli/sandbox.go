package cli

import (
	"net/http"

	"github.com/spf13/cobra"
)

// newSandboxCommand builds parley sandbox: the hub's sandboxes and the
// profiles they are created from.
func newSandboxCommand() *cobra.Command {
	return group("sandbox", "Work with the hub's sandboxes and their profiles",
		declare(command{
			Name:        "profiles",
			Description: "List the sandbox profiles the hub offers, in name order",
			Method:      http.MethodGet,
			Path:        "/sandbox-profiles",
			Auth:        authUser,
		}),
	)
}
