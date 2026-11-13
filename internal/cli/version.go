package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
	"github.com/spf13/cobra"
)

// endpointURLEnv names the variable that gives the hub URL when the flag
// --endpoint-url does not.
const endpointURLEnv = "PARLEY_ENDPOINT_URL"

// versionDocument is what parley version prints.
type versionDocument struct {
	CLI    clientBuild  `json:"cli"`
	Server *serverBuild `json:"server,omitempty"`
}

type clientBuild struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// serverBuild is the body of the hub's GET /version.
type serverBuild struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

func newVersionCommand() *cobra.Command {
	return declare(command{
		Name:        "version",
		Description: "Print the client's build and, given a hub URL, the hub's build",
		Method:      http.MethodGet,
		Path:        "/version",
		Auth:        authNone,
		Run: func(c *call) error {
			info := buildinfo.Get()
			doc := versionDocument{CLI: clientBuild{Version: info.Version, Commit: info.Commit}}

			hubURL := firstNonEmpty(globalFlag(c.cmd, "endpoint-url"), os.Getenv(endpointURLEnv))
			if hubURL == "" {
				file, err := readConfig()
				if err != nil {
					// parley version always succeeds, so a file it cannot
					// parse only costs it the hub URL.
					warn(c.stderr, "%v; printing the client's build alone", err)
				}
				hubURL = file.EndpointURL
			}
			if hubURL != "" {
				// No credential: GET /version needs none. The command's own
				// timeout is shorter than the default, so that a hub that
				// does not answer costs a few seconds and not thirty.
				server, err := hubVersion(c.ctx, newClient(hubURL, "", c.opts.versionTimeout))
				if err != nil {
					// The client's own build is still worth printing, so a hub
					// that cannot be read is a warning and not a failure.
					reason := strings.ReplaceAll(err.Error(), "\n", " ")
					warn(c.stderr, "cannot read the hub's version from %s: %s", hubURL, reason)
				}
				doc.Server = server
			}

			out, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return fmt.Errorf("encode the version document: %w", err)
			}
			fmt.Fprintf(c.stdout, "%s\n", out)
			return nil
		},
	})
}

// hubVersion asks the hub of hub for its build, with GET /version, which
// lies outside /api/v1.
func hubVersion(ctx context.Context, hub *client) (*serverBuild, error) {
	resp, err := hub.send(ctx, apiRequest{method: http.MethodGet, path: "/version", outsideAPI: true})
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusOK {
		return nil, fmt.Errorf("the hub answered %d", resp.status)
	}
	var server *serverBuild
	if err := json.Unmarshal(resp.body, &server); err != nil {
		return nil, fmt.Errorf("decode the answer: %w", err)
	}
	if server == nil {
		return nil, errors.New("decode the answer: null is not a JSON object")
	}
	return server, nil
}
