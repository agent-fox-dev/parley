package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/net/websocket"
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

// sandboxCameUp classifies a sandbox a command waits on until it is up:
// every state but provisioning means the sandbox is up or will not come
// up.
var sandboxCameUp = byState("state", []string{"ready", "completed"}, []string{"failed", "stopped", "deleted"})

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
	sandbox := group("sandbox", "Create, list and read the sandboxes of a workspace, run commands and shells in them, copy files in and out, read their logs, checkpoint, stop, start, extend and delete them, list their profiles, decide their proposals for network access, and list and delete the sandbox rules of a workspace",
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
		declare(command{
			Name:        "shell",
			Description: "Open an interactive shell in a sandbox on the local terminal; the sandbox keeps running when it ends",
			Method:      http.MethodGet,
			Path:        sandboxesRoute + "/:id/attach",
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
			Auth:        authUser,
			Interactive: true,
			Run:         shellInSandbox,
		}),
		newSandboxLogsCommand(),
		declare(command{
			Name:        "upload",
			Description: "Copy a local file, with its mode, or a local directory into a sandbox's working directory and print what the hub wrote",
			Method:      http.MethodPut,
			Path:        sandboxFilesRoute,
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg, {Name: "local-path", Required: true}, {Name: "sandbox-path", Required: true}},
			Auth:        authUser,
			Run:         uploadToSandbox,
		}),
		declare(command{
			Name:        "download",
			Description: "Copy a file or a directory of a sandbox to a local path, or a file to stdout with -, and print the path and the bytes written",
			Method:      http.MethodGet,
			Path:        sandboxFilesRoute,
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg, {Name: "sandbox-path", Required: true}, {Name: "local-path", Required: true}},
			Auth:        authUser,
			Run:         downloadFromSandbox,
		}),
	)
	sandbox.AddCommand(sandboxLifecycleCommands()...)
	sandbox.AddCommand(sandboxPolicyCommands()...)
	return sandbox
}

// sandboxLifecycleCommands builds the commands that change a sandbox's
// life: parley sandbox checkpoint, stop, start, extend and delete, each on
// <workspace-slug> <id>.
func sandboxLifecycleCommands() []*cobra.Command {
	return []*cobra.Command{
		declare(command{
			Name:        "checkpoint",
			Description: "Save a running sandbox's working directory as its checkpoint and print the sandbox",
			Method:      http.MethodPost,
			Path:        sandboxesRoute + "/:id/checkpoint",
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
			Auth:        authUser,
		}),
		declare(command{
			Name:        "stop",
			Description: "Stop a running sandbox, keeping its working directory, and print the sandbox",
			Method:      http.MethodPost,
			Path:        sandboxesRoute + "/:id/stop",
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
			Auth:        authUser,
		}),
		declare(command{
			Name:        "start",
			Description: "Start a stopped or ended sandbox again and print the sandbox; with --wait, wait until it is up",
			Method:      http.MethodPost,
			Path:        sandboxesRoute + "/:id/start",
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
			Auth:        authUser,
			Wait: &waitFor{
				Poll: func(c *call, _ []byte) (apiRequest, error) {
					return apiRequest{method: http.MethodGet, path: routePath(sandboxesRoute+"/:id", c.args)}, nil
				},
				Classify: sandboxCameUp,
			},
		}),
		declare(command{
			Name:        "extend",
			Description: "Give a sandbox a new time limit, counted from now, and print the sandbox",
			Method:      http.MethodPatch,
			Path:        sandboxesRoute + "/:id",
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.String("ttl", "", "the sandbox's new time limit from now, such as 2h, within its profile's maximum")
				_ = cobra.MarkFlagRequired(fs, "ttl")
			},
			Run: func(c *call) error {
				ttl, _ := c.cmd.Flags().GetString("ttl")
				return sendWith(c, nil, map[string]any{"ttl": ttl})
			},
		}),
		declare(command{
			Name:        "delete",
			Description: "Delete a sandbox, saving its working directory as a checkpoint first unless --no-checkpoint is given",
			Method:      http.MethodDelete,
			Path:        sandboxesRoute + "/:id",
			Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.Bool("no-checkpoint", false, "delete the sandbox without saving its working directory first")
			},
			Run: deleteSandbox,
			// The hub answers a delete with no body.
			Confirm: func(c *call) any {
				return struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				}{c.args[sandboxIDArg.Name], "deleted"}
			},
		}),
	}
}

