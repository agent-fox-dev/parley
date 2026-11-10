// Package cli is the parley command-line client: the root command and one
// file per command group.
package cli

import (
	"context"
	"errors"
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

// globalFlags are the flags every command accepts, before or after its
// words.
type globalFlags struct {
	endpointURL string
	apiKey      string
	json        bool
}

func newRootCommand(flags *globalFlags, opts Options) *cobra.Command {
	root := &cobra.Command{
		Use:           "parley",
		Short:         "parley is the command-line client of the Parley hub",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return UsageError("unknown command %q", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return UsageError("%v", err)
	})
	root.PersistentFlags().StringVar(&flags.endpointURL, "endpoint-url", "", "the hub URL (default from PARLEY_ENDPOINT_URL, then the configuration file)")
	root.PersistentFlags().StringVar(&flags.apiKey, "api-key", "", "the credential (default from PARLEY_API_KEY, then the configuration file)")
	root.PersistentFlags().BoolVar(&flags.json, "json", false, "print help as JSON")
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
	return run(ctx, args, opts, stdout, stderr)
}

// run runs the root with extra commands a test declares.
func run(ctx context.Context, args []string, opts Options, stdout, stderr io.Writer, extra ...*cobra.Command) int {
	var flags globalFlags
	root := newRootCommand(&flags, opts)
	root.AddCommand(extra...)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	ranStep := false
	err := root.ExecuteContext(context.WithValue(ctx, ranStepKey{}, &ranStep))
	if err == nil {
		return 0
	}
	var hub *HubError
	var ce *Error
	if !ranStep && !errors.As(err, &hub) && !errors.As(err, &ce) {
		// cobra refused the command line before any run step.
		err = UsageError("%v", err)
	}
	PrintError(stderr, err)
	return ExitCode(err)
}
