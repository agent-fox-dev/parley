package cli

import (
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// newAdminOrgsCommand builds parley admin orgs: the administration of every
// organization of the hub and of its members.
func newAdminOrgsCommand() *cobra.Command {
	// onOrg declares a command that sends method to path, of one org,
	// without a body.
	onOrg := func(name, description, method, path string) *cobra.Command {
		return declare(command{
			Name:        name,
			Description: description,
			Method:      method,
			Path:        path,
			Args:        []argSpec{orgArg},
			Auth:        authAdmin,
		})
	}
	return group("orgs", "Administer organizations",
		declare(command{
			Name:        "list",
			Description: "List the active organizations by name, and with --include-blocked the blocked ones too",
			Method:      http.MethodGet,
			Path:        "/orgs",
			Auth:        authAdmin,
			Flags: func(fs *pflag.FlagSet) {
				fs.Bool("include-blocked", false, "list the blocked organizations too")
			},
			Run: func(c *call) error {
				var query url.Values
				if include, _ := c.cmd.Flags().GetBool("include-blocked"); include {
					query = url.Values{"include_blocked": {"true"}}
				}
				return sendWith(c, query, nil)
			},
		}),
		declare(command{
			Name:        "create",
			Description: "Create an organization",
			Method:      http.MethodPost,
			Path:        "/orgs",
			Auth:        authAdmin,
			Flags: func(fs *pflag.FlagSet) {
				fs.String("name", "", "the organization's name")
				fs.String("slug", "", "the organization's slug")
				// They fail only for a flag the set does not hold.
				_ = cobra.MarkFlagRequired(fs, "name")
				_ = cobra.MarkFlagRequired(fs, "slug")
				fs.String("url", "", "the organization's URL; sent only when given")
				fs.String("owner", "", "the id of the user who owns the organization; sent only when given")
			},
			Run: func(c *call) error {
				fs := c.cmd.Flags()
				body := map[string]string{}
				body["name"], _ = fs.GetString("name")
				body["slug"], _ = fs.GetString("slug")
				// An optional flag not given leaves its member out, so the
				// hub's own default applies.
				addChanged(fs, body, "url", "url")
				addChanged(fs, body, "owner", "owner_id")
				return sendWith(c, nil, body)
			},
		}),
		newAdminOrgsUpdateCommand(),
		onOrg("delete", "Delete an organization and its memberships", http.MethodDelete, "/orgs/:org"),
		onOrg("block", "Block an organization", http.MethodPost, "/orgs/:org/block"),
		onOrg("unblock", "Unblock an organization", http.MethodPost, "/orgs/:org/unblock"),
		group("members", "Administer the members of an organization",
			onOrg("list", "List the members of an organization by username", http.MethodGet, "/orgs/:org/members"),
			declare(command{
				Name:        "add",
				Description: "Make a user a member of an organization",
				Method:      http.MethodPut,
				Path:        "/orgs/:org/members/:user",
				Args:        []argSpec{orgArg, userArg},
				Auth:        authAdmin,
			}),
			declare(command{
				Name:        "remove",
				Description: "Remove a user from an organization",
				Method:      http.MethodDelete,
				Path:        "/orgs/:org/members/:user",
				Args:        []argSpec{orgArg, userArg},
				Auth:        authAdmin,
			}),
		),
	)
}

// newAdminOrgsUpdateCommand builds parley admin orgs update, which sends only
// the members whose flags are given, an empty --url included.
func newAdminOrgsUpdateCommand() *cobra.Command {
	cmd := declare(command{
		Name:        "update",
		Description: "Change an organization's name or URL",
		Method:      http.MethodPatch,
		Path:        "/orgs/:org",
		Args:        []argSpec{orgArg},
		Auth:        authAdmin,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("name", "", "the organization's new name; sent only when given")
			fs.String("url", "", "the organization's new URL, the empty string clears it; sent only when given")
		},
		Run: func(c *call) error {
			fs := c.cmd.Flags()
			body := map[string]string{}
			addChanged(fs, body, "name", "name")
			addChanged(fs, body, "url", "url")
			return sendWith(c, nil, body)
		},
	})
	// Neither flag is a command line the hub would refuse, so it is
	// refused here, with the other usage errors and before any setting is
	// resolved or any request sent.
	checkLine := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		if fs := cmd.Flags(); !fs.Changed("name") && !fs.Changed("url") {
			return usageError("%s needs the flag --name or --url", cmd.CommandPath())
		}
		return nil
	}
	return cmd
}

// addChanged sets body[member] to the value of the string flag name when the
// flag was given on the command line, the empty string included.
func addChanged(fs *pflag.FlagSet, body map[string]string, name, member string) {
	if fs.Changed(name) {
		body[member], _ = fs.GetString(name)
	}
}
