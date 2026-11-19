package cli

import (
	"encoding/base64"
	"encoding/json"
	"io"
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
		newSandboxExecCommand(),
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

// maxExecStdin is the most bytes parley sandbox exec --stdin sends, the
// hub's limit on a command's standard input.
const maxExecStdin = 4 << 20

// maxExitCode is the highest exit status parley sandbox exec passes on from
// the command; higher ones are the shell's own statuses (126 and above), so
// they are reported as 125.
const maxExitCode = 125

// newSandboxExecCommand builds parley sandbox exec. The words after -- are
// the command, which is required, so the command takes exactly two
// arguments before them.
func newSandboxExecCommand() *cobra.Command {
	exec := declare(command{
		Name:        "exec",
		Description: "Run a command in a sandbox, write its output as it arrives, and exit with its exit code; with --json, print the collected answer",
		Method:      http.MethodPost,
		Path:        sandboxesRoute + "/:id/exec",
		Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("workdir", "", "the directory the command runs in; the sandbox's working directory when not given")
			fs.StringArray("env", nil, "set the variable key=value for the command; repeat it for several")
			fs.String("timeout", "", "how long the command may run before it is killed, such as 90s; the hub's default when not given")
			fs.Bool("stdin", false, "send the whole of standard input to the command, at most 4 MiB")
			fs.Bool("json", false, "print the hub's answer, the collected output and exit code, as one JSON document")
		},
		Run: execInSandbox,
	})
	exec.Use += " -- <command...>"
	checkLine := exec.Args
	// The body is built here too, so that a missing command or a malformed
	// flag fails with the other usage errors, before any setting is
	// resolved or any request sent (ADR 02).
	exec.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, beforeDash(cmd, args)); err != nil {
			return err
		}
		_, err := sandboxExecBody(cmd)
		return err
	}
	return exec
}

// sandboxExecBody is the body of POST /workspaces/:slug/sandboxes/:id/exec
// for cmd's flags, without the standard input: the words after -- as
// command, and workdir, env and timeout only for the flags given, so that
// the hub's default applies to the others. Values are sent as given, for
// the hub to judge.
func sandboxExecBody(cmd *cobra.Command) (map[string]any, error) {
	command := afterDash(cmd)
	if command == nil {
		return nil, usageError("%s needs a command after --", cmd.CommandPath())
	}
	fs := cmd.Flags()
	body := map[string]any{"command": command}
	for _, flag := range []string{"workdir", "timeout"} {
		if fs.Changed(flag) {
			body[flag], _ = fs.GetString(flag)
		}
	}
	if fs.Changed("env") {
		values, _ := fs.GetStringArray("env")
		env, err := keyValues(cmd, "env", values)
		if err != nil {
			return nil, err
		}
		body["env"] = env
	}
	return body, nil
}

// execInSandbox is the run step of parley sandbox exec. The request has no
// timeout of its own: the command runs as long as the hub lets it.
func execInSandbox(c *call) error {
	body, err := sandboxExecBody(c.cmd)
	if err != nil {
		return err
	}
	if withStdin, _ := c.cmd.Flags().GetBool("stdin"); withStdin {
		stdin, err := io.ReadAll(io.LimitReader(c.opts.stdin, maxExecStdin+1))
		if err != nil {
			return clientError("cannot read standard input: %v", err)
		}
		if len(stdin) > maxExecStdin {
			return clientError("standard input holds more than %d bytes, the most --stdin sends", maxExecStdin)
		}
		body["stdin_base64"] = base64.StdEncoding.EncodeToString(stdin)
	}
	r := apiRequest{method: c.decl.Method, path: routePath(c.decl.Path, c.args), body: body}
	if collected, _ := c.cmd.Flags().GetBool("json"); collected {
		return execCollected(c, r)
	}
	r.accept = "text/event-stream"
	return execStreamed(c, r)
}

// execEnd is how a command ended, as the hub reports it in the exit frame
// and in the collected answer.
type execEnd struct {
	ExitCode *int `json:"exit_code"`
	TimedOut bool `json:"timed_out"`
}

// outcome is the error parley ends with for the command's end: none for
// exit code 0, the exit code itself (above 125 as 125) for another, and
// wait_timeout for a command killed at its timeout.
func (e execEnd) outcome() error {
	switch {
	case e.TimedOut:
		return waitTimeout("the command did not end within its timeout and was killed")
	case e.ExitCode == nil:
		return clientError("the hub reported neither an exit code nor a timeout")
	case *e.ExitCode == 0:
		return nil
	case *e.ExitCode < 0 || *e.ExitCode > maxExitCode:
		return &commandExit{maxExitCode}
	}
	return &commandExit{*e.ExitCode}
}

// execStreamed sends r and writes the output frames of the event stream to
// stdout and stderr as they arrive, then ends as the exit frame says. A
// stream that ends or breaks before the exit frame is a client_error, since
// the command's end is unknown.
func execStreamed(c *call, r apiRequest) error {
	resp, err := c.client.open(c.ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	frames := newFrameScanner(resp.Body)
	for f, ok := frames.next(); ok; f, ok = frames.next() {
		switch f.event {
		case "stdout", "stderr":
			if err := writeOutputFrame(c, f); err != nil {
				return err
			}
		case "exit":
			var end execEnd
			if err := json.Unmarshal([]byte(f.data), &end); err != nil {
				return clientError("the hub sent an exit frame that is not JSON: %v", err)
			}
			return end.outcome()
		}
	}
	if err := frames.err(); err != nil {
		return clientError("the stream of the hub at %s broke before the command's exit: %v", c.client.hubURL, err)
	}
	return clientError("the stream of the hub at %s ended before the command's exit", c.client.hubURL)
}

// writeOutputFrame writes the bytes of an output frame, data as text or
// data_base64 decoded, to the stream it is named for.
func writeOutputFrame(c *call, f frame) error {
	var out struct {
		Data       *string `json:"data"`
		DataBase64 *string `json:"data_base64"`
	}
	if err := json.Unmarshal([]byte(f.data), &out); err != nil {
		return clientError("the hub sent a %s frame that is not JSON: %v", f.event, err)
	}
	var b []byte
	switch {
	case out.Data != nil:
		b = []byte(*out.Data)
	case out.DataBase64 != nil:
		decoded, err := base64.StdEncoding.DecodeString(*out.DataBase64)
		if err != nil {
			return clientError("the hub sent a %s frame whose data_base64 is not base64: %v", f.event, err)
		}
		b = decoded
	}
	w := c.stdout
	if f.event == "stderr" {
		w = c.stderr
	}
	if _, err := w.Write(b); err != nil {
		return clientError("cannot write the command's %s: %v", f.event, err)
	}
	return nil
}

// execCollected sends r for the collected answer, prints it as the result
// and ends as its exit_code and timed_out say.
func execCollected(c *call, r apiRequest) error {
	resp, err := c.client.open(c.ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerSize+1))
	if err != nil {
		return clientError("cannot read the answer of the hub at %s: %v", c.client.hubURL, err)
	}
	if len(answer) > maxAnswerSize {
		return clientError("the hub answered %d with a body larger than %d bytes", resp.StatusCode, maxAnswerSize)
	}
	var end execEnd
	if err := json.Unmarshal(answer, &end); err != nil {
		return clientError("the hub answered %d with a body that is not a JSON object", resp.StatusCode)
	}
	if err := c.printResult(answer); err != nil {
		return err
	}
	return end.outcome()
}
