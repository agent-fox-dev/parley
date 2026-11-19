package cli

import (
	"net/http"
	"net/url"
	"slices"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The positional arguments of the network policy commands, besides the
// workspace and the sandbox.
var (
	proposalIDArg = argSpec{Name: "proposal-id", Required: true}
	ruleIDArg     = argSpec{Name: "rule-id", Required: true}
)

// The routes of the network policy commands: a sandbox's proposals, one
// proposal of it, and a workspace's sandbox rules.
const (
	proposalsRoute    = sandboxesRoute + "/:id/proposals"
	proposalRoute     = proposalsRoute + "/:proposal-id"
	sandboxRulesRoute = "/workspaces/:workspace-slug/sandbox-rules"
)

// approvalScopes are the values parley sandbox approve --scope takes.
var approvalScopes = []string{"sandbox", "workspace"}

// sandboxPolicyCommands builds the commands of the sandboxes' network
// access: parley sandbox proposals, approve and reject on a sandbox's
// proposals, and rules and rule-delete on a workspace's sandbox rules.
func sandboxPolicyCommands() []*cobra.Command {
	return []*cobra.Command{
		declare(command{
			Name:        "proposals",
			Description: "List the proposals of a sandbox for more network access, oldest first, the open ones unless --state says otherwise",
			Method:      http.MethodGet,
			Path:        proposalsRoute,
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.String("state", "", "list the proposals in this state, open, approved, rejected or withdrawn; open when not given")
			},
			Run: func(c *call) error {
				return sendWith(c, changedQuery(c.cmd.Flags(), "state"), nil)
			},
		}),
		newSandboxApproveCommand(),
		declare(command{
			Name:        "reject",
			Description: "Reject an open proposal of a sandbox and print the proposal",
			Method:      http.MethodPost,
			Path:        proposalRoute + "/reject",
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg, proposalIDArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.String("comment", "", "why the proposal is rejected")
			},
			Run: func(c *call) error {
				return sendWith(c, nil, decisionBody(c.cmd.Flags()))
			},
		}),
		declare(command{
			Name:        "rules",
			Description: "List the sandbox rules of a workspace, or of the parent of a task fork, oldest first",
			Method:      http.MethodGet,
			Path:        sandboxRulesRoute,
			Args:        []argSpec{workspaceSlugArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.Int("limit", 0, "the most rules the page holds; the hub's default when not given")
				fs.String("cursor", "", "the next_cursor of the page before, to read the page after it")
			},
			Run: func(c *call) error {
				return sendWith(c, changedQuery(c.cmd.Flags(), "limit", "cursor"), nil)
			},
		}),
		declare(command{
			Name:        "rule-delete",
			Description: "Delete a sandbox rule of a workspace; the sandboxes created with it keep it",
			Method:      http.MethodDelete,
			Path:        sandboxRulesRoute + "/:rule-id",
			Args:        []argSpec{workspaceSlugArg, ruleIDArg},
			Auth:        authUser,
			// The hub answers a delete with no body.
			Confirm: func(c *call) any {
				return struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				}{c.args[ruleIDArg.Name], "deleted"}
			},
		}),
	}
}

// newSandboxApproveCommand builds parley sandbox approve. A --scope parley
// can judge is judged with the other usage errors, before any request is
// sent (ADR 02).
func newSandboxApproveCommand() *cobra.Command {
	cmd := declare(command{
		Name:        "approve",
		Description: "Approve an open proposal of a sandbox, adding its rule to the sandbox and with --scope workspace saving it for later sandboxes of the workspace, and print the proposal",
		Method:      http.MethodPost,
		Path:        proposalRoute + "/approve",
		Args:        []argSpec{workspaceSlugArg, sandboxIDArg, proposalIDArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("comment", "", "a comment on the approval")
			fs.Bool("acknowledge-findings", false, "approve a proposal with a high finding of the runtime")
			fs.String("scope", "", "sandbox for this sandbox only, or workspace to save the rule for later sandboxes too; sandbox when not given")
		},
		Run: func(c *call) error {
			return sendWith(c, nil, decisionBody(c.cmd.Flags()))
		},
	})
	return checkFlags(cmd, func(cmd *cobra.Command) error {
		fs := cmd.Flags()
		if !fs.Changed("scope") {
			return nil
		}
		if scope, _ := fs.GetString("scope"); !slices.Contains(approvalScopes, scope) {
			return usageError("%s --scope takes sandbox or workspace, got %q", cmd.CommandPath(), scope)
		}
		return nil
	})
}

// decisionBody is the body of an approve or a reject: a member for each of
// the flags --comment, --acknowledge-findings and --scope the command has
// and was given, and {} when none was, so that the hub's defaults apply to
// the others.
func decisionBody(fs *pflag.FlagSet) map[string]any {
	body := map[string]any{}
	for flag, member := range map[string]string{"comment": "comment", "scope": "scope"} {
		if f := fs.Lookup(flag); f != nil && f.Changed {
			body[member] = f.Value.String()
		}
	}
	if fs.Changed("acknowledge-findings") {
		body["acknowledge_findings"], _ = fs.GetBool("acknowledge-findings")
	}
	return body
}

// changedQuery is a query parameter of the same name for each of flags that
// was given, with its value as given, so that the hub judges it.
func changedQuery(fs *pflag.FlagSet, flags ...string) url.Values {
	query := url.Values{}
	for _, flag := range flags {
		if f := fs.Lookup(flag); f.Changed {
			query.Set(flag, f.Value.String())
		}
	}
	return query
}
