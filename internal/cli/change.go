package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// numberArg is the <number> argument: the number of a change in its
// workspace.
var numberArg = argSpec{Name: "number", Required: true}

// openStringFlags are the string flags of parley change open sent as one
// body member each when given; the order is the order of their help.
var openStringFlags = []struct {
	flag, member, usage string
}{
	{"target", "target_branch", "the branch the change proposes to land on (default: the workspace's default branch, a fork's base branch)"},
	{"title", "title", "the change's title (default: the subject of the source's head commit)"},
	{"intent", "intent", "why the change exists; @<file> reads it from the file, - from stdin (default: the head commit's body, then a fork's purpose)"},
	{"session", "session_id", "the id of the agent session the change belongs to"},
}

// changePath is the route of one change, which edit, ready and draft
// patch.
const changePath = "/workspaces/:slug/changes/:number"

// editFlags are the flags of parley change edit, each of which edits one
// member of the change; edit needs at least one of them.
var editFlags = []string{"title", "intent", "label", "remove-label", "auto-land", "no-auto-land", "target"}

// newChangeCommand builds parley change: the changes of a workspace, their
// versions and their diffs, and their edits, abandon and restore.
func newChangeCommand() *cobra.Command {
	return group("change", "Open, list, read, edit, abandon and restore the changes of a workspace, and read their versions and diffs",
		newChangeOpenCommand(),
		newChangeEditCommand(),
		newChangeStateCommand("ready", "Mark a draft change ready for review and landing", "open"),
		newChangeStateCommand("draft", "Move an open change back to draft", "draft"),
		newChangeAbandonCommand(),
		withNumbers(declare(command{
			Name:        "restore",
			Description: "Bring an abandoned change back to open at its last version, recreating its deleted source branch",
			Method:      http.MethodPost,
			Path:        changePath + "/restore",
			Args:        []argSpec{slugArg, numberArg},
			Auth:        authUser,
		}), true),
		withNumbers(scmRead{
			command: command{Name: "list", Description: "List the changes of a workspace, the draft and open ones unless --state says otherwise", Path: "/workspaces/:slug/changes", Args: []argSpec{slugArg}},
			query: []queryFlag{
				{name: "state", param: "state", kind: listFlag, usage: "only the changes in this state: draft, open, landed or abandoned; repeatable"},
				{name: "target", param: "target_branch", usage: "only the changes into this branch"},
				{name: "author", param: "author", usage: "only the changes of this user id, or me for your own"},
				{name: "session", param: "session_id", usage: "only the changes the agent session with this id contributed to"},
				{name: "label", param: "label", usage: "only the changes with this label"},
				{name: "path", param: "path", usage: "only the changes that touch a path matching this glob"},
				{name: "mergeable", param: "mergeable", usage: "only the changes whose mergeable is this: clean, conflict or unknown"},
				{name: "landing-status", param: "landing_status", usage: "only the changes whose landing status is this: ready, pending or blocked"},
				{name: "stack-of", param: "stack_of", usage: "the stack of the change with this number, bottom first, in every state"},
				limitFlag, cursorFlag,
			},
		}.build(), false, "stack-of"),
		withNumbers(scmRead{
			command: command{Name: "show", Description: "Show one change", Path: "/workspaces/:slug/changes/:number", Args: []argSpec{slugArg, numberArg}},
			query: []queryFlag{
				{name: "include", param: "include", usage: "add these members to the change, separated by commas, such as versions"},
			},
		}.build(), true),
		scmRead{
			command: command{Name: "find", Description: "Show the change a commit belongs to, with the version it is the head of", Path: "/workspaces/:slug/changes/by-commit/:sha", Args: []argSpec{slugArg, {Name: "sha", Required: true}}},
		}.build(),
		withNumbers(scmRead{
			command: command{Name: "versions", Description: "List the versions of a change, oldest first", Path: "/workspaces/:slug/changes/:number/versions", Args: []argSpec{slugArg, numberArg}},
		}.build(), true),
		withNumbers(scmRead{
			command: command{Name: "diff", Description: "Show the diff of a version of a change, or the interdiff of two versions", Path: "/workspaces/:slug/changes/:number/diff", Args: []argSpec{slugArg, numberArg}},
			query: []queryFlag{
				{name: "version", param: "version", usage: "the version whose diff is shown, a number or latest (default: latest)"},
				{name: "against", param: "against", usage: "show the interdiff from this version to --version instead of the diff against the base"},
				globsFlag("kept"),
				{name: "patch", param: "format", kind: switchFlag, sends: "patch", usage: "print the unified diff text instead of JSON"},
				{name: "name-only", param: "patches", kind: switchFlag, sends: "false", usage: "leave the patch text out of the files"},
			},
			check: onePatchForm,
			send:  diffRevisions,
		}.build(), true, "version", "against"),
	)
}