// deleteSandbox is the run step of parley sandbox delete: it sends
// checkpoint=false only with --no-checkpoint, so that otherwise the hub's
// default, a checkpoint before the delete, applies.
func deleteSandbox(c *call) error {
	var query url.Values
	if skip, _ := c.cmd.Flags().GetBool("no-checkpoint"); skip {
		query = url.Values{"checkpoint": {"false"}}
	}
	return sendWith(c, query, nil)
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
			Classify: sandboxCameUp,
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

// maxExecAnswerSize bounds the collected answer of an exec, in bytes. The
// hub keeps up to 1 MiB of each of stdout and stderr and escapes a control
// or HTML byte to six bytes in JSON, so a valid answer can reach 12 MiB and
// some members; the answer of any other route stays under maxAnswerSize.
const maxExecAnswerSize = 2*6*(1<<20) + 4<<20

// execCollected sends r for the collected answer, prints it as the result
// and ends as its exit_code and timed_out say.
func execCollected(c *call, r apiRequest) error {
	resp, err := c.client.open(c.ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxExecAnswerSize+1))
	if err != nil {
		return clientError("cannot read the answer of the hub at %s: %v", c.client.hubURL, err)
	}
	if len(answer) > maxExecAnswerSize {
		return clientError("the hub answered %d with a body larger than %d bytes", resp.StatusCode, maxExecAnswerSize)
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

// logSources are the values parley sandbox logs --source takes.
var logSources = []string{"process", "policy"}

// newSandboxLogsCommand builds parley sandbox logs.
func newSandboxLogsCommand() *cobra.Command {
	logs := declare(command{
		Name:        "logs",
		Description: "Print the stored lines of a sandbox's main process and network policy, one JSON object per line; with --follow, go on printing new lines as they are stored",
		Method:      http.MethodGet,
		Path:        sandboxesRoute + "/:id/logs",
		Args:        []argSpec{workspaceSlugArg, sandboxIDArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("source", "", "print only the lines of this source, process or policy")
			fs.String("since", "", "print only the lines stored at or after this time, such as 2026-01-02T03:04:05Z")
			fs.Int("tail", 0, "print only the last n lines, at least 1")
			fs.Bool("follow", false, "go on printing new lines as they are stored, until the sandbox is deleted or the hub stops")
			markUntilInterrupted(fs, "follow")
		},
		Run: printSandboxLogs,
	})
	checkLine := logs.Args
	// The query is built here too, so that a value parley can judge fails
	// with the other usage errors, before any request is sent (ADR 02).
	logs.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		_, err := sandboxLogsQuery(cmd)
		return err
	}
	return logs
}

// sandboxLogsQuery is the query of GET /workspaces/:slug/sandboxes/:id/logs
// for cmd's filter flags, a parameter only for each flag given. --since is
// sent as given, for the hub to judge.
func sandboxLogsQuery(cmd *cobra.Command) (url.Values, error) {
	fs := cmd.Flags()
	query := url.Values{}
	if fs.Changed("source") {
		source, _ := fs.GetString("source")
		if !slices.Contains(logSources, source) {
			return nil, usageError("%s --source takes process or policy, got %q", cmd.CommandPath(), source)
		}
		query.Set("source", source)
	}
	if fs.Changed("since") {
		since, _ := fs.GetString("since")
		query.Set("since", since)
	}
	if fs.Changed("tail") {
		tail, _ := fs.GetInt("tail")
		if tail < 1 {
			return nil, usageError("%s --tail takes a number of lines of at least 1, got %d", cmd.CommandPath(), tail)
		}
		query.Set("tail", strconv.Itoa(tail))
	}
	return query, nil
}

// printSandboxLogs is the run step of parley sandbox logs: every page of the
// selected lines, following next_cursor until there is none, or with
// --follow the hub's stream of them. The filters are sent on every page,
// since a cursor holds only the position.
func printSandboxLogs(c *call) error {
	query, err := sandboxLogsQuery(c.cmd)
	if err != nil {
		return err
	}
	r := apiRequest{method: c.decl.Method, path: routePath(c.decl.Path, c.args), query: query}
	if follow, _ := c.cmd.Flags().GetBool("follow"); follow {
		return followSandboxLogs(c, r)
	}
	for {
		resp, err := c.client.send(c.ctx, r)
		if err != nil {
			return err
		}
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor *string           `json:"next_cursor"`
		}
		if err := json.Unmarshal(resp.body, &page); err != nil {
			return clientError("the hub answered %d with a body that is not a page of log lines", resp.status)
		}
		for _, item := range page.Items {
			if err := printLogLine(c, item); err != nil {
				return err
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return nil
		}
		r.query.Set("cursor", *page.NextCursor)
	}
}

// followSandboxLogs opens the stream of r's lines without a request timeout
// and prints the data of each log frame as it arrives. The hub ends the
// stream once the sandbox is deleted or the hub stops, which ends the
// command with success; so does an interrupt, since the user follows the
// lines until they stop.
func followSandboxLogs(c *call, r apiRequest) error {
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt)
	defer stop()
	r.query.Set("follow", "true")
	r.accept = "text/event-stream"
	resp, err := c.client.open(ctx, r)
	if err != nil {
		if ctx.Err() != nil && c.ctx.Err() == nil {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	frames := newFrameScanner(resp.Body)
	for f, ok := frames.next(); ok; f, ok = frames.next() {
		if f.event != "log" {
			continue
		}
		if err := printLogLine(c, []byte(f.data)); err != nil {
			return err
		}
	}
	if err := frames.err(); err != nil && ctx.Err() == nil {
		return clientError("the log stream of the hub at %s broke: %v", c.client.hubURL, err)
	}
	return nil
}

// printLogLine prints the log line line, a JSON object, compacted on a line
// of its own.
func printLogLine(c *call, line []byte) error {
	var out bytes.Buffer
	if err := json.Compact(&out, line); err != nil {
		return clientError("the hub sent a log line that is not JSON: %v", err)
	}
	out.WriteByte('\n')
	if _, err := c.stdout.Write(out.Bytes()); err != nil {
		return clientError("cannot print a log line: %v", err)
	}
	return nil
}

// shellInSandbox is the run step of parley sandbox shell: it opens the
// sandbox's terminal at the size of the local one and relays it until the
// hub closes the connection, which ends the command with success. An
// interrupt or a termination closes the connection too, so that the local
// terminal is restored on the way out.
func shellInSandbox(c *call) error {
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	local := localTerminalOf(c.opts.stdin)
	cols, rows := local.size()
	query := url.Values{"cols": {strconv.Itoa(cols)}, "rows": {strconv.Itoa(rows)}}
	ws, err := c.client.dialTerminal(ctx, apiRequest{method: c.decl.Method, path: routePath(c.decl.Path, c.args), query: query})
	if err != nil {
		return err
	}
	defer ws.Close()
	return local.relay(ctx, ws, c.stdout, cols, rows)
}

// dialTerminal opens r, a GET of a terminal route, as a WebSocket with the
// client's credential, the connection bounded by the client's timeout and
// the session by ctx alone. The hub refuses a terminal with an error
// envelope before the upgrade, which the WebSocket handshake does not keep,
// so a refusal is asked for again through refusalOf.
func (c *client) dialTerminal(ctx context.Context, r apiRequest) (*websocket.Conn, error) {
	target, err := url.Parse(c.hubURL + apiPrefix + r.path + "?" + r.query.Encode())
	if err != nil {
		return nil, clientError("cannot build a request to the hub at %s: %v", c.hubURL, err)
	}
	switch target.Scheme {
	case "http":
		target.Scheme = "ws"
	case "https":
		target.Scheme = "wss"
	default:
		return nil, clientError("the hub URL %s is neither http nor https", c.hubURL)
	}
	config, err := websocket.NewConfig(target.String(), c.hubURL)
	if err != nil {
		return nil, clientError("cannot build a request to the hub at %s: %v", c.hubURL, err)
	}
	if c.credential != "" {
		config.Header.Set("Authorization", "Bearer "+c.credential)
	}
	dialCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ws, err := config.DialContext(dialCtx)
	if err == nil {
		return ws, nil
	}
	var dial *websocket.DialError
	if errors.As(err, &dial) {
		if dial.Err == websocket.ErrBadStatus {
			return nil, c.refusalOf(ctx, r)
		}
		err = dial.Err
	}
	if ctx.Err() == nil && errors.Is(dialCtx.Err(), context.DeadlineExceeded) {
		return nil, clientError("the hub at %s did not open a terminal within %v", c.hubURL, c.timeout)
	}
	return nil, clientError("cannot open a terminal on the hub at %s: %v", c.hubURL, err)
}

// refusalOf sends r again as an upgrade request through the client, to read
// the hub's error envelope, and returns it as the error. The request asks
// for the upgrade, since the hub judges a request that does not ask for one
// before the state of the sandbox. An upgrade granted this time is closed
// unused, and is a client_error, as is an answer that is no error envelope.
func (c *client) refusalOf(ctx context.Context, r apiRequest) error {
	key := make([]byte, 16)
	rand.Read(key)
	r.header = http.Header{
		"Upgrade":               {"websocket"},
		"Connection":            {"Upgrade"},
		"Sec-WebSocket-Key":     {base64.StdEncoding.EncodeToString(key)},
		"Sec-WebSocket-Version": {"13"},
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := c.newRequest(reqCtx, r)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.failure(ctx, reqCtx, c.timeout, "cannot reach", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return clientError("the hub at %s refused the terminal, then opened it when asked again", c.hubURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerSize+1))
	if err != nil {
		return c.failure(ctx, reqCtx, c.timeout, "cannot read the answer of", err)
	}
	if len(body) > maxAnswerSize {
		return clientError("the hub answered %d with a body larger than %d bytes", resp.StatusCode, maxAnswerSize)
	}
	if err := failureOf(&response{status: resp.StatusCode, body: body}); err != nil {
		return err
	}
	return clientError("the hub at %s answered %d instead of opening the terminal", c.hubURL, resp.StatusCode)
}

// sandboxFilesRoute is the route that moves files in and out of a sandbox.
const sandboxFilesRoute = sandboxesRoute + "/:id/files"

// The media types of a transfer's body: one file's bytes, or a tar stream.
const (
	mediaOctets = "application/octet-stream"
	mediaTar    = "application/x-tar"
)

// uploadToSandbox is the run step of parley sandbox upload: a regular file
// is sent as it is, with its permission bits as mode; a directory as a tar
// stream of its regular files and directories, written while it is sent.
// The local path is read before anything is sent, so one that cannot be
// read sends no request. The upload has no request timeout: it takes as
// long as its bytes take.
func uploadToSandbox(c *call) error {
	local := c.args["local-path"]
	r := apiRequest{
		method: c.decl.Method,
		path:   routePath(c.decl.Path, c.args),
		query:  url.Values{"path": {c.args["sandbox-path"]}},
	}
	info, err := os.Stat(local)
	if err != nil {
		return clientError("cannot read %s: %v", local, err)
	}
	switch {
	case info.Mode().IsRegular():
		f, err := os.Open(local)
		if err != nil {
			return clientError("cannot read %s: %v", local, err)
		}
		// The client closes the file once it is sent; this closes it when
		// the request is never sent.
		defer f.Close()
		r.query.Set("mode", fmt.Sprintf("%04o", info.Mode().Perm()))
		r.payload, r.payloadType, r.payloadLength = f, mediaOctets, info.Size()
	case info.IsDir():
		entries, err := localTree(c, local)
		if err != nil {
			return err
		}
		return uploadTree(c, r, entries)
	default:
		return clientError("%s is neither a regular file nor a directory", local)
	}
	resp, err := c.client.open(c.ctx, r)
	if err != nil {
		return err
	}
	return printUploaded(c, resp)
}

// uploadTree sends r with the tar stream of entries as its body, written
// while it is sent, and prints the hub's answer.
func uploadTree(c *call, r apiRequest, entries []localEntry) error {
	pr, pw := io.Pipe()
	written := make(chan error, 1)
	go func() {
		err := writeTar(pw, entries)
		pw.CloseWithError(err)
		written <- err
	}()
	r.query.Set("archive", "tar")
	r.payload, r.payloadType, r.payloadLength = pr, mediaTar, -1
	resp, err := c.client.open(c.ctx, r)
	// Closing the reader ends the writer when the hub answered or the
	// request failed before the stream was read to its end.
	_ = pr.Close()
	werr := <-written
	if err != nil {
		// A stream that failed on the local side says why the request
		// failed better than the broken request does.
		var local *cliError
		if errors.As(werr, &local) {
			return werr
		}
		return err
	}
	return printUploaded(c, resp)
}

// printUploaded prints the hub's answer to an upload, the document of
// what it wrote.
func printUploaded(c *call, resp *http.Response) error {
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerSize+1))
	if err != nil {
		return clientError("cannot read the answer of the hub at %s: %v", c.client.hubURL, err)
	}
	if len(answer) > maxAnswerSize || !json.Valid(answer) {
		return clientError("the hub answered %d with a body that is not one JSON document", resp.StatusCode)
	}
	return c.printResult(answer)
}

// localEntry is a regular file or a directory below the directory an
// upload sends, with its name in the tar stream.
type localEntry struct {
	path, name string
	dir        bool
}

// localTree is every regular file and directory below dir, in lexical
// order, read before the upload is sent so that a directory or a file that
// cannot be read sends no request. A symbolic link is not followed but left out with
// a warning, as is anything else that is neither a file nor a directory.
func localTree(c *call, dir string) ([]localEntry, error) {
	root := dir
	// A directory named through a link is the directory it points to; any
	// other link below it is left out.
	if info, err := os.Lstat(dir); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		if root, err = filepath.EvalSymlinks(dir); err != nil {
			return nil, clientError("cannot read %s: %v", dir, err)
		}
	}
	var entries []localEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		shown := filepath.Join(dir, strings.TrimPrefix(p, root))
		if err != nil {
			return clientError("cannot read %s: %v", shown, err)
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return clientError("cannot read %s: %v", shown, err)
		}
		name := filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			entries = append(entries, localEntry{path: p, name: name + "/", dir: true})
		case d.Type().IsRegular():
			// The file is read only while the body streams; opening it
			// now makes a file that cannot be read send no request.
			f, err := os.Open(p)
			if err != nil {
				return clientError("cannot read %s: %v", shown, err)
			}
			_ = f.Close()
			entries = append(entries, localEntry{path: p, name: name})
		case d.Type()&fs.ModeSymlink != 0:
			warn(c.stderr, "the symbolic link %s is not uploaded", shown)
		default:
			warn(c.stderr, "%s is neither a regular file nor a directory and is not uploaded", shown)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// writeTar writes the tar stream of entries to w, each with its permission
// bits and modification time; the hub keeps the modes. A local path it
// cannot read is a client_error; a failure to write to w is returned as it
// is, since the request reports it.
func writeTar(w io.Writer, entries []localEntry) error {
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if err := writeTarEntry(tw, e); err != nil {
			return err
		}
	}
	return tw.Close()
}

