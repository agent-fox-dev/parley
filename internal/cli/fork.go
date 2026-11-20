package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// parentArg is the <parent> argument: the slug of the workspace whose forks
// a command creates or lists.
var parentArg = argSpec{Name: "parent", Required: true}

// forkStringFlags are the string flags of parley fork create sent as one
// body member each when given; the order is the order of their help.
var forkStringFlags = []struct {
	flag, member, usage string
}{
	{"name", "name", "the fork's name; its slug is <parent>--<name> (default: a generated name)"},
	{"ref", "ref", "the parent's branch the fork is taken from (default: the parent's default branch)"},
	{"ttl", "ttl", "the fork's lifetime, such as 2h or 7d (default: the hub's [forks].default_ttl)"},
	{"purpose", "purpose", "what the fork is for"},
	{"session", "session_id", "the id of the agent session to link the fork to"},
}

// forkSlugSeparator joins a fork's parent slug and its name; only a fork's
// slug holds it.
const forkSlugSeparator = "--"

// newForkCommand builds parley fork: the task forks of a workspace.
func newForkCommand() *cobra.Command {
	return group("fork", "Create, list, extend and delete the task forks of a workspace",
		newForkCreateCommand(),
		declare(command{
			Name:        "list",
			Description: "List the forks of a workspace you can read, newest first",
			Method:      http.MethodGet,
			Path:        "/workspaces/:parent/forks",
			Args:        []argSpec{parentArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.Bool("mine", false, "list only the forks you own")
				fs.String("session", "", "list only the forks linked to the agent session with this id")
				fs.Int("limit", 0, "the most forks the page holds; the hub's default, 50, when not given")
				fs.String("cursor", "", "the next_cursor of the page before, to read the page after it")
			},
			Run: listForks,
		}),
		newForkExtendCommand(),
		newForkDeleteCommand(),
	)
}

// newForkExtendCommand builds parley fork extend: a new lifetime for a fork,
// counted from the hub's time of the request. The form of --ttl is the
// hub's to judge, so its refusal names what is wrong; only an empty value,
// which can be no lifetime, is refused here.
func newForkExtendCommand() *cobra.Command {
	extend := forkOnly(declare(command{
		Name:        "extend",
		Description: "Give a task fork a new lifetime, longer or shorter, counted from now",
		Method:      http.MethodPatch,
		Path:        "/workspaces/:slug",
		Args:        []argSpec{slugArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("ttl", "", "the fork's new lifetime from now, such as 2h or 7d, within the hub's [forks].max_ttl")
		},
		Run: func(c *call) error {
			ttl, _ := c.cmd.Flags().GetString("ttl")
			return sendWith(c, nil, map[string]any{"ttl": ttl})
		},
	}))
	_ = extend.MarkFlagRequired("ttl")
	checkLine := extend.Args
	extend.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		if ttl, _ := cmd.Flags().GetString("ttl"); ttl == "" {
			return usageError("%s needs a value for --ttl", cmd.CommandPath())
		}
		return nil
	}
	return extend
}

// newForkDeleteCommand builds parley fork delete. A fork is disposable, so
// unlike parley workspace delete it takes no --confirm. The hub answers a
// delete with no body, so the command prints a document that names the fork
// deleted.
func newForkDeleteCommand() *cobra.Command {
	return forkOnly(declare(command{
		Name:        "delete",
		Description: "Delete a task fork at once, with its own branches and the tokens bound to it",
		Method:      http.MethodDelete,
		Path:        "/workspaces/:slug",
		Args:        []argSpec{slugArg},
		Auth:        authUser,
		Confirm: func(c *call) any {
			return struct {
				Status string `json:"status"`
				Slug   string `json:"slug"`
			}{"deleted", c.args[slugArg.Name]}
		},
	}))
}

// forkOnly makes cmd refuse, as a usage error before any setting is
// resolved or any request sent, a <slug> that is no fork's slug, so that a
// command meant for a fork can never reach a repository workspace.
func forkOnly(cmd *cobra.Command) *cobra.Command {
	checkLine := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		if !strings.Contains(args[0], forkSlugSeparator) {
			return usageError("%s takes the slug of a task fork, <parent>--<name>, got %q", cmd.CommandPath(), args[0])
		}
		return nil
	}
	return cmd
}

