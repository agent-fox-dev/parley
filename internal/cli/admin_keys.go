package cli

import (
	"net/http"

	"github.com/spf13/cobra"
)

// newAdminKeysCommand builds parley admin keys: the API keys of any user.
func newAdminKeysCommand() *cobra.Command {
	return group("keys", "List and revoke the API keys of any user",
		declare(command{
			Name:        "list",
			Description: "List a user's API keys, newest first",
			Method:      http.MethodGet,
			Path:        "/users/:user/keys",
			Args:        []argSpec{userArg},
			Auth:        authAdmin,
		}),
		declare(command{
			Name:        "revoke",
			Description: "Revoke one of a user's API keys",
			Method:      http.MethodDelete,
			Path:        "/users/:user/keys/:key_id",
			Args:        []argSpec{userArg, {Name: "key_id", Required: true}},
			Auth:        authAdmin,
		}),
	)
}
