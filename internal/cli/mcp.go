package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// mcpDefaultGroups is the selection of parley mcp when --groups is not
// given: the commands an agent uses, and of workspace only the reading
// ones, so that an agent neither archives nor deletes a workspace.
const mcpDefaultGroups = "activity,change,claim,fork,land,sandbox,scm,workspace:read"

// maxMCPTools is the most tools one session serves. Each tool is declared
// to the model, and code mode declares each again as a function, so a
// larger selection is refused instead of served badly.
const maxMCPTools = 150

// readSuffix after a group's name in --groups selects only its commands
// whose route is a GET.
const readSuffix = ":read"

// mcpNeverServed are the groups parley mcp serves no command of, whatever
// --groups says: they administer the hub or handle credentials, or they are
// the CLI's own (help, version, mcp itself).
var mcpNeverServed = []string{"admin", "keys", "tokens", "secrets", "login", "credential-helper", "help", "version", "mcp"}

// mcpOmittedFlags are the flags no tool has as a parameter: the endpoint and
// the credential are the session's, a tool's result is always JSON, and a
// wait inside a tool call polls at the default interval.
var mcpOmittedFlags = []string{"api-key", "endpoint-url", "json", "poll-interval"}

// workspaceArgs are the names the commands give the argument that takes a
// workspace's slug. With --workspace each becomes optional.
var workspaceArgs = []string{"slug", "workspace-slug", "WORKSPACE", "parent"}

// mcpSettings are the flags of parley mcp.
type mcpSettings struct {
	list          bool
	groups        string
	readOnly      bool
	workspace     string
	sandbox       string
	noCodeMode    bool
	scriptTimeout string
}

// newMCPCommand builds parley mcp, which serves the CLI's commands as tools
// to a harness that speaks MCP.
func newMCPCommand() *cobra.Command {
	return declare(command{
		Name:        "mcp",
		Description: "Serve the commands as tools over MCP on standard input and output, for a harness that starts it; --list prints the tools instead",
		Auth:        authUser,
		// --list needs neither a hub nor a credential.
		OwnSettings: true,
		Flags: func(fs *pflag.FlagSet) {
			fs.Bool("list", false, "print the tools that would be served as one JSON array and exit")
			fs.String("groups", mcpDefaultGroups, "the command groups to serve, separated by commas; name:read serves only a group's reading commands")
			fs.Bool("read-only", false, "serve only commands that read, and of a sandbox only the tools that change nothing")
			fs.String("workspace", "", "the workspace every tool works on when a call names none")
			fs.String("sandbox", "", "serve the tools of this sandbox of --workspace too")
			fs.Bool("no-code-mode", false, "do not serve code_mode")
			fs.String("script-timeout", "", "the longest a code_mode script may run, from 1s to 30m (default 10m)")
		},
		Run: runMCPCommand,
	})
}

// runMCPCommand is the run step of parley mcp.
func runMCPCommand(c *call) error {
	fs := c.cmd.Flags()
	var s mcpSettings
	s.list, _ = fs.GetBool("list")
	s.groups, _ = fs.GetString("groups")
	s.readOnly, _ = fs.GetBool("read-only")
	s.workspace, _ = fs.GetString("workspace")
	s.sandbox, _ = fs.GetString("sandbox")
	s.noCodeMode, _ = fs.GetBool("no-code-mode")
	s.scriptTimeout, _ = fs.GetString("script-timeout")

	// The selection is judged first, so that a usage error does not wait
	// for a hub URL or a credential.
	session, err := newToolSession(c.opts, settings{}, s)
	if err != nil {
		return err
	}
	// Only a sandbox's tools are read from the hub, so --list without one
	// needs neither a hub URL nor a credential.
	if s.list && s.sandbox == "" {
		return printTools(c, session)
	}
	if session.st, err = resolveSettings(c.cmd); err != nil {
		return err
	}
	if err := session.loadSandboxTools(c.ctx); err != nil {
		return err
	}
	if s.list {
		return printTools(c, session)
	}
	return session.serve(c.ctx, c.stderr, s)
}

// toolListing is a tool as --list prints it.
type toolListing struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// printTools prints the tools session serves as one JSON array.
func printTools(c *call, session *toolSession) error {
	list, err := session.listing()
	if err != nil {
		return err
	}
	out, err := json.Marshal(list)
	if err != nil {
		return clientError("cannot encode the tools: %v", err)
	}
	return c.printResult(out)
}

// commandTool is the tool of one command: what a harness is told of it, and
// how a call's parameters become the command's line.
type commandTool struct {
	// name is the command's words joined by _, and words the words.
	name        string
	description string
	words       []string
	params      []toolParam
}

// toolParam is one parameter of a command tool: an argument or a flag of
// the command.
type toolParam struct {
	// name is the parameter's name, and source the argument's or the flag's
	// own name.
	name, source string
	isArg        bool
	// kind is the JSON schema type: string, boolean, integer or array (of
	// strings).
	kind        string
	required    bool
	description string
	// fallback is the value of an argument a call leaves out; "" for none.
	fallback string
}

// notNameCharacter is what a parameter's name replaces by _.
var notNameCharacter = regexp.MustCompile(`[^a-z0-9]`)

// parameterName is the name of the parameter of the argument or flag name:
// in lower case, with every character that is not a letter or a digit
// replaced by _.
func parameterName(name string) string {
	return notNameCharacter.ReplaceAllString(strings.ToLower(name), "_")
}