// newChangeOpenCommand builds parley change open: a change of a source
// branch, of the workspace or of one of its forks, into a target branch.
func newChangeOpenCommand() *cobra.Command {
	open := declare(command{
		Name:        "open",
		Description: "Open a change that proposes to land the commits of a source branch on a target branch",
		Method:      http.MethodPost,
		Path:        "/workspaces/:slug/changes",
		Args:        []argSpec{slugArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("source", "", "the branch whose commits the change proposes")
			_ = cobra.MarkFlagRequired(fs, "source")
			fs.String("source-workspace", "", "the workspace the source branch is in, such as a fork of <slug> (default: <slug>)")
			for _, f := range openStringFlags {
				fs.String(f.flag, "", f.usage)
			}
			fs.Bool("draft", false, "open the change as a draft")
			fs.String("depends-on", "", "the number of the change this one is stacked on")
			fs.Bool("auto-land", false, "land the change on its own once it is ready")
			fs.StringArray("label", nil, "a label of the change, repeatable")
		},
		Run: openChange,
	})
	return withNumbers(open, false, "depends-on")
}

// openChange is the run step of parley change open. Its body holds the
// source branch and a member for each flag given, and none for a flag not
// given, so that the hub's default applies. The intent is read before
// anything is sent, so that a file that cannot be read sends no request.
func openChange(c *call) error {
	fs := c.cmd.Flags()
	source := map[string]any{}
	source["branch"], _ = fs.GetString("source")
	if fs.Changed("source-workspace") {
		source["workspace_slug"], _ = fs.GetString("source-workspace")
	}
	body := map[string]any{"source": source}
	for _, f := range openStringFlags {
		if fs.Changed(f.flag) {
			body[f.member], _ = fs.GetString(f.flag)
		}
	}
	if fs.Changed("intent") {
		intent, err := intentText(body["intent"].(string), c.opts.stdin)
		if err != nil {
			return err
		}
		body["intent"] = intent
	}
	for flag, member := range map[string]string{"draft": "draft", "auto-land": "auto_land"} {
		if fs.Changed(flag) {
			body[member], _ = fs.GetBool(flag)
		}
	}
	if fs.Changed("depends-on") {
		value, _ := fs.GetString("depends-on")
		// withNumbers accepted it, so it parses.
		body["depends_on"], _ = strconv.ParseInt(value, 10, 64)
	}
	if fs.Changed("label") {
		body["labels"], _ = fs.GetStringArray("label")
	}
	return sendWith(c, nil, body)
}

// newChangeEditCommand builds parley change edit: a PATCH of a change
// whose body holds a member for each flag given and none for a flag not
// given, so that an empty --title is sent and a member not named is left
// as it is.
func newChangeEditCommand() *cobra.Command {
	edit := declare(command{
		Name:        "edit",
		Description: "Edit the title, intent, labels, auto-land flag or target branch of a change",
		Method:      http.MethodPatch,
		Path:        changePath,
		Args:        []argSpec{slugArg, numberArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("title", "", "the change's new title")
			fs.String("intent", "", "the change's new intent; @<file> reads it from the file, - from stdin")
			fs.StringArray("label", nil, "add this label unless the change has it, repeatable")
			fs.StringArray("remove-label", nil, "remove this label, repeatable")
			fs.Bool("auto-land", false, "land the change on its own once it is ready")
			fs.Bool("no-auto-land", false, "stop landing the change on its own")
			fs.String("target", "", "the branch the change proposes to land on from now on")
		},
		Run: editChange,
	})
	return checkFlags(withNumbers(edit, true), func(cmd *cobra.Command) error {
		fs := cmd.Flags()
		if !slices.ContainsFunc(editFlags, fs.Changed) {
			return usageError("%s needs at least one of --%s", cmd.CommandPath(), strings.Join(editFlags, ", --"))
		}
		if fs.Changed("auto-land") && fs.Changed("no-auto-land") {
			return usageError("%s takes --auto-land or --no-auto-land, not both", cmd.CommandPath())
		}
		return nil
	})
}

// editChange is the run step of parley change edit. The intent is read
// before anything is sent, so that a file that cannot be read sends no
// request. With a label flag the change is read first, because the PATCH
// replaces the whole list: a read that fails sends no PATCH.
func editChange(c *call) error {
	fs := c.cmd.Flags()
	body := map[string]any{}
	for flag, member := range map[string]string{"title": "title", "intent": "intent", "target": "target_branch"} {
		if fs.Changed(flag) {
			body[member], _ = fs.GetString(flag)
		}
	}
	if fs.Changed("intent") {
		intent, err := intentText(body["intent"].(string), c.opts.stdin)
		if err != nil {
			return err
		}
		body["intent"] = intent
	}
	if fs.Changed("auto-land") {
		body["auto_land"], _ = fs.GetBool("auto-land")
	}
	if fs.Changed("no-auto-land") {
		off, _ := fs.GetBool("no-auto-land")
		body["auto_land"] = !off
	}
	if fs.Changed("label") || fs.Changed("remove-label") {
		resp, err := c.client.request(c.ctx, http.MethodGet, routePath(c.decl.Path, c.args), nil, nil)
		if err != nil {
			return err
		}
		var change struct {
			Labels []string `json:"labels"`
		}
		if err := json.Unmarshal(resp.body, &change); err != nil {
			return clientError("cannot read the labels of the change: %v", err)
		}
		add, _ := fs.GetStringArray("label")
		remove, _ := fs.GetStringArray("remove-label")
		body["labels"] = editLabels(change.Labels, add, remove)
	}
	return sendWith(c, nil, body)
}

