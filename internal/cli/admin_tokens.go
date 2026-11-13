package cli

import (
	"net/http"

	"github.com/spf13/cobra"
)

// newAdminTokensCommand builds parley admin tokens: the personal access
// tokens of any user.
func newAdminTokensCommand() *cobra.Command {
	return group("tokens", "List and revoke the personal access tokens of any user",
		declare(command{
			Name:        "list",
			Description: "List a user's personal access tokens, newest first",
			Method:      http.MethodGet,
			Path:        "/users/:user/tokens",
			Args:        []argSpec{userArg},
			Auth:        authAdmin,
		}),
		declare(command{
			Name:        "revoke",
			Description: "Revoke one of a user's personal access tokens",
			Method:      http.MethodDelete,
			Path:        "/users/:user/tokens/:token_id",
			Args:        []argSpec{userArg, tokenIDArg},
			Auth:        authAdmin,
		}),
	)
}
