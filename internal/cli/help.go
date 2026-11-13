package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// installHelp gives root the help of parley: as text, cobra's own, and with
// --json one JSON document built from the declarations. It replaces cobra's
// help command with a declared one, which refuses words that name no
// command. Help reads no setting and sends no request.
func installHelp(root *cobra.Command) {
	text := root.HelpFunc()
	show := func(w io.Writer, cmd *cobra.Command, asJSON bool) {
		if !asJSON {
			text(cmd, nil)
			return
		}
		var doc any = describe(cmd)
		if isGroup(cmd) {
			doc = commandList{Commands: describeAll(cmd)}
		}
		// The documents hold strings, booleans and slices of them only, so
		// this cannot fail.
		out, _ := json.MarshalIndent(doc, "", "  ")
		fmt.Fprintf(w, "%s\n", out)
	}
	// --help on any command, and a group run without a word, end here.
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		show(cmd.OutOrStdout(), cmd, asJSON(cmd))
	})
	root.SetHelpCommand(declare(command{
		Name:        "help",
		Description: "Print the help of parley or of a command",
		Args:        []argSpec{{Name: "command", Repeats: true}},
		Auth:        authNone,
		Run: func(c *call) error {
			words := c.cmd.Flags().Args()
			target, rest, err := c.cmd.Root().Find(words)
			if err != nil {
				return usageError("%v", err)
			}
			if len(rest) > 0 {
				return usageError("unknown command %q for %q", rest[0], target.CommandPath())
			}
			// What --help on the target would add before printing, so that
			// both print the same.
			target.InitDefaultHelpFlag()
			show(c.stdout, target, asJSON(c.cmd))
			return nil
		},
	}))
}

// commandHelp is the JSON help of one command, built from its declaration.
type commandHelp struct {
	// Name is the command's words after parley, joined by single spaces.
	Name        string `json:"name"`
	Description string `json:"description"`
	// Method and Path are null for a command that calls no single route.
	Method *string    `json:"method"`
	Path   *string    `json:"path"`
	Args   []argSpec  `json:"args"`
	Flags  []flagHelp `json:"flags"`
	Auth   authLevel  `json:"auth"`
}

// flagHelp is one flag of a command in its JSON help.
type flagHelp struct {
	// Name is the flag's name without its dashes.
	Name string `json:"name"`
	// Type is the type name the flag library reports for the value.
	Type     string `json:"type"`
	Required bool   `json:"required"`
	// Default is the default value as the flag library writes it.
	Default     string `json:"default"`
	Description string `json:"description"`
}

// commandList is the JSON help of a command group or of parley itself.
type commandList struct {
	Commands []commandHelp `json:"commands"`
}

// describe is the JSON help of cmd. A command declared without declare has
// no description, route or auth, which the help shows as such.
func describe(cmd *cobra.Command) commandHelp {
	decl := declarationOf(cmd)
	args := decl.Args
	if args == nil {
		args = []argSpec{}
	}
	return commandHelp{
		Name:        strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "),
		Description: decl.Description,
		Method:      nullIfEmpty(decl.Method),
		Path:        nullIfEmpty(decl.Path),
		Args:        args,
		Flags:       flagsOf(cmd),
		Auth:        decl.Auth,
	}
}

// describeAll is the JSON help of every command under g that is not a
// group, at any depth, sorted by name.
func describeAll(g *cobra.Command) []commandHelp {
	all := []commandHelp{}
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, cmd := range parent.Commands() {
			if isGroup(cmd) {
				walk(cmd)
			} else {
				all = append(all, describe(cmd))
			}
		}
	}
	walk(g)
	slices.SortFunc(all, func(a, b commandHelp) int { return strings.Compare(a.Name, b.Name) })
	return all
}

// flagsOf is every flag cmd takes, its own and the global ones, sorted by
// name, without the flags cobra adds itself (--help).
func flagsOf(cmd *cobra.Command) []flagHelp {
	byName := map[string]flagHelp{}
	add := func(f *pflag.Flag) {
		if _, cobras := f.Annotations[cobra.FlagSetByCobraAnnotation]; cobras {
			return
		}
		byName[f.Name] = flagHelp{
			Name:        f.Name,
			Type:        f.Value.Type(),
			Required:    isRequired(f),
			Default:     f.DefValue,
			Description: f.Usage,
		}
	}
	cmd.LocalFlags().VisitAll(add)
	cmd.InheritedFlags().VisitAll(add)
	flags := make([]flagHelp, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		flags = append(flags, byName[name])
	}
	return flags
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// asJSON tells whether the global flag --json was given to cmd, which has
// parsed its command line.
func asJSON(cmd *cobra.Command) bool {
	value, _ := cmd.Flags().GetBool("json")
	return value
}
