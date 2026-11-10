package cli

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// commandDoc is a command's JSON help, built from its declaration.
type commandDoc struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Method      *string   `json:"method"`
	Path        *string   `json:"path"`
	Args        []Arg     `json:"args"`
	Flags       []flagDoc `json:"flags"`
	Auth        string    `json:"auth"`
}

// flagDoc is one flag of a command's JSON help.
type flagDoc struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Default     string `json:"default"`
	Description string `json:"description"`
}

// groupDoc is the JSON help of a command group or of parley itself.
type groupDoc struct {
	Commands []commandDoc `json:"commands"`
}

// installHelp makes help a step that never resolves settings, reads the
// configuration file or sends a request: --help and parley help print the
// same text, or with --json the command's or the group's JSON help.
func installHelp(root *cobra.Command, flags *globalFlags) {
	text := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if !flags.json {
			text(cmd, args)
			return
		}
		var err error
		if isGroup(cmd) {
			err = printDocument(cmd.OutOrStdout(), describeGroup(cmd))
		} else {
			err = printDocument(cmd.OutOrStdout(), describe(cmd))
		}
		if err != nil {
			PrintError(cmd.ErrOrStderr(), err)
		}
	})
	root.SetHelpCommand(newHelpCommand())
	root.CompletionOptions.DisableDefaultCmd = true
}

// newHelpCommand is parley help [command]: the help of the command its words
// name, or of parley itself.
func newHelpCommand() *cobra.Command {
	return Declare(Spec{
		Use:         "help",
		Description: "Print the help of a command, or with --json its description as JSON",
		Args:        []Arg{{Name: "command", Required: false}},
		Variadic:    true,
		Auth:        AuthNone,
		Run: func(c *Call) error {
			words := strings.Fields(c.Args["command"])
			target, rest, err := c.Cmd.Root().Find(words)
			if err != nil || len(rest) > 0 {
				return UsageError("no command %q: run parley help to list the commands", strings.Join(words, " "))
			}
			target.InitDefaultHelpFlag()
			return target.Help()
		},
	})
}

// isGroup reports whether a command is parley itself or a command group: a
// command with subcommands.
func isGroup(cmd *cobra.Command) bool {
	return cmd.HasSubCommands()
}

// describe builds a command's JSON help from the annotations Declare stored
// and its flags: its own and the global ones, without --help, sorted by
// name.
func describe(cmd *cobra.Command) commandDoc {
	d := commandDoc{
		Name:        commandName(cmd),
		Description: cmd.Short,
		Args:        []Arg{},
		Flags:       []flagDoc{},
		Auth:        cmd.Annotations[annotAuth],
	}
	if m, p := cmd.Annotations[annotMethod], cmd.Annotations[annotPath]; m != "" && p != "" {
		d.Method, d.Path = &m, &p
	}
	if a := cmd.Annotations[annotArgs]; a != "" && a != "null" {
		_ = json.Unmarshal([]byte(a), &d.Args)
	}
	add := func(f *pflag.Flag) {
		if f.Name == "help" || slices.ContainsFunc(d.Flags, func(x flagDoc) bool { return x.Name == f.Name }) {
			return
		}
		_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
		d.Flags = append(d.Flags, flagDoc{Name: f.Name, Type: f.Value.Type(), Required: required, Default: f.DefValue, Description: f.Usage})
	}
	cmd.NonInheritedFlags().VisitAll(add)
	cmd.InheritedFlags().VisitAll(add)
	slices.SortFunc(d.Flags, func(a, b flagDoc) int { return strings.Compare(a.Name, b.Name) })
	return d
}

// describeGroup is the JSON help of every command under a group, at any
// depth, that is not itself a group, sorted by name.
func describeGroup(group *cobra.Command) groupDoc {
	doc := groupDoc{Commands: []commandDoc{}}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if isGroup(sub) {
				walk(sub)
			} else {
				doc.Commands = append(doc.Commands, describe(sub))
			}
		}
	}
	walk(group)
	slices.SortFunc(doc.Commands, func(a, b commandDoc) int { return strings.Compare(a.Name, b.Name) })
	return doc
}
