package cli

import (
	"net/http"

	"github.com/spf13/cobra"
)

// varsCollection is the last segment of the variable routes of a scope.
const varsCollection = "vars"

// newVarsCommand builds parley vars: the variables of the caller's user, of
// an organization and of a workspace, and those a workspace runs with. They
// share the scope flags, the entries and the run steps of parley secrets;
// only the routes differ, and a variable's value is printed.
func newVarsCommand() *cobra.Command {
	return group("vars", "Create, list, update, delete and resolve variables",
		withScopeArgs(declare(command{
			Name:        "create",
			Description: "Create variables in your user scope, an organization's or a workspace's",
			Method:      http.MethodPost,
			Path:        "/user/vars",
			Args:        []argSpec{{Name: "KEY=VALUE", Required: true, Repeats: true}},
			Auth:        authUser,
			Flags:       scopeFlags(true, "create the variables in"),
			Run:         createEntries(varsCollection),
		}), true, checkEntries),
		withScopeArgs(declare(command{
			Name:        "list",
			Description: "List a scope's variables with their values, every page of them",
			Method:      http.MethodGet,
			Path:        "/user/vars",
			Auth:        authUser,
			Flags:       scopeFlags(false, "list the variables of"),
			Run:         listEntries(varsCollection),
		}), false, nil),
		withScopeArgs(declare(command{
			Name:        "update",
			Description: "Replace the value of one variable",
			Method:      http.MethodPatch,
			Path:        "/user/vars/:key",
			// More than one entry is refused by checkOneEntry, as for a
			// secret.
			Args:  []argSpec{{Name: "KEY=VALUE", Required: true, Repeats: true}},
			Auth:  authUser,
			Flags: scopeFlags(true, "update the variable of"),
			Run:   updateEntry(varsCollection),
		}), false, checkOneEntry),
		withScopeArgs(declare(command{
			Name:        "delete",
			Description: "Delete one variable",
			Method:      http.MethodDelete,
			Path:        "/user/vars/:key",
			Args:        []argSpec{{Name: "KEY", Required: true}},
			Auth:        authUser,
			Flags:       scopeFlags(false, "delete the variable of"),
			Run:         deleteEntry(varsCollection),
			Confirm:     deletedKey,
		}), false, checkKey),
		// resolve declares no scope flag: the chain of the workspace decides
		// the scopes, so one given is refused as an unknown flag.
		withArgsCheck(declare(command{
			Name:        "resolve",
			Description: "List the variables a workspace runs with and where each comes from",
			Method:      http.MethodGet,
			Path:        "/workspaces/:slug/vars/resolved",
			Args:        []argSpec{{Name: "WORKSPACE", Required: true}},
			Auth:        authUser,
			Run: func(c *call) error {
				return printWholeList(c, "/workspaces/"+pathSegment(c.args["WORKSPACE"])+"/vars/resolved")
			},
		}), checkSlug),
	)
}

// checkSlug refuses an empty workspace slug, which would leave a segment of
// the route empty and so name another route.
func checkSlug(cmd *cobra.Command, args []string) error {
	if args[0] == "" {
		return usageError("%s needs a WORKSPACE slug that is not empty", cmd.CommandPath())
	}
	return nil
}
