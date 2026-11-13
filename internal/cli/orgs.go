package cli

import (
	"net/http"

	"github.com/spf13/cobra"
)

// orgArg is the <org> argument: an org's id or slug.
var orgArg = argSpec{Name: "org", Required: true}

// newOrgsCommand builds parley orgs: the orgs the caller belongs to.
func newOrgsCommand() *cobra.Command {
	return group("orgs", "List and read the organizations you belong to",
		declare(command{
			Name:        "list",
			Description: "List the active organizations you are a member of",
			Method:      http.MethodGet,
			Path:        "/user/orgs",
			Auth:        authUser,
		}),
		declare(command{
			Name:        "show",
			Description: "Print an organization",
			Method:      http.MethodGet,
			Path:        "/orgs/:org",
			Args:        []argSpec{orgArg},
			Auth:        authUser,
		}),
		declare(command{
			Name:        "members",
			Description: "List the members of an organization",
			Method:      http.MethodGet,
			Path:        "/orgs/:org/members",
			Args:        []argSpec{orgArg},
			Auth:        authUser,
		}),
	)
}
