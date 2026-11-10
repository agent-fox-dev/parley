package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Auth is what a command needs to run: none, or a hub URL and a credential
// (user, admin).
type Auth string

// The auth values.
const (
	AuthNone  Auth = "none"
	AuthUser  Auth = "user"
	AuthAdmin Auth = "admin"
)

// Arg is a named argument of a command.
type Arg struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

// Spec declares a command: its word, its description, the route it calls
// (method and path relative to /api/v1 with :param, or none), its arguments,
// its auth, its flags and its run step. Every command is declared through
// Declare, so that every command gets the same argument checks, settings,
// output and help.
type Spec struct {
	Use         string
	Description string
	Method      string
	Path        string
	Args        []Arg
	Auth        Auth
	Flags       func(fs *pflag.FlagSet)
	// Run is the command's run step; nil sends the declared request
	// through the client and prints its result.
	Run func(c *Call) error
	// Confirm is the document printed when the route answers without a
	// body; nil prints {}.
	Confirm func(args map[string]string) any
	// ShowOnce marks a result that carries a secret shown once.
	ShowOnce bool
	// FailOn are the command's --fail-on-… flags.
	FailOn []FailOn
}

// FailOn is a --fail-on-… flag: when it is given and Holds is true of the
// result, the result is printed and the command ends with
// condition_not_met and Message.
type FailOn struct {
	Name    string
	Usage   string
	Holds   func(result []byte) bool
	Message func(result []byte) string
}

// Call is one run of a declared command.
type Call struct {
	Ctx context.Context
	Cmd *cobra.Command
	// Args are the named arguments' values.
	Args   map[string]string
	Stdout io.Writer
	Stderr io.Writer
	// Client sends the command's requests; nil for a command whose auth is
	// none.
	Client *Client
}

// The keys of a declaration in the cobra command's Annotations.
const (
	annotMethod = "parley.method"
	annotPath   = "parley.path"
	annotAuth   = "parley.auth"
	annotArgs   = "parley.args"
)

// ranStep is set while a declared command's run step runs, so that an error
// cobra raises before it (a flag or argument problem) is told from one the
// step returns.
type ranStepKey struct{}

// Declare builds the cobra command of a declaration.
func Declare(s Spec) *cobra.Command {
	use := s.Use
	for _, a := range s.Args {
		if a.Required {
			use += " <" + a.Name + ">"
		} else {
			use += " [" + a.Name + "]"
		}
	}
	argsJSON, _ := json.Marshal(s.Args)
	cmd := &cobra.Command{
		Use:   use,
		Short: s.Description,
		Annotations: map[string]string{
			annotMethod: s.Method,
			annotPath:   s.Path,
			annotAuth:   string(s.Auth),
			annotArgs:   string(argsJSON),
		},
		Args: func(cmd *cobra.Command, args []string) error {
			required := 0
			for _, a := range s.Args {
				if a.Required {
					required++
				}
			}
			if len(args) < required || len(args) > len(s.Args) {
				return UsageError("%s takes %s, got %d", commandName(cmd), argCount(required, len(s.Args)), len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if marker, ok := cmd.Context().Value(ranStepKey{}).(*bool); ok {
				*marker = true
			}
			call := &Call{Ctx: cmd.Context(), Cmd: cmd, Args: map[string]string{}, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}
			for i, v := range args {
				call.Args[s.Args[i].Name] = v
			}
			if s.Auth == AuthUser || s.Auth == AuthAdmin {
				hub, cred, err := resolveSettings(cmd)
				if err != nil {
					return err
				}
				if call.Client, err = newClient(hub, cred); err != nil {
					return err
				}
			}
			if s.Run == nil {
				return runDefault(call, &s)
			}
			return s.Run(call)
		},
	}
	if s.Flags != nil {
		s.Flags(cmd.Flags())
	}
	for _, f := range s.FailOn {
		cmd.Flags().Bool(f.Name, false, f.Usage)
	}
	return cmd
}

// expandPath fills the :param segments of a declared path with the named
// arguments' values.
func expandPath(pattern string, args map[string]string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if name, ok := strings.CutPrefix(p, ":"); ok {
			parts[i] = args[name]
		}
	}
	return strings.Join(parts, "/")
}

// runDefault is the run step of a command that declares none: it sends the
// declared request through the shared client, prints the result (or {},
// or the confirmation document, for a success without a body), warns about
// a secret shown once, and checks the --fail-on-… flags.
func runDefault(c *Call, s *Spec) error {
	resp, err := c.Client.Send(c.Ctx, s.Method, expandPath(s.Path, c.Args), nil, nil)
	if err != nil {
		return err
	}
	result, err := resp.Result()
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(result)) == 0 && s.Confirm != nil {
		err = printDocument(c.Stdout, s.Confirm(c.Args))
	} else {
		err = printResult(c.Stdout, result)
	}
	if err != nil {
		return err
	}
	if s.ShowOnce {
		warnf(c.Stderr, "%s", showOnceWarning)
	}
	for _, f := range s.FailOn {
		if on, _ := c.Cmd.Flags().GetBool(f.Name); on && f.Holds(result) {
			return ConditionNotMet("%s", f.Message(result))
		}
	}
	return nil
}

func argCount(required, total int) string {
	switch {
	case required == total && total == 0:
		return "no arguments"
	case required == total:
		return pluralArgs(total)
	}
	return "from " + itoa(required) + " to " + pluralArgs(total)
}

func pluralArgs(n int) string {
	if n == 1 {
		return "1 argument"
	}
	return itoa(n) + " arguments"
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// commandName is the command's words after parley, joined by single spaces.
func commandName(cmd *cobra.Command) string {
	return strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()))
}

// Group builds a command group: alone it prints its help; an unknown word
// under it is a usage error.
func Group(use, description string, children ...*cobra.Command) *cobra.Command {
	g := &cobra.Command{
		Use:   use,
		Short: description,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return UsageError("unknown command %q for %q", args[0], commandName(cmd))
			}
			return cmd.Help()
		},
	}
	g.AddCommand(children...)
	return g
}
