package cli

import (
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// userArg is the <user> argument: a user's id, e-mail or username.
var userArg = argSpec{Name: "user", Required: true}

// newAdminUsersCommand builds parley admin users: the administration of
// every user of the hub.
func newAdminUsersCommand() *cobra.Command {
	// onUser declares a command that sends method to path, of one user,
	// without a body.
	onUser := func(name, description, method, path string) *cobra.Command {
		return declare(command{
			Name:        name,
			Description: description,
			Method:      method,
			Path:        path,
			Args:        []argSpec{userArg},
			Auth:        authAdmin,
		})
	}
	return group("users", "Administer users",
		declare(command{
			Name:        "list",
			Description: "List the active users, newest first, and with --include-blocked the blocked ones too",
			Method:      http.MethodGet,
			Path:        "/users",
			Auth:        authAdmin,
			Flags: func(fs *pflag.FlagSet) {
				fs.Bool("include-blocked", false, "list the blocked users too")
			},
			Run: func(c *call) error {
				var query url.Values
				if include, _ := c.cmd.Flags().GetBool("include-blocked"); include {
					query = url.Values{"include_blocked": {"true"}}
				}
				return sendWith(c, query, nil)
			},
		}),
		onUser("show", "Print a user", http.MethodGet, "/users/:user"),
		declare(command{
			Name:        "create",
			Description: "Create a user and the user's personal organization",
			Method:      http.MethodPost,
			Path:        "/users",
			Auth:        authAdmin,
			Flags: func(fs *pflag.FlagSet) {
				for _, f := range newUserFlags {
					fs.String(f.flag, "", f.usage)
					// It fails only for a flag the set does not hold.
					_ = cobra.MarkFlagRequired(fs, f.flag)
				}
				fs.String("full-name", "", "the user's full name; sent only when given")
			},
			Run: func(c *call) error {
				fs := c.cmd.Flags()
				body := map[string]string{}
				for _, f := range newUserFlags {
					body[f.member], _ = fs.GetString(f.flag)
				}
				// An absent --full-name leaves the member out, so the hub's
				// own default applies.
				if fs.Changed("full-name") {
					body["full_name"], _ = fs.GetString("full-name")
				}
				return sendWith(c, nil, body)
			},
		}),
		declare(command{
			Name:        "update",
			Description: "Change a user's full name",
			Method:      http.MethodPatch,
			Path:        "/users/:user",
			Args:        []argSpec{userArg},
			Auth:        authAdmin,
			Flags:       fullNameFlag,
			Run:         sendFullName,
		}),
		onUser("delete", "Delete a user, keeping the user's organizations", http.MethodDelete, "/users/:user"),
		onUser("promote", "Give a user the role admin", http.MethodPost, "/users/:user/promote"),
		onUser("demote", "Give a user the role user", http.MethodPost, "/users/:user/demote"),
		onUser("block", "Block a user", http.MethodPost, "/users/:user/block"),
		onUser("unblock", "Unblock a user", http.MethodPost, "/users/:user/unblock"),
	)
}

// newUserFlags are the required flags of parley admin users create, each
// with the body member it is sent as.
var newUserFlags = []struct{ flag, member, usage string }{
	{"username", "username", "the user's username"},
	{"email", "email", "the user's e-mail address"},
	{"provider", "provider", "the identity provider the user signs in with"},
	{"provider-id", "provider_id", "the user's id at that provider"},
}
