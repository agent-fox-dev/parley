package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

// versionTimeout bounds the request for the hub's build.
var versionTimeout = 5 * time.Second

type clientBuild struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type serverBuild struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

type versionDoc struct {
	CLI    clientBuild  `json:"cli"`
	Server *serverBuild `json:"server,omitempty"`
}

func newVersionCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the client's build and, with a hub URL, the hub's build",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			doc := versionDoc{CLI: clientBuild{Version: info.Version, Commit: info.Commit}}
			if hub := hubURL(flags); hub != "" {
				server, err := fetchServerBuild(cmd.Context(), hub)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: cannot read the hub's version from %s: %v\n", hub, err)
				} else {
					doc.Server = server
				}
			}
			return printJSON(cmd.OutOrStdout(), doc)
		},
	}
}

// hubURL is the first non-empty value of --endpoint-url and
// PARLEY_ENDPOINT_URL.
func hubURL(flags *globalFlags) string {
	if flags.endpointURL != "" {
		return flags.endpointURL
	}
	return os.Getenv("PARLEY_ENDPOINT_URL")
}

// fetchServerBuild sends GET <hub>/version without a credential and decodes
// the build the hub answers.
func fetchServerBuild(ctx context.Context, hub string) (*serverBuild, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hub, "/")+"/version", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the hub answered %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("the hub's answer is not a JSON object")
	}
	var build serverBuild
	if err := json.Unmarshal(trimmed, &build); err != nil {
		return nil, fmt.Errorf("the hub's answer is not a JSON object: %v", err)
	}
	return &build, nil
}

// printJSON writes v as one JSON document indented by 2 spaces and followed
// by a newline.
func printJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}
