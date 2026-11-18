package cli

import (
	"net/http"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// newPRCommand builds parley pr: the pull requests of a workspace's forge,
// with their state, changed files, check runs and reviews, and opening and
// commenting on them.
func newPRCommand() *cobra.Command {
	targetOnly := []queryFlag{targetFlag}
	page := append([]queryFlag{targetFlag}, pageFlags...)
	return group("pr", "Read the pull requests of a workspace's forge, their state, changed files, checks and reviews, and open and comment on them",
		forgeRead(scmRead{
			command: command{Name: "get", Description: "Show a pull request of a workspace's forge", Path: "/workspaces/:slug/pulls/:number"},
			query:   targetOnly,
		}),
		forgeRead(scmRead{
			command: command{Name: "state", Description: "Show whether a pull request of a workspace's forge is open, closed or merged, and its head", Path: "/workspaces/:slug/pulls/:number/state"},
			query:   targetOnly,
		}),
		forgeRead(scmRead{
			command: command{Name: "files", Description: "List the files a pull request of a workspace's forge changes", Path: "/workspaces/:slug/pulls/:number/files"},
			query:   page,
		}),
		forgeRead(scmRead{
			command: command{Name: "checks", Description: "List the check runs of the head of a pull request of a workspace's forge", Path: "/workspaces/:slug/pulls/:number/checks"},
			query:   page,
		}),
		forgeRead(scmRead{
			command: command{Name: "reviews", Description: "List the reviews of a pull request of a workspace's forge, oldest first", Path: "/workspaces/:slug/pulls/:number/reviews"},
			query:   page,
		}),
		newPRCreateCommand(),
		newPRCommentCommand(),
	)
}

// prFields maps the flags of parley pr create that give a member of its
// body to that member; the text input gives body and --draft draft.
var prFields = map[string]string{"title": "title", "head": "head", "base": "base"}

func newPRCreateCommand() *cobra.Command {
	text := &textInput{what: "the pull request's description"}
	return forgeWrite{
		command: command{
			Name: "create", Description: "Open a pull request on a workspace's forge from a branch the forge holds",
			Method: http.MethodPost, Path: "/workspaces/:slug/pulls",
			Flags: func(fs *pflag.FlagSet) {
				fs.String("title", "", "the pull request's title")
				fs.String("head", "", "the branch whose commits the pull request proposes, <owner>:<branch> for a fork's on GitHub")
				fs.String("base", "", "the branch the pull request proposes them for")
				fs.Bool("draft", false, "open the pull request as a draft; sent only when given, so the forge's default applies otherwise")
				for flag := range prFields {
					// It fails only for a flag the set does not hold, and the
					// lines above add them.
					_ = cobra.MarkFlagRequired(fs, flag)
				}
			},
		},
		text: text,
		body: func(c *call) any {
			fs := c.cmd.Flags()
			body := map[string]any{}
			givenFields(fs, body, prFields)
			if text.given {
				body["body"] = text.text
			}
			if fs.Changed("draft") {
				body["draft"], _ = fs.GetBool("draft")
			}
			return body
		},
	}.build()
}

func newPRCommentCommand() *cobra.Command {
	text := &textInput{what: "the comment", required: true}
	return forgeWrite{
		command: command{
			Name: "comment", Description: "Post a comment on a pull request of a workspace's forge",
			Method: http.MethodPost, Path: "/workspaces/:slug/pulls/:number/comments",
		},
		text: text,
		body: func(*call) any { return map[string]string{"body": text.text} },
	}.build()
}
