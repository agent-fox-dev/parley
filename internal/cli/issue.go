package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/forge/number"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// labelFlag is the repeatable flag of parley issue list whose values are
// sent as one labels parameter, and of the issue writes whose values are
// sent as the labels list.
const labelFlag = "label"

// issueFields maps the flags of an issue write that give a member of its
// body to that member; the text input gives body.
var issueFields = map[string]string{"title": "title", "state": "state", labelFlag: "labels", "assignee": "assignees"}

// newIssueCommand builds parley issue: the issues of a workspace's forge,
// their comments and their labels.
func newIssueCommand() *cobra.Command {
	return group("issue", "List, read, open, change, close, comment on and label the issues of a workspace's forge",
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
		newIssueCreateCommand(),
		newIssueUpdateCommand(),
		forgeWrite{
			command: command{
				Name: "close", Description: "Close an issue of a workspace's forge, with a comment first when --comment is given",
				Method: http.MethodPost, Path: "/workspaces/:slug/issues/:number/close",
				Flags: func(fs *pflag.FlagSet) { fs.String("comment", "", "post this comment on the issue before closing it") },
			},
			body: closeBody,
		}.build(),
		newIssueCommentCommand(),
		forgeWrite{
			command: command{
				Name: "label", Description: "Add labels to an issue of a workspace's forge, keeping the labels it has",
				Method: http.MethodPost, Path: "/workspaces/:slug/issues/:number/labels",
				Args:    []argSpec{{Name: "label", Required: true, Repeats: true}},
				Confirm: labeled,
			},
			body: func(c *call) any { return labelsOf(c) },
		}.build(),
		forgeWrite{
			command: command{
				Name: "unlabel", Description: "Remove a label from an issue of a workspace's forge",
				Method: http.MethodDelete, Path: "/workspaces/:slug/issues/:number/labels/:label",
				Args:    []argSpec{{Name: "label", Required: true}},
				Confirm: unlabeled,
			},
		}.build(),
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

// issueFlags adds the flags of the fields of an issue that a create and an
// update both set.
func issueFlags(fs *pflag.FlagSet) {
	fs.StringArray(labelFlag, nil, "a label of the issue, repeatable; an update replaces the issue's labels with those given")
	fs.StringArray("assignee", nil, "a forge user the issue is assigned to, repeatable; an update replaces the issue's assignees with those given")
}

// issueBody is the body of an issue create or update: a member for each
// flag given and the text, when given, as body.
func issueBody(c *call, text *textInput) any {
	body := map[string]any{}
	givenFields(c.cmd.Flags(), body, issueFields)
	if text.given {
		body["body"] = text.text
	}
	return body
}

func newIssueCreateCommand() *cobra.Command {
	text := &textInput{what: "the issue's body"}
	return forgeWrite{
		command: command{
			Name: "create", Description: "Open an issue on a workspace's forge",
			Method: http.MethodPost, Path: "/workspaces/:slug/issues",
			Flags: func(fs *pflag.FlagSet) {
				fs.String("title", "", "the issue's title")
				issueFlags(fs)
				// It fails only for a flag the set does not hold, and the
				// line above adds it.
				_ = cobra.MarkFlagRequired(fs, "title")
			},
		},
		text: text,
		body: func(c *call) any { return issueBody(c, text) },
	}.build()
}

func newIssueUpdateCommand() *cobra.Command {
	text := &textInput{what: "the issue's new body"}
	return forgeWrite{
		command: command{
			Name: "update", Description: "Change the title, body, state, labels or assignees of an issue of a workspace's forge",
			Method: http.MethodPatch, Path: "/workspaces/:slug/issues/:number",
			Flags: func(fs *pflag.FlagSet) {
				fs.String("title", "", "the issue's new title")
				fs.String("state", "", "the issue's new state, open or closed")
				issueFlags(fs)
			},
		},
		text: text,
		// An update that changes nothing is a mistake of the command line,
		// not a request for the hub.
		check: func(cmd *cobra.Command) error {
			fs := cmd.Flags()
			for flag := range issueFields {
				if fs.Changed(flag) {
					return nil
				}
			}
			if text.given {
				return nil
			}
			return usageError("%s needs at least one of --title, --%s, --%s, --state, --%s and --assignee", cmd.CommandPath(), bodyFlag, bodyFileFlag, labelFlag)
		},
		body: func(c *call) any { return issueBody(c, text) },
	}.build()
}

// closeBody is the body of parley issue close: the comment when --comment
// is given, none otherwise.
func closeBody(c *call) any {
	fs := c.cmd.Flags()
	if !fs.Changed("comment") {
		return nil
	}
	comment, _ := fs.GetString("comment")
	return map[string]string{"comment": comment}
}

func newIssueCommentCommand() *cobra.Command {
	text := &textInput{what: "the comment", required: true}
	return forgeWrite{
		command: command{
			Name: "comment", Description: "Post a comment on an issue of a workspace's forge",
			Method: http.MethodPost, Path: "/workspaces/:slug/issues/:number/comments",
		},
		text: text,
		body: func(*call) any { return map[string]string{"body": text.text} },
	}.build()
}

// labelsOf is the labels of parley issue label, each word after <number>
// one label: the declared argument joins them into one.
func labelsOf(c *call) []string {
	return c.cmd.Flags().Args()[2:]
}

// issueNumber is the <number> of the command, checked with the command line.
func issueNumber(c *call) int64 {
	n, _ := number.Parse(c.args[numberArg.Name])
	return n
}

// labeled is what parley issue label prints for the hub's answer without a
// body.
func labeled(c *call) any {
	return struct {
		Status string   `json:"status"`
		Number int64    `json:"number"`
		Labels []string `json:"labels"`
	}{"labeled", issueNumber(c), labelsOf(c)}
}

// unlabeled is what parley issue unlabel prints for the hub's answer
// without a body.
func unlabeled(c *call) any {
	return struct {
		Status string `json:"status"`
		Number int64  `json:"number"`
		Label  string `json:"label"`
	}{"unlabeled", issueNumber(c), c.args["label"]}
}
