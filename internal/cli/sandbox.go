package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The positional arguments of the sandbox commands.
var (
	workspaceSlugArg = argSpec{Name: "workspace-slug", Required: true}
	sandboxIDArg     = argSpec{Name: "id", Required: true}
)

// sandboxesRoute is the collection of a workspace's sandboxes.
const sandboxesRoute = "/workspaces/:workspace-slug/sandboxes"

// waitExitFlag waits for the sandbox's command to end, not for the sandbox
// to be up.
const waitExitFlag = "wait-exit"

// sandboxStringFlags are the string flags of parley sandbox create and the
// body member each one sets.
var sandboxStringFlags = []struct{ flag, member, usage string }{
	{"profile", "profile", "the profile to create the sandbox from; the hub's default profile when not given"},
	{"restore-from", "restore_from", "the id of a sandbox whose checkpoint the sandbox starts from"},
	{"ttl", "ttl", "how long the sandbox may live, such as 2h; the profile's when not given"},
	{"purpose", "purpose", "what the sandbox is for"},
	{"session", "session_id", "the agent session the sandbox belongs to"},
}

// newSandboxCommand builds parley sandbox: the hub's sandboxes and the
// profiles they are created from.
func newSandboxCommand() *cobra.Command {
	return group("sandbox", "Create, list and read the sandboxes of a workspace, and list their profiles",
		declare(command{
			Name:        "profiles",
			Description: "List the sandbox profiles the hub offers, in name order",
			Method:      http.MethodGet,
			Path:        "/sandbox-profiles",
			Auth:        authUser,
		}),
		newSandboxCreateCommand(),
		declare(command{
			Name:        "list",
			Description: "List the sandboxes of a workspace, newest first, without deleted ones unless asked",
			Method:      http.MethodGet,
			Path:        sandboxesRoute,
			Args:        []argSpec{workspaceSlugArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.StringArray("state", nil, "list only sandboxes in this state; repeat it for several states")
				fs.String("session", "", "list only the sandboxes of this agent session")
				fs.StringArray("label", nil, "list only sandboxes with this key=value label; repeat it for several, all of which must match")
				fs.Bool("include-forks", false, "list the sandboxes of the workspace's task forks too")
				fs.Int("limit", 0, "the most sandboxes the page holds; the hub's default when not given")
				fs.String("cursor", "", "the next_cursor of the page before, to read the page after it")
			},
			Run: listSandboxes,
		}),
		declare(command{
			Name:        "get",
			Description: "Print a sandbox of a workspace",
			Method:      http.MethodGet,
			Path:        sandboxesRoute + "/:id",
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
			Auth:        authUser,
		}),
	)
}

// newSandboxCreateCommand builds parley sandbox create. The words after --
// are the sandbox's command, so the command takes exactly one argument
// before them.
func newSandboxCreateCommand() *cobra.Command {
	create := declare(command{
		Name:        "create",
		Description: "Create a sandbox on a workspace; with --wait, wait until it is up, with --wait-exit until its command has ended",
		Method:      http.MethodPost,
		Path:        sandboxesRoute,
		Args:        []argSpec{workspaceSlugArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			for _, f := range sandboxStringFlags {
				fs.String(f.flag, "", f.usage)
			}
			fs.String("ref", "", "check out this branch, tag or commit of the workspace")
			fs.String("branch", "", "the branch to work on in the checkout")
			fs.StringArray("env", nil, "set the variable key=value in the sandbox; repeat it for several")
			fs.StringArray("label", nil, "label the sandbox with key=value; repeat it for several")
			fs.Bool("no-checkout", false, "create the sandbox without a checkout of the workspace")
			fs.Bool(waitExitFlag, false, "wait until the sandbox's command has ended and print the final sandbox; success only when it completed")
		},
		Wait: &waitFor{
			Start: func(c *call) (apiRequest, error) {
				body, err := sandboxCreateBody(c.cmd)
				if err != nil {
					return apiRequest{}, err
				}
				return apiRequest{method: c.decl.Method, path: routePath(c.decl.Path, c.args), body: body}, nil
			},
			Poll: func(c *call, record []byte) (apiRequest, error) {
				var s struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(record, &s); err != nil || s.ID == "" {
					return apiRequest{}, clientError("the hub's answer names no sandbox id")
				}
				return apiRequest{method: http.MethodGet, path: routePath(c.decl.Path, c.args) + "/" + pathSegment(s.ID)}, nil
			},
			// Every state but provisioning means the sandbox is up or
			// will not come up.
			Classify: byState("state", []string{"ready", "completed"}, []string{"failed", "stopped", "deleted"}),
		},
		Run: createSandbox,
	})
	create.Use += " [-- command...]"
	checkLine := create.Args
	// The body is built here too, so that a malformed flag fails with the
	// other usage errors, before any setting is resolved or any request
	// sent (ADR 02).
	create.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, beforeDash(cmd, args)); err != nil {
			return err
		}
		_, err := sandboxCreateBody(cmd)
		return err
	}
	return create
}

