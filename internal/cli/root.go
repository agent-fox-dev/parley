// Package cli is the parley command-line client: the root command and one
// file per command group.
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// globalFlags are the flags every command reads.
type globalFlags struct {
	endpointURL string
}

func newRootCommand(flags *globalFlags) *cobra.Command {
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
	return root
}

// Run runs the parley CLI with the given arguments and returns its exit
// status.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var flags globalFlags
	root := newRootCommand(&flags)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
