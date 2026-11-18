package cli

import "github.com/spf13/cobra"

// newPRCommand builds parley pr: the pull requests of a workspace's forge,
// with their state, changed files, check runs and reviews.
func newPRCommand() *cobra.Command {
	targetOnly := []queryFlag{targetFlag}
	page := append([]queryFlag{targetFlag}, pageFlags...)
	return group("pr", "Read the pull requests of a workspace's forge, their state, changed files, checks and reviews",
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
	)
}