// newForkCreateCommand builds parley fork create. The words after -- are
// the command of the sandbox the fork is created with, so the command takes
// exactly one argument before them.
func newForkCreateCommand() *cobra.Command {
	create := declare(command{
		Name:        "create",
		Description: "Create a task fork of a workspace, with a token bound to it and a sandbox when asked",
		Method:      http.MethodPost,
		Path:        "/workspaces/:parent/forks",
		Args:        []argSpec{parentArg},
		Trailing:    &trailingWords{Name: "command"},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			for _, f := range forkStringFlags {
				fs.String(f.flag, "", f.usage)
			}
			fs.Bool("no-auto-change", false, "do not open a change when an own branch of the fork is pushed")
			fs.String("token-permissions", "", "mint a token bound to the fork with these scopes, separated by commas")
			fs.String("token-paths", "", "with --token-permissions, globs of the repository paths the token may write, separated by commas")
			fs.String("sandbox-profile", "", "create a sandbox from this profile with the fork; the words after -- are its command")
		},
		Run: createFork,
	})
	create.Use += " [-- command...]"
	checkLine := create.Args
	// The body is built here too, so that a flag that needs another fails
	// with the other usage errors, before any setting is resolved or any
	// request sent (ADR 02).
	create.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, beforeDash(cmd, args)); err != nil {
			return err
		}
		_, err := forkCreateBody(cmd)
		return err
	}
	return create
}

// beforeDash is args up to the --, if the command line has one.
func beforeDash(cmd *cobra.Command, args []string) []string {
	if n := cmd.ArgsLenAtDash(); n >= 0 {
		return args[:n]
	}
	return args
}

// afterDash is the words after the -- of the command line, nil without one.
func afterDash(cmd *cobra.Command) []string {
	args := cmd.Flags().Args()
	if n := cmd.ArgsLenAtDash(); n >= 0 && n < len(args) {
		return args[n:]
	}
	return nil
}

// forkCreateBody is the body of POST /workspaces/:slug/forks for cmd's
// flags: a member for each flag given, even with an empty value, which the
// hub judges, and none for a flag not given, so that the hub's default
// applies. --token-paths without --token-permissions, and a command after
// -- without --sandbox-profile, are usage errors: the hub would mint no
// token, or start no sandbox, from them.
func forkCreateBody(cmd *cobra.Command) (map[string]any, error) {
	fs := cmd.Flags()
	body := map[string]any{}
	for _, f := range forkStringFlags {
		if fs.Changed(f.flag) {
			body[f.member], _ = fs.GetString(f.flag)
		}
	}
	if noAuto, _ := fs.GetBool("no-auto-change"); noAuto {
		body["auto_change"] = false
	}
	switch {
	case fs.Changed("token-permissions"):
		permissions, _ := fs.GetString("token-permissions")
		token := map[string]any{"permissions": splitList(permissions)}
		if fs.Changed("token-paths") {
			paths, _ := fs.GetString("token-paths")
			token["paths"] = splitList(paths)
		}
		body["token"] = token
	case fs.Changed("token-paths"):
		return nil, usageError("%s needs --token-permissions with --token-paths", cmd.CommandPath())
	}
	command := afterDash(cmd)
	switch {
	case fs.Changed("sandbox-profile"):
		profile, _ := fs.GetString("sandbox-profile")
		sandbox := map[string]any{"profile": profile}
		if command != nil {
			sandbox["command"] = command
		}
		body["sandbox"] = sandbox
	case command != nil:
		return nil, usageError("%s needs --sandbox-profile with a command after --", cmd.CommandPath())
	}
	return body, nil
}

// createFork is the run step of parley fork create. It prints the hub's
// answer and, when the answer holds the secret of the token minted with the
// fork, warns on stderr, without the secret, that it is shown only once.
func createFork(c *call) error {
	body, err := forkCreateBody(c.cmd)
	if err != nil {
		return err
	}
	resp, err := c.client.request(c.ctx, c.decl.Method, routePath(c.decl.Path, c.args), nil, body)
	if err != nil {
		return err
	}
	if err := c.printAnswer(resp); err != nil {
		return err
	}
	var answer struct {
		Token struct {
			Token string `json:"token"`
		} `json:"token"`
	}
	if json.Unmarshal(resp.body, &answer) == nil && answer.Token.Token != "" {
		warn(c.stderr, "the token's value is shown once and will not be shown again")
	}
	return nil
}

// listForks is the run step of parley fork list: it sends a query parameter
// for each flag given and none for a flag not given, and prints the page.
// Values are sent as given, so that the hub's own error says what is wrong
// with one.
func listForks(c *call) error {
	fs := c.cmd.Flags()
	query := url.Values{}
	if mine, _ := fs.GetBool("mine"); mine {
		query.Set("owner", "me")
	}
	for flag, param := range map[string]string{"session": "session_id", "limit": "limit", "cursor": "cursor"} {
		if f := fs.Lookup(flag); f.Changed {
			query.Set(param, f.Value.String())
		}
	}
	return sendWith(c, query, nil)
}