// writeTarEntry writes the entry e: a directory's header, or a file's
// header and bytes, sized as the file is when it is opened.
func writeTarEntry(tw *tar.Writer, e localEntry) error {
	if e.dir {
		info, err := os.Stat(e.path)
		if err != nil {
			return clientError("cannot read %s: %v", e.path, err)
		}
		hdr := &tar.Header{Typeflag: tar.TypeDir, Name: e.name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()}
		return tw.WriteHeader(hdr)
	}
	f, err := os.Open(e.path)
	if err != nil {
		return clientError("cannot read %s: %v", e.path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return clientError("cannot read %s: %v", e.path, err)
	}
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: e.name, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime()}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, localReader{f, e.path}, info.Size()); err != nil {
		if errors.Is(err, io.EOF) {
			return clientError("%s became shorter while it was sent", e.path)
		}
		return err
	}
	return nil
}

// localReader reads a local file of an upload, its failures client_errors,
// so that they are told apart from a failure to send its bytes.
type localReader struct {
	r    io.Reader
	path string
}

func (l localReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = clientError("cannot read %s: %v", l.path, err)
	}
	return n, err
}

// errorTypePathIsDirectory is the hub's answer to a download of a
// directory asked for as a file.
const errorTypePathIsDirectory = "path_is_directory"

