// Package cli is the parley command-line client: its root command and its
// subcommands. It reports its own build through internal/buildinfo and talks
// to the hub only over HTTP, never through the server's code.
package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
)

// usageExitStatus is what Run returns for arguments it cannot parse.
const usageExitStatus = 2

// options holds the settings tests change; Run uses defaultOptions.
type options struct {
	// versionTimeout bounds the version command's request to the hub.
	versionTimeout time.Duration
}

func defaultOptions() options {
	return options{versionTimeout: 5 * time.Second}
}

// Run is parley: it runs the command args name, writes its output to stdout
// and its warnings and errors to stderr, and returns the exit status.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return run(ctx, args, stdout, stderr, defaultOptions())
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, opts options) int {
	root := newRootCommand(opts)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return usageExitStatus
	}
	return 0
}

// newRootCommand builds the command tree afresh, so no flag value survives
// from one Run to the next.
func newRootCommand(opts options) *cobra.Command {
	var endpointURL string
	// The root has no Run of its own, so without a subcommand cobra prints
	// the help on stdout and succeeds.
	root := &cobra.Command{
		Use:   "parley",
		Short: "parley is the command-line client of the Parley hub",
		// Run writes its own error line; cobra's would repeat it, and its
		// usage dump would bury it.
		SilenceErrors: true,
		SilenceUsage:  true,
		// A suggestion would add lines after the one error line Run writes.
		DisableSuggestions: true,
		// Shell completion is not part of the client yet.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.PersistentFlags().StringVar(&endpointURL, "endpoint-url", "",
		"URL of the hub (default: $"+endpointURLEnv+")")
	root.AddCommand(newVersionCommand(&endpointURL, opts.versionTimeout))
	return root
}
