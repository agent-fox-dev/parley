package cli

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// labelFlag is the repeatable flag of parley issue list whose values are
// sent as one labels parameter.
const labelFlag = "label"

// newIssueCommand builds parley issue: the issues of a workspace's forge
// and their comments.
func newIssueCommand() *cobra.Command {
	return group("issue", "List and read the issues of a workspace's forge and their comments",
		forgeRead(scmRead{
			command: command{
				Name: "list", Description: "List the issues of a workspace's forge, newest first, without its pull requests", Path: "/workspaces/:slug/issues",
				Flags: func(fs *pflag.FlagSet) {
					fs.StringArray(labelFlag, nil, "only the issues that carry this label, repeatable; an issue carries every label given")
				},
			},
			query: slices.Concat([]queryFlag{
				{name: "state", param: "state", usage: "only the issues in this state, open, closed or all (default: open)"},
				{name: "assignee", param: "assignee", usage: "only the issues assigned to this forge user"},
				targetFlag,
			}, pageFlags),
			send: listIssues,
		}),
		forgeRead(scmRead{
			command: command{Name: "get", Description: "Show an issue of a workspace's forge with its first comments", Path: "/workspaces/:slug/issues/:number"},
			query:   []queryFlag{targetFlag},
		}),
		forgeRead(scmRead{
			command: command{Name: "comments", Description: "List the comments of an issue of a workspace's forge, oldest first", Path: "/workspaces/:slug/issues/:number/comments"},
			query:   append([]queryFlag{targetFlag}, pageFlags...),
		}),
	)
}

// listIssues is the run step of parley issue list: the values of --label,
// when it is given, go as one labels parameter, joined by commas, as the
// hub takes them.
func listIssues(c *call, query url.Values) error {
	if fs := c.cmd.Flags(); fs.Changed(labelFlag) {
		labels, err := fs.GetStringArray(labelFlag)
		if err != nil {
			return fmt.Errorf("read --%s: %w", labelFlag, err)
		}
		query.Set("labels", strings.Join(labels, ","))
	}
	return sendWith(c, query, nil)
}
