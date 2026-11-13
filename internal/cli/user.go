package cli

import (
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// newUserCommand builds parley user: the caller's own profile.
func newUserCommand() *cobra.Command {
	return group("user", "Read and update your own profile",
		declare(command{
			Name:        "show",
			Description: "Print your profile",
			Method:      http.MethodGet,
			Path:        "/user",
			Auth:        authUser,
		}),
		declare(command{
			Name:        "update",
			Description: "Change your full name",
			Method:      http.MethodPatch,
			Path:        "/user",
			Auth:        authUser,
			Flags:       fullNameFlag,
			Run:         sendFullName,
		}),
	)
}

// fullNameFlag adds the required --full-name of an update of a user.
func fullNameFlag(fs *pflag.FlagSet) {
	fs.String("full-name", "", "the user's full name; the empty string clears it")
	// It fails only for a flag the set does not hold, and the line above
	// adds it.
	_ = cobra.MarkFlagRequired(fs, "full-name")
}

// sendFullName sends the --full-name it was given, the empty string
// included, as the body's full_name.
func sendFullName(c *call) error {
	name, _ := c.cmd.Flags().GetString("full-name")
	return sendWith(c, nil, map[string]string{"full_name": name})
}

// sendWith is sendAndPrint for a command whose request carries a query or a
// body: it sends the command's method to its path, its :params filled in
// from the arguments, with query and body, and prints the hub's answer. A
// nil body sends none.
func sendWith(c *call, query url.Values, body any) error {
	resp, err := c.client.request(c.ctx, c.decl.Method, routePath(c.decl.Path, c.args), query, body)
	if err != nil {
		return err
	}
	return c.printAnswer(resp)
}