// schema is the tool's input schema: an object with one property per
// parameter and no other allowed.
func (t commandTool) schema() json.RawMessage {
	properties := map[string]any{}
	required := []string{}
	for _, p := range t.params {
		property := map[string]any{"type": p.kind, "description": p.description}
		if p.kind == "array" {
			property["items"] = map[string]any{"type": "string"}
		}
		properties[p.name] = property
		if p.required {
			required = append(required, p.name)
		}
	}
	slices.Sort(required)
	// Maps of strings, slices and booleans only: this cannot fail.
	out, _ := json.Marshal(map[string]any{
		"type": "object", "properties": properties, "required": required, "additionalProperties": false,
	})
	return out
}

// groupSelection is one element of --groups.
type groupSelection struct {
	name     string
	readOnly bool
}

// leafCommands are the commands under root that are not groups, at any
// depth.
func leafCommands(root *cobra.Command) []*cobra.Command {
	var all []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, cmd := range parent.Commands() {
			if isGroup(cmd) {
				walk(cmd)
			} else {
				all = append(all, cmd)
			}
		}
	}
	walk(root)
	return all
}

// wordsOf are the words of cmd after the root's name.
func wordsOf(cmd *cobra.Command) []string {
	return strings.Fields(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "))
}

// readSelection reads --groups against the groups the commands leaves have.
// A group that is never served, one no command has, an empty element and a
// suffix other than :read are usage errors.
func readSelection(groups string, leaves []*cobra.Command) ([]groupSelection, error) {
	var nameable []string
	for _, cmd := range leaves {
		if g := wordsOf(cmd)[0]; !slices.Contains(mcpNeverServed, g) && !slices.Contains(nameable, g) {
			nameable = append(nameable, g)
		}
	}
	slices.Sort(nameable)
	unknown := func(element string) error {
		return usageError("--groups: %q is not a command group; the groups are %s, each also as name%s", element, strings.Join(nameable, ", "), readSuffix)
	}
	var selection []groupSelection
	for _, element := range strings.Split(groups, ",") {
		element = strings.TrimSpace(element)
		name, suffix, hasSuffix := strings.Cut(element, ":")
		switch {
		case slices.Contains(mcpNeverServed, name):
			return nil, usageError("--groups: parley mcp never serves the group %q", name)
		case !slices.Contains(nameable, name), hasSuffix && ":"+suffix != readSuffix:
			return nil, unknown(element)
		}
		selection = append(selection, groupSelection{name: name, readOnly: hasSuffix})
	}
	return selection, nil
}

// commandTools are the tools of the commands under root that s selects,
// ordered by name. Whatever is selected, a command is a tool only when a
// user's credential runs it, it shows no secret once, and it neither needs
// a terminal nor runs until interrupted: the rule holds for commands added
// later, without a list to keep.
func commandTools(root *cobra.Command, s mcpSettings) ([]commandTool, error) {
	leaves := leafCommands(root)
	selection, err := readSelection(s.groups, leaves)
	if err != nil {
		return nil, err
	}
	var tools []commandTool
	for _, cmd := range leaves {
		decl := declarationOf(cmd)
		words := wordsOf(cmd)
		i := slices.IndexFunc(selection, func(g groupSelection) bool { return g.name == words[0] })
		switch {
		case i < 0, decl.Auth != authUser, decl.ShowsOnce, decl.Interactive:
			continue
		case (s.readOnly || selection[i].readOnly) && decl.Method != http.MethodGet:
			continue
		}
		tools = append(tools, newCommandTool(cmd, decl, words, s.workspace))
	}
	slices.SortFunc(tools, func(a, b commandTool) int { return strings.Compare(a.name, b.name) })
	return tools, nil
}

// newCommandTool is the tool of the command cmd, declared by decl. With a
// workspace, the command's workspace argument is optional and takes it when
// a call leaves it out; a call may still name another, such as a fork.
func newCommandTool(cmd *cobra.Command, decl command, words []string, workspace string) commandTool {
	t := commandTool{
		name:        strings.ReplaceAll(strings.Join(words, "_"), "-", "_"),
		description: decl.Description,
		words:       words,
	}
	for _, a := range decl.Args {
		p := toolParam{
			name: parameterName(a.Name), source: a.Name, isArg: true, kind: "string", required: a.Required,
			description: fmt.Sprintf("the argument %s of parley %s", a.Name, strings.Join(words, " ")),
		}
		if workspace != "" && slices.Contains(workspaceArgs, a.Name) {
			p.required, p.fallback = false, workspace
			p.description += fmt.Sprintf("; %s when left out", workspace)
		}
		t.params = append(t.params, p)
	}
	add := func(f *pflag.Flag) {
		_, cobras := f.Annotations[cobra.FlagSetByCobraAnnotation]
		_, untilInterrupted := f.Annotations[untilInterruptedAnnotation]
		if cobras || untilInterrupted || slices.Contains(mcpOmittedFlags, f.Name) {
			return
		}
		t.params = append(t.params, toolParam{
			name: parameterName(f.Name), source: f.Name, kind: flagKind(f), required: isRequired(f), description: f.Usage,
		})
	}
	cmd.LocalFlags().VisitAll(add)
	cmd.InheritedFlags().VisitAll(add)
	return t
}

// flagKind is the JSON schema type of the flag f's value.
func flagKind(f *pflag.Flag) string {
	switch f.Value.Type() {
	case "bool":
		return "boolean"
	case "int", "int64", "integer":
		return "integer"
	case "stringArray":
		return "array"
	}
	// string and duration, which is written as the shell takes it (90s),
	// and any type a later flag brings.
	return "string"
}
