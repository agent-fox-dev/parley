// Package cli is the parley command-line client: the root command and one
// file per command group.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// Options are what a command needs beyond its arguments and output; tests
// replace them.
type Options struct {
	// Stdin is read by parley login --api-key -.
	Stdin io.Reader
	// Opener opens a URL in the browser; nil means the system's.
	Opener func(url string) error
	// LoginWait bounds the wait for the browser's redirect; 0 means
	// DefaultLoginWait.
	LoginWait time.Duration
}

// globalFlags are the flags every command reads.
type globalFlags struct {
	endpointURL string
}

func newRootCommand(flags *globalFlags, opts Options) *cobra.Command {
	root := &cobra.Command{
		Use:           "parley",
		Short:         "parley is the command-line client of the Parley hub",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.PersistentFlags().StringVar(&flags.endpointURL, "endpoint-url", "", "the hub URL (default from PARLEY_ENDPOINT_URL)")
	root.AddCommand(newVersionCommand(flags))
	root.AddCommand(newLoginCommand(flags, opts))
	return root
}

// Run runs the parley CLI with the given arguments and returns its exit
// status.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunWith(ctx, args, Options{Stdin: os.Stdin}, stdout, stderr)
}

// RunWith is Run with the options a test sets.
func RunWith(ctx context.Context, args []string, opts Options, stdout, stderr io.Writer) int {
	var flags globalFlags
	root := newRootCommand(&flags, opts)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			fmt.Fprintf(stderr, "%s", ee.envelope)
			return ee.code
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
