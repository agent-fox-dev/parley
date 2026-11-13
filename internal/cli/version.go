package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
	"github.com/spf13/cobra"
)

// endpointURLEnv names the variable that gives the hub URL when the flag
// --endpoint-url does not.
const endpointURLEnv = "PARLEY_ENDPOINT_URL"

// maxVersionBody bounds how much of the hub's answer is read.
const maxVersionBody = 1 << 20

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

			hubURL := globalFlag(c.cmd, "endpoint-url")
			if hubURL == "" {
				hubURL = os.Getenv(endpointURLEnv)
			}
			if hubURL != "" {
				server, err := hubVersion(c.ctx, hubURL, c.opts.versionTimeout)
				if err != nil {
					// The client's own build is still worth printing, so a hub
					// that cannot be read is a warning and not a failure.
					reason := strings.ReplaceAll(err.Error(), "\n", " ")
					fmt.Fprintf(c.stderr, "warning: cannot read the hub's version from %s: %s\n", hubURL, reason)
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

// hubVersion asks the hub at hubURL for its build, without a credential.
func hubVersion(ctx context.Context, hubURL string, timeout time.Duration) (*serverBuild, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hubURL, "/")+"/version", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the hub answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVersionBody))
	if err != nil {
		return nil, fmt.Errorf("read the answer: %w", err)
	}
	var server *serverBuild
	if err := json.Unmarshal(body, &server); err != nil {
		return nil, fmt.Errorf("decode the answer: %w", err)
	}
	if server == nil {
		return nil, errors.New("decode the answer: null is not a JSON object")
	}
	return server, nil
}
