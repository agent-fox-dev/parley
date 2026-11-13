// Package cli is the parley command-line client: its root command and its
// subcommands. It reports its own build through internal/buildinfo and talks
// to the hub only over HTTP, never through the server's code.
package cli

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// options holds the settings tests change; RunWith starts from
// defaultOptions.
type options struct {
	// versionTimeout bounds the version command's request to the hub.
	versionTimeout time.Duration
	// stdin is where parley login --api-key - reads the credential.
	stdin io.Reader
	// openBrowser opens a URL in the user's browser.
	openBrowser func(url string) error
	// loginWait bounds how long parley login waits for the browser's
	// redirect.
	loginWait time.Duration
	// extra builds commands added to the root on each run, beside parley's
	// own; tests add probe commands with it.
	extra []func() *cobra.Command
	// requestTimeout bounds each request to the hub of a command that
	// states no timeout of its own (defaultRequestTimeout).
	requestTimeout time.Duration
	// pollInterval, when not zero, replaces the --poll-interval of a wait,
	// so that tests poll faster than the flag's smallest value, 1s.
	pollInterval time.Duration
}

func defaultOptions() options {
	return options{
		versionTimeout: 5 * time.Second,
		stdin:          os.Stdin,
		openBrowser:    openSystemBrowser,
		loginWait:      120 * time.Second,
		requestTimeout: defaultRequestTimeout,
	}
}

// Hooks replace what parley takes from the process, for a caller that runs it
// in-process. A zero field keeps the default.
type Hooks struct {
	// Stdin is where parley login --api-key - reads the credential
	// (default os.Stdin).
	Stdin io.Reader
	// OpenBrowser opens the provider's sign-in page for parley login
	// (default: the system's opener, see openSystemBrowser).
	OpenBrowser func(url string) error
	// LoginWait bounds how long parley login waits for the browser to come
	// back (default 120 s).
	LoginWait time.Duration
}

// Run is parley: it runs the command args name, writes its output to stdout
// and its warnings and errors to stderr, and returns the exit status.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunWith(ctx, args, stdout, stderr, Hooks{})
}

// RunWith is Run with the hooks h.
func RunWith(ctx context.Context, args []string, stdout, stderr io.Writer, h Hooks) int {
	opts := defaultOptions()
	if h.Stdin != nil {
		opts.stdin = h.Stdin
	}
	if h.OpenBrowser != nil {
		opts.openBrowser = h.OpenBrowser
	}
	if h.LoginWait > 0 {
		opts.loginWait = h.LoginWait
	}
	return run(ctx, args, stdout, stderr, opts)
}

// run executes the command tree with args, prints the error the command
// ended with, if any, as one error envelope on stderr, and returns the exit
// status that error maps to.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, opts options) int {
	root := newRootCommand(opts)
	if args == nil {
		// cobra would read the process's own arguments instead.
		args = []string{}
	}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(withOptions(ctx, opts))
	if err != nil {
		printError(stderr, err)
	}
	return exitCode(err)
}

// newRootCommand builds the command tree afresh, so no flag value survives
// from one Run to the next.
func newRootCommand(opts options) *cobra.Command {
	// Without a subcommand the root, a group, prints the help on stdout and
	// succeeds.
	root := group("parley", "parley is the command-line client of the Parley hub")
	// run prints the error as an envelope, alone: cobra's own error line
	// and usage text would make stderr more than one JSON document.
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.DisableSuggestions = true
	// Shell completion is not part of the client yet.
	root.CompletionOptions = cobra.CompletionOptions{DisableDefaultCmd: true}
	// Every command inherits the global flags and this handler of a flag
	// that is unknown or does not parse.
	root.PersistentFlags().String("endpoint-url", "", "URL of the hub (default: $"+endpointURLEnv+", then endpoint_url in the config file)")
	root.PersistentFlags().String("api-key", "", "credential presented to the hub (default: $"+apiKeyEnv+", then api_key in the config file); parley login takes it from this flag alone and stores it (- reads it from stdin)")
	root.PersistentFlags().Bool("json", false, "print the help as JSON")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError("%v", err) })

	installHelp(root)
	root.AddCommand(newVersionCommand(), newLoginCommand(), newUserCommand(), newOrgsCommand(), newKeysCommand(), newAdminCommand())
	for _, build := range opts.extra {
		root.AddCommand(build())
	}
	return root
}

// newAdminCommand builds parley admin: the commands of the routes only an
// admin may call.
func newAdminCommand() *cobra.Command {
	return group("admin", "Administer the hub's users, organizations and API keys", newAdminUsersCommand(), newAdminOrgsCommand(), newAdminKeysCommand())
}