// downloadFromSandbox is the run step of parley sandbox download: the file
// at the sandbox path, or, when the hub answers that it is a directory, a
// tar stream of the directory, asked for again. Neither request has a
// timeout: a transfer takes as long as its bytes take. Nothing is written
// at the local path before the hub has answered with the bytes.
func downloadFromSandbox(c *call) error {
	local := c.args["local-path"]
	r := apiRequest{
		method: c.decl.Method,
		path:   routePath(c.decl.Path, c.args),
		query:  url.Values{"path": {c.args["sandbox-path"]}},
		accept: mediaOctets,
	}
	resp, err := c.client.open(c.ctx, r)
	tarred := false
	if he := (*hubError)(nil); errors.As(err, &he) && errorTypeOf(he.body) == errorTypePathIsDirectory {
		tarred = true
		r.query.Set("archive", "tar")
		r.accept = mediaTar
		resp, err = c.client.open(c.ctx, r)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var n int64
	switch {
	case local == "-":
		// A directory goes to stdout as the tar stream itself.
		if _, err := io.Copy(c.stdout, resp.Body); err != nil {
			return clientError("cannot copy the download from the hub at %s to stdout: %v", c.client.hubURL, err)
		}
		return nil
	case tarred:
		n, err = unpackTar(resp.Body, local)
	default:
		n, err = writeLocalFile(resp.Body, local)
	}
	if err != nil {
		return err
	}
	result, err := json.Marshal(struct {
		Path  string `json:"path"`
		Bytes int64  `json:"bytes"`
	}{local, n})
	if err != nil {
		return clientError("cannot encode the result: %v", err)
	}
	return c.printResult(result)
}

// downloadedFileMode is the mode of a downloaded file: the hub sends none.
const downloadedFileMode = 0o644

// writeLocalFile writes body to the file at path through a temporary file
// in the same directory, renamed onto path once the whole body is written,
// so that path holds either all of it or what it held before. It returns
// the bytes written.
func writeLocalFile(body io.Reader, path string) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".parley-*")
	if err != nil {
		return 0, clientError("cannot write %s: %v", path, err)
	}
	renamed := false
	defer func() {
		if !renamed {
			tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	n, err := io.Copy(tmp, body)
	if err != nil {
		return 0, clientError("the download of %s broke: %v", path, err)
	}
	if err := tmp.Chmod(downloadedFileMode); err != nil {
		return 0, clientError("cannot write %s: %v", path, err)
	}
	if err := tmp.Close(); err != nil {
		return 0, clientError("cannot write %s: %v", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, clientError("cannot write %s: %v", path, err)
	}
	renamed = true
	return n, nil
}

// unpackTar unpacks the tar stream r into the directory dir, creating it,
// with the modes of its entries, and returns the sum of the sizes of the
// files it wrote. An entry whose name is absolute or leaves dir is refused,
// and so is one that is neither a regular file nor a directory; every
// write goes through an os.Root of dir, so nothing is written outside it,
// through a link either. When unpacking fails, a dir it created is removed
// again.
func unpackTar(r io.Reader, dir string) (int64, error) {
	_, statErr := os.Stat(dir)
	created := errors.Is(statErr, fs.ErrNotExist)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, clientError("cannot create %s: %v", dir, err)
	}
	n, err := unpackInto(r, dir)
	if err != nil && created {
		os.RemoveAll(dir)
	}
	return n, err
}

// unpackInto is unpackTar's work, in a dir that exists. Directory modes are
// set last, so that a directory without write permission is still filled.
func unpackInto(r io.Reader, dir string) (int64, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, clientError("cannot open %s: %v", dir, err)
	}
	defer root.Close()
	type dirMode struct {
		name string
		mode fs.FileMode
	}
	var dirs []dirMode
	var n int64
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, clientError("the tar stream of the download broke: %v", err)
		}
		name := hdr.Name
		if path.IsAbs(name) || filepath.IsAbs(name) || !filepath.IsLocal(filepath.FromSlash(name)) {
			return 0, clientError("the tar stream holds the entry %q, which lies outside %s", name, dir)
		}
		name = filepath.FromSlash(path.Clean(name))
		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return 0, clientError("cannot create %s: %v", filepath.Join(dir, name), err)
			}
			dirs = append(dirs, dirMode{name, mode})
		case tar.TypeReg:
			written, err := unpackFile(root, name, mode, tr)
			if err != nil {
				return 0, clientError("cannot write %s: %v", filepath.Join(dir, name), err)
			}
			n += written
		default:
			return 0, clientError("the tar stream holds the entry %q, which is neither a regular file nor a directory", hdr.Name)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := root.Chmod(dirs[i].name, dirs[i].mode); err != nil {
			return 0, clientError("cannot set the mode of %s: %v", filepath.Join(dir, dirs[i].name), err)
		}
	}
	return n, nil
}

// unpackFile writes the file name below root from r, creating its parent
// directories, with mode, and returns its size.
func unpackFile(root *os.Root, name string, mode fs.FileMode, r io.Reader) (int64, error) {
	if parent := filepath.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return 0, err
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	return n, root.Chmod(name, mode)
}
