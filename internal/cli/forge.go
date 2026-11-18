package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/agent-fox-dev/parley/internal/forge/number"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The flags the forge commands share.
var (
	targetFlag = queryFlag{name: "target", param: "target", usage: "the remote the command works on, origin or upstream (default: origin)"}
	sinceFlag  = queryFlag{name: "since", param: "since", usage: "only what was received at or after this RFC 3339 time"}
	// pageFlags are the flags of a forge read that answers a list.
	pageFlags = []queryFlag{limitFlag, cursorFlag}
)

// newForgeCommand builds parley forge: what the hub derives about a
// workspace's forge, the forge's repository, and the webhook deliveries the
// forge sends the hub.
func newForgeCommand() *cobra.Command {
	return group("forge", "Show how the hub reaches a workspace's forge, the forge's repository and the webhook deliveries it sends",
		forgeRead(scmRead{command: command{Name: "status", Description: "Show the forge, API URL and credential kind the hub derives for a workspace, without asking the forge", Path: "/workspaces/:slug/forge"}}),
		forgeRead(scmRead{
			command: command{Name: "repository", Description: "Show the forge's repository of a workspace: its name, default branch, visibility and the caller's permissions", Path: "/workspaces/:slug/forge/repository"},
			query:   []queryFlag{targetFlag},
		}),
		newWebhookCommand(),
		forgeRead(scmRead{
			command: command{Name: "deliveries", Description: "List the webhook deliveries the hub recorded for a workspace, newest first", Path: "/workspaces/:slug/forge/deliveries"},
			query:   []queryFlag{sinceFlag, limitFlag, cursorFlag},
		}),
	)
}

// The flags of parley forge webhook set that say where the secret comes
// from; at most one of them is given.
var webhookSources = []string{"secret", "from-stdin", "generate"}

// newWebhookCommand builds parley forge webhook: the secret the forge signs
// a workspace's webhook deliveries with, which a person enters in the
// forge's settings.
func newWebhookCommand() *cobra.Command {
	const path = "/workspaces/:slug/forge/webhook"
	set := declare(command{
		Name:        "set",
		Description: "Set or rotate the secret of a workspace's forge webhook: one the hub generates and shows once, or one given",
		Method:      http.MethodPut,
		Path:        path,
		Args:        []argSpec{slugArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("secret", "", "the secret to store, which the hub checks")
			fs.Bool("from-stdin", false, "read the secret from standard input, without one trailing line ending")
			fs.Bool("generate", false, "have the hub generate the secret (the default)")
		},
		Run: setWebhook,
	})
	return group("webhook", "Set or remove the secret of a workspace's forge webhook",
		checkFlags(set, func(cmd *cobra.Command) error {
			var given []string
			for _, name := range webhookSources {
				if cmd.Flags().Changed(name) {
					given = append(given, "--"+name)
				}
			}
			if len(given) > 1 {
				return usageError("%s takes one of --secret, --from-stdin and --generate, got %s", cmd.CommandPath(), strings.Join(given, " and "))
			}
			return nil
		}),
		declare(command{
			Name:        "unset",
			Description: "Remove the secret of a workspace's forge webhook, so that the hub refuses its deliveries",
			Method:      http.MethodDelete,
			Path:        path,
			Args:        []argSpec{slugArg},
			Auth:        authUser,
			Confirm: func(c *call) any {
				return map[string]string{"status": "unset", "workspace": c.args[slugArg.Name]}
			},
		}),
	)
}

// setWebhook sends the secret given, or no body for the hub to generate
// one, and prints the hub's answer. It writes the show-once note itself,
// not through ShowsOnce, because only an answer that holds a generated
// secret shows one.
func setWebhook(c *call) error {
	fs := c.cmd.Flags()
	var body any
	switch {
	case fs.Changed("secret"):
		value, _ := fs.GetString("secret")
		body = map[string]string{"secret": value}
	case fromStdin(c.cmd):
		data, err := io.ReadAll(c.opts.stdin)
		if err != nil {
			return clientError("cannot read the secret from stdin: %v", err)
		}
		body = map[string]string{"secret": withoutLineEnding(data)}
	}
	resp, err := c.client.request(c.ctx, c.decl.Method, routePath(c.decl.Path, c.args), nil, body)
	if err != nil {
		return err
	}
	if err := c.printAnswer(resp); err != nil {
		return err
	}
	var answer map[string]json.RawMessage
	if json.Unmarshal(resp.body, &answer) == nil {
		if _, shown := answer["secret"]; shown {
			warn(c.stderr, showOnceWarning)
		}
	}
	return nil
}

