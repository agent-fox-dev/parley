package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// authLevel says what a command needs before it runs: user and admin need a
// hub URL and a credential, none needs neither.
type authLevel string

const (
	authNone  authLevel = "none"
	authUser  authLevel = "user"
	authAdmin authLevel = "admin"
)

// argSpec is one named positional argument of a command.
type argSpec struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

// command is the declaration of one parley command: everything that differs
// from one command to the next. Every command is declared through declare,
// so that each behaves alike on what this declaration does not say (ADR 02).
type command struct {
	// Name is the command's last word ("get" of "parley probe get").
	Name        string `json:"name"`
	Description string `json:"description"`
	// Method and Path are the route the command calls, Path relative to
	// /api/v1 with :param for its parameters; both are empty for a command
	// that calls no single route.
	Method string `json:"method"`
	Path   string `json:"path"`
	// Args are the named positional arguments, the required ones first.
	Args []argSpec `json:"args"`
	Auth authLevel `json:"auth"`
	// Flags adds the command's own flags, if it has any.
	Flags func(fs *pflag.FlagSet) `json:"-"`
	// Run is the command's run step; it runs only once the command line
	// has been accepted. Without one, the command sends Method and Path
	// and prints the hub's answer.
	Run func(c *call) error `json:"-"`
	// Confirm builds the document a success without a body prints in
	// place of {}.
	Confirm func(c *call) any `json:"-"`
	// ShowsOnce marks a command whose result carries a secret the hub
	// shows only this once.
	ShowsOnce bool `json:"-"`
	// FailOn are the command's --fail-on-… conditions.
	FailOn []failOn `json:"-"`
}

// failOn is a --fail-on-<Name> flag: a condition over the command's result
// that, when the flag is given and the condition holds, ends the command
// with condition_not_met once the result is printed.
type failOn struct {
	// Name follows "fail-on-" in the flag's name.
	Name        string
	Description string
	// Check reports whether the condition holds in result, the document
	// printed on stdout, and the message of condition_not_met when it does.
	Check func(result []byte) (holds bool, message string)
}

// call is one run of a declared command.
type call struct {
	ctx  context.Context
	cmd  *cobra.Command
	decl command
	// args holds the positional arguments by their declared names; an
	// optional argument not given is absent.
	args           map[string]string
	stdout, stderr io.Writer
	opts           options
	// client sends the command's requests to the hub with the settings
	// resolved for it. It is nil for a command whose auth is none, which
	// needs no credential and resolves what it does need itself.
	client *client
}

// The annotations a declared command or a group carries.
const (
	// declarationAnnotation holds the command's declaration as JSON.
	declarationAnnotation = "parley.declaration"
	// groupAnnotation marks a command group.
	groupAnnotation = "parley.group"
)

// declare builds the cobra command of decl. It keeps decl in the command's
// annotations, refuses a wrong number of arguments as a usage error, and
// runs decl.Run with the arguments by name.
func declare(decl command) *cobra.Command {
	// Only strings, booleans and slices of them are encoded, so this
	// cannot fail.
	encoded, _ := json.Marshal(decl)
	cmd := &cobra.Command{
		Use:         useLine(decl),
		Short:       decl.Description,
		Args:        checkArgs(decl.Args),
		Annotations: map[string]string{declarationAnnotation: string(encoded)},
		RunE: func(cmd *cobra.Command, args []string) error {
			named := make(map[string]string, len(args))
			for i, value := range args {
				named[decl.Args[i].Name] = value
			}
			c := &call{
				ctx:    cmd.Context(),
				cmd:    cmd,
				decl:   decl,
				args:   named,
				stdout: cmd.OutOrStdout(),
				stderr: cmd.ErrOrStderr(),
				opts:   optionsFrom(cmd.Context()),
			}
			// Both settings are resolved before the run step, so a command
			// that lacks one fails before it sends anything.
			if decl.Auth != authNone {
				s, err := resolveSettings(cmd)
				if err != nil {
					return err
				}
				c.client = newClient(s.hubURL, s.credential)
			}
			if decl.Run == nil {
				return sendAndPrint(c)
			}
			return decl.Run(c)
		},
	}
	if decl.Flags != nil {
		decl.Flags(cmd.Flags())
	}
	for _, f := range decl.FailOn {
		cmd.Flags().Bool(f.flag(), false, f.Description)
	}
	return cmd
}

// group builds a command group: a command that only holds children. Run
// alone it prints its help; given a word that is none of its children, it
// fails with a usage error, where cobra would print the help.
func group(name, description string, children ...*cobra.Command) *cobra.Command {
	g := &cobra.Command{
		Use:         name,
		Short:       description,
		Annotations: map[string]string{groupAnnotation: "true"},
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageError("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return nil
		},
		// cobra checks the arguments only of a command that runs, so a group
		// runs, to print its help.
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	g.AddCommand(children...)
	return g
}

// useLine is the command's name followed by its arguments, <required> and
// [optional].
func useLine(decl command) string {
	words := []string{decl.Name}
	for _, a := range decl.Args {
		if a.Required {
			words = append(words, "<"+a.Name+">")
		} else {
			words = append(words, "["+a.Name+"]")
		}
	}
	return strings.Join(words, " ")
}

// checkArgs refuses fewer arguments than specs requires and more than it
// names, as a usage error.
func checkArgs(specs []argSpec) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		for i, spec := range specs {
			if spec.Required && i >= len(args) {
				return usageError("%s needs the argument <%s>", cmd.CommandPath(), spec.Name)
			}
		}
		if len(args) > len(specs) {
			if len(specs) == 0 {
				return usageError("%s takes no arguments, got %q", cmd.CommandPath(), args[0])
			}
			return usageError("%s takes at most %d arguments, got the extra argument %q", cmd.CommandPath(), len(specs), args[len(specs)])
		}
		return nil
	}
}

// optionsKey is the context key of the options of the current run.
type optionsKey struct{}

// withOptions is ctx carrying opts, so that a declared command finds them.
func withOptions(ctx context.Context, opts options) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

// optionsFrom is the options ctx carries, or the defaults.
func optionsFrom(ctx context.Context) options {
	if opts, ok := ctx.Value(optionsKey{}).(options); ok {
		return opts
	}
	return defaultOptions()
}

// globalFlag is the value of the global string flag name as cmd parsed it.
func globalFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}