// createSandbox is the run step of parley sandbox create: the shared wait,
// which with --wait-exit waits on until the sandbox's command has ended,
// whether --wait is given or not.
func createSandbox(c *call) error {
	if untilExit, _ := c.cmd.Flags().GetBool(waitExitFlag); untilExit {
		w := *c.decl.Wait
		// A ready sandbox is still running its command.
		w.Classify = byState("state", []string{"completed"}, []string{"failed", "stopped", "deleted"})
		c.decl.Wait = &w
		if err := c.cmd.Flags().Set(waitFlag, "true"); err != nil {
			return err
		}
	}
	return c.wait()
}

// sandboxCreateBody is the body of POST /workspaces/:slug/sandboxes for
// cmd's flags: a member for each flag given, even with an empty value,
// which the hub judges, and none for a flag not given, so that the hub's
// default applies. The words after -- are the member command.
func sandboxCreateBody(cmd *cobra.Command) (map[string]any, error) {
	fs := cmd.Flags()
	body := map[string]any{}
	for _, f := range sandboxStringFlags {
		if fs.Changed(f.flag) {
			body[f.member], _ = fs.GetString(f.flag)
		}
	}
	checkout := map[string]any{}
	for _, flag := range []string{"ref", "branch"} {
		if fs.Changed(flag) {
			checkout[flag], _ = fs.GetString(flag)
		}
	}
	for _, f := range []struct{ flag, member string }{{"env", "env"}, {"label", "labels"}} {
		if !fs.Changed(f.flag) {
			continue
		}
		values, _ := fs.GetStringArray(f.flag)
		pairs, err := keyValues(cmd, f.flag, values)
		if err != nil {
			return nil, err
		}
		body[f.member] = pairs
	}
	noCheckout, _ := fs.GetBool("no-checkout")
	switch {
	case noCheckout && len(checkout) > 0:
		return nil, usageError("%s takes --no-checkout without --ref and --branch", cmd.CommandPath())
	case noCheckout:
		body["checkout"] = false
	case len(checkout) > 0:
		body["checkout"] = checkout
	}
	command := afterDash(cmd)
	if command != nil {
		body["command"] = command
	}
	if untilExit, _ := fs.GetBool(waitExitFlag); untilExit && command == nil {
		return nil, usageError("%s needs a command after -- with --%s", cmd.CommandPath(), waitExitFlag)
	}
	return body, nil
}

// keyValues is the map of the key=value values of the repeated flag, each
// split at its first =. A value without = or with an empty key is a usage
// error.
func keyValues(cmd *cobra.Command, flag string, values []string) (map[string]string, error) {
	pairs := make(map[string]string, len(values))
	for _, v := range values {
		key, value, found := strings.Cut(v, "=")
		if !found || key == "" {
			return nil, usageError("%s --%s takes key=value with a key that is not empty, got %q", cmd.CommandPath(), flag, v)
		}
		pairs[key] = value
	}
	return pairs, nil
}

// listSandboxes is the run step of parley sandbox list: it sends a query
// parameter for each flag given, one per occurrence of a repeated flag, and
// none for a flag not given, and prints the page. Values are sent as given,
// so that the hub's own error says what is wrong with one.
func listSandboxes(c *call) error {
	fs := c.cmd.Flags()
	query := url.Values{}
	for _, flag := range []string{"state", "label"} {
		values, _ := fs.GetStringArray(flag)
		for _, v := range values {
			query.Add(flag, v)
		}
	}
	if forks, _ := fs.GetBool("include-forks"); forks {
		query.Set("include_forks", "true")
	}
	for flag, param := range map[string]string{"session": "session_id", "limit": "limit", "cursor": "cursor"} {
		if f := fs.Lookup(flag); f.Changed {
			query.Set(param, f.Value.String())
		}
	}
	return sendWith(c, query, nil)
}