// forgeRead builds r, a read of a workspace's forge: GET of its path with
// the arguments of the path (forgeArgs) and a query parameter for each flag
// of r.query given.
func forgeRead(r scmRead) *cobra.Command {
	r.Args = forgeArgs(r.Path)
	return withForgeNumber(r.build(), r.Path)
}

// forgeArgs are the arguments of a forge route's path: the workspace as
// <slug> and, where the path names one, the issue or pull request number as
// <number>.
func forgeArgs(path string) []argSpec {
	args := []argSpec{slugArg}
	if hasForgeNumber(path) {
		args = append(args, numberArg)
	}
	return args
}

func hasForgeNumber(path string) bool {
	return strings.Contains(path, ":"+numberArg.Name)
}

// withForgeNumber makes cmd, the command of a forge route's path, check its
// <number>, the second argument, with the rule the hub applies, as the
// command line is, so that a bad one is a usage error before any request
// (ADR 02). A path without a number leaves cmd as it is.
func withForgeNumber(cmd *cobra.Command, path string) *cobra.Command {
	if !hasForgeNumber(path) {
		return cmd
	}
	checkLine := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		if _, err := number.Parse(args[1]); err != nil {
			return usageError("%s takes <number> %q: %v", cmd.CommandPath(), args[1], err)
		}
		return nil
	}
	return cmd
}

// forgeWrite declares a write to a workspace's forge: its method to its
// path, with the arguments of the path (forgeArgs) followed by its own
// Args, --target, the text input when it takes one, and the body it builds.
// Every refusal of the command line is a usage error raised before any
// setting is resolved or any request sent (ADR 02). It sends no
// Idempotency-Key: the hub's replay of a create is for callers that retry
// one request, and the CLI sends each command once.
type forgeWrite struct {
	command
	// text, when set, gives the command --body and --body-file.
	text *textInput
	// check refuses a command line, as a usage error, once its arguments
	// and its text have been read.
	check func(cmd *cobra.Command) error
	// body builds the request's body from the command line; nil, or a nil
	// result, sends none.
	body func(c *call) any
}

// build declares w.
func (w forgeWrite) build() *cobra.Command {
	decl := w.command
	decl.Auth = authUser
	decl.Args = append(forgeArgs(decl.Path), w.Args...)
	own := decl.Flags
	decl.Flags = func(fs *pflag.FlagSet) {
		targetFlag.add(fs)
		if w.text != nil {
			w.text.addFlags(fs)
		}
		if own != nil {
			own(fs)
		}
	}
	decl.Run = func(c *call) error {
		var body any
		if w.body != nil {
			body = w.body(c)
		}
		return sendWith(c, forgeQuery(c.cmd.Flags()), body)
	}
	cmd := withForgeNumber(declare(decl), decl.Path)
	return checkFlags(cmd, func(cmd *cobra.Command) error {
		if w.text != nil {
			if err := w.text.read(cmd); err != nil {
				return err
			}
		}
		if w.check != nil {
			return w.check(cmd)
		}
		return nil
	})
}

// forgeQuery is the query of a forge write: target when --target is given,
// so that the hub's default applies otherwise.
func forgeQuery(fs *pflag.FlagSet) url.Values {
	query := url.Values{}
	if fs.Changed(targetFlag.name) {
		value, _ := fs.GetString(targetFlag.name)
		query.Set(targetFlag.param, value)
	}
	return query
}

// givenFields adds to body, under its member, the value of each flag of
// fields given: the string of a string flag, the list of a repeatable one.
// A flag not given adds nothing, and one given empty adds its empty value.
func givenFields(fs *pflag.FlagSet, body map[string]any, fields map[string]string) {
	for flag, member := range fields {
		f := fs.Lookup(flag)
		switch {
		case f == nil || !f.Changed:
		case f.Value.Type() == "stringArray":
			body[member], _ = fs.GetStringArray(flag)
		default:
			body[member] = f.Value.String()
		}
	}
}
