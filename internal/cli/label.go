package cli

import (
	"net/http"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// newLabelCommand builds parley label: the labels of a workspace's forge
// repository.
func newLabelCommand() *cobra.Command {
	return group("label", "Make sure a label exists in a workspace's forge repository",
		forgeWrite{
			command: command{
				Name: "create", Description: "Create a label in a workspace's forge repository, or show it unchanged when it exists",
				Method: http.MethodPost, Path: "/workspaces/:slug/labels",
				Args: []argSpec{{Name: "name", Required: true}},
				Flags: func(fs *pflag.FlagSet) {
					fs.String("color", "", "the label's colour, six hex digits with or without # (default: the hub's)")
					fs.String("description", "", "the label's description")
				},
			},
			body: func(c *call) any {
				body := map[string]any{"name": c.args["name"]}
				givenFields(c.cmd.Flags(), body, map[string]string{"color": "color", "description": "description"})
				return body
			},
		}.build(),
	)
}