// editLabels is labels without the values of remove, then with each value
// of add it does not hold yet appended in order. It is never nil, so that
// removing every label sends an empty list.
func editLabels(labels, add, remove []string) []string {
	edited := []string{}
	for _, label := range labels {
		if !slices.Contains(remove, label) {
			edited = append(edited, label)
		}
	}
	for _, label := range add {
		if !slices.Contains(edited, label) {
			edited = append(edited, label)
		}
	}
	return edited
}

// newChangeStateCommand builds parley change ready or draft: a PATCH of a
// change that moves it to state and to nothing else.
func newChangeStateCommand(name, description, state string) *cobra.Command {
	return withNumbers(declare(command{
		Name:        name,
		Description: description,
		Method:      http.MethodPatch,
		Path:        changePath,
		Args:        []argSpec{slugArg, numberArg},
		Auth:        authUser,
		Run: func(c *call) error {
			return sendWith(c, nil, map[string]string{"state": state})
		},
	}), true)
}

// abandonDependants are the values --dependants of parley change abandon
// takes, as the hub reads them.
var abandonDependants = []string{"reparent", "abandon"}

// newChangeAbandonCommand builds parley change abandon. Its body holds a
// member for each flag given, so that the hub's default applies to the
// others; without a flag it is {}.
func newChangeAbandonCommand() *cobra.Command {
	abandon := declare(command{
		Name:        "abandon",
		Description: "Abandon a change, saying what becomes of the changes stacked on it",
		Method:      http.MethodPost,
		Path:        changePath + "/abandon",
		Args:        []argSpec{slugArg, numberArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("reason", "", "why the change is abandoned")
			fs.String("dependants", "", "what becomes of the changes stacked on it: reparent stacks them on what it sat on, abandon abandons them too")
		},
		Run: func(c *call) error {
			fs := c.cmd.Flags()
			body := map[string]string{}
			for _, flag := range []string{"reason", "dependants"} {
				if fs.Changed(flag) {
					body[flag], _ = fs.GetString(flag)
				}
			}
			return sendWith(c, nil, body)
		},
	})
	return checkFlags(withNumbers(abandon, true), func(cmd *cobra.Command) error {
		if fs := cmd.Flags(); fs.Changed("dependants") {
			if value, _ := fs.GetString("dependants"); !slices.Contains(abandonDependants, value) {
				return usageError("%s takes --dependants reparent or abandon, got %q", cmd.CommandPath(), value)
			}
		}
		return nil
	})
}

// checkFlags makes cmd refuse, as a usage error before any setting is
// resolved or any request sent (ADR 02), a combination of flags check
// refuses, once the command line has been accepted otherwise.
func checkFlags(cmd *cobra.Command, check func(cmd *cobra.Command) error) *cobra.Command {
	checkLine := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		return check(cmd)
	}
	return cmd
}

// intentText is the intent --intent value gives: the content of the file
// after @, all of stdin for -, or the value itself. A file or stdin that
// cannot be read is a client_error, the fault of no argument's form.
func intentText(value string, stdin io.Reader) (string, error) {
	switch {
	case value == "-":
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", clientError("cannot read the intent from stdin: %v", err)
		}
		return string(data), nil
	case strings.HasPrefix(value, "@"):
		data, err := os.ReadFile(value[1:])
		if err != nil {
			return "", clientError("cannot read the intent: %v", err)
		}
		return string(data), nil
	}
	return value, nil
}

// withNumbers makes cmd refuse, as a usage error before any setting is
// resolved or any request sent (ADR 02), a <number>, its second argument
// when numberArg, and a value of each flag of flags given, that is not a
// positive integer. --version takes latest too, which the hub reads as the
// change's last version.
func withNumbers(cmd *cobra.Command, numberArg bool, flags ...string) *cobra.Command {
	checkLine := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		if numberArg && !isPositiveNumber(args[1]) {
			return usageError("%s takes a <number> that is a positive integer, got %q", cmd.CommandPath(), args[1])
		}
		for _, flag := range flags {
			f := cmd.Flags().Lookup(flag)
			if !f.Changed {
				continue
			}
			value := f.Value.String()
			if flag == "version" && value == "latest" {
				continue
			}
			if !isPositiveNumber(value) {
				want := "a positive integer"
				if flag == "version" {
					want += " or latest"
				}
				return usageError("%s takes --%s <n>, %s, got %q", cmd.CommandPath(), flag, want, value)
			}
		}
		return nil
	}
	return cmd
}

// isPositiveNumber tells whether s is a positive integer written in decimal
// digits alone, as the hub reads a change or version number: no sign, no
// space and no fraction.
func isPositiveNumber(s string) bool {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0
}
