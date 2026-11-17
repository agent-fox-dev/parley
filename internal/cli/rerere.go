package cli

import (
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// rerereRoute is the route of the conflict resolutions a carry-patch
// workspace's rebuilds remember.
const rerereRoute = "/workspaces/:slug/rerere"

// pathspecArg is the argument that names remembered resolutions by their id
// or by the path they were recorded at.
var pathspecArg = argSpec{Name: "id-or-path", Required: true}

// newRerereCommand builds parley rerere: the conflict resolutions the
// rebuilds of a carry-patch workspace remember and replay.
func newRerereCommand() *cobra.Command {
	return group("rerere", "List and forget the conflict resolutions the rebuilds of a carry-patch workspace remember and replay",
		declare(command{
			Name:        "list",
			Description: "List the conflict resolutions a carry-patch workspace remembers, the most recently recorded first",
			Method:      http.MethodGet,
			Path:        rerereRoute,
			Args:        []argSpec{slugArg},
			Auth:        authUser,
		}),
		declare(command{
			Name:        "forget",
			Description: "Forget the remembered resolution with this id, or every one recorded at this path, so the next rebuild stops at the conflict again",
			Method:      http.MethodDelete,
			Path:        rerereRoute + "/*" + pathspecArg.Name,
			Args:        []argSpec{slugArg, pathspecArg},
			Auth:        authUser,
			Run:         forgetResolutions,
			Confirm: func(c *call) any {
				return struct {
					Status   string `json:"status"`
					Pathspec string `json:"pathspec"`
				}{"forgotten", c.args[pathspecArg.Name]}
			},
		}),
	)
}

// forgetResolutions is the run step of parley rerere forget. The hub reads
// the rest of the path after /rerere/ as the id or the path, slashes
// included, so the argument is sent as segments of the route, each encoded
// on its own, where a :param would send it as one segment with its slashes
// encoded.
func forgetResolutions(c *call) error {
	parts := strings.Split(c.args[pathspecArg.Name], "/")
	for i, part := range parts {
		parts[i] = pathSegment(part)
	}
	path := routePath(rerereRoute, c.args) + "/" + strings.Join(parts, "/")
	resp, err := c.client.request(c.ctx, c.decl.Method, path, nil, nil)
	if err != nil {
		return err
	}
	return c.printAnswer(resp)
}
