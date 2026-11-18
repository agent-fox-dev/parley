package cli

import (
	"strings"

	"github.com/agent-fox-dev/parley/internal/forge/number"
	"github.com/spf13/cobra"
)

// The flags the forge reads share.
var (
	targetFlag = queryFlag{name: "target", param: "target", usage: "the remote the read works on, origin or upstream (default: origin)"}
	// pageFlags are the flags of a forge read that answers a list.
	pageFlags = []queryFlag{limitFlag, cursorFlag}
)

// newForgeCommand builds parley forge: what the hub derives about a
// workspace's forge, and the forge's repository.
func newForgeCommand() *cobra.Command {
	return group("forge", "Show how the hub reaches a workspace's forge, and the forge's repository",
		forgeRead(scmRead{command: command{Name: "status", Description: "Show the forge, API URL and credential kind the hub derives for a workspace, without asking the forge", Path: "/workspaces/:slug/forge"}}),
		forgeRead(scmRead{
			command: command{Name: "repository", Description: "Show the forge's repository of a workspace: its name, default branch, visibility and the caller's permissions", Path: "/workspaces/:slug/forge/repository"},
			query:   []queryFlag{targetFlag},
		}),
	)
}

// forgeRead builds r, a read of a workspace's forge: GET of its path with
// the workspace as the argument <slug> and, where the path names one, the
// issue or pull request number as <number>, and a query parameter for each
// flag of r.query given. The number is checked with the rule the hub
// applies, as the command line is, so that a bad one is a usage error
// before any request (ADR 02).
func forgeRead(r scmRead) *cobra.Command {
	r.Args = []argSpec{slugArg}
	if strings.Contains(r.Path, ":"+numberArg.Name) {
		r.Args = append(r.Args, numberArg)
	}
	cmd := r.build()
	if len(r.Args) == 1 {
		return cmd
	}
	checkLine := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		if _, err := number.Parse(args[1]); err != nil {
			return usageError("%s takes <number> %q: %v", cmd.CommandPath(), args[1], err)
		}
		return nil
	}
	return cmd
}
