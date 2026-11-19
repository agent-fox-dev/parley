package cli

import (
	"encoding/json"
	"net/http"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// sessionIDArg is the <session-id> argument: the id of an agent session.
var sessionIDArg = argSpec{Name: "session-id", Required: true}

// sessionFlag is an optional flag of a parley session command, sent as one
// body member when given and as none when not, so that the hub's default
// applies.
type sessionFlag struct {
	flag, member, usage string
	// count marks a flag that takes an integer, which the flag parser
	// checks before any request.
	count bool
}

// add adds f to fs.
func (f sessionFlag) add(fs *pflag.FlagSet) {
	if f.count {
		fs.Int64(f.flag, 0, f.usage)
		return
	}
	fs.String(f.flag, "", f.usage)
}

// addTo sets f's member of body when f was given.
func (f sessionFlag) addTo(body map[string]any, fs *pflag.FlagSet) {
	if !fs.Changed(f.flag) {
		return
	}
	if f.count {
		body[f.member], _ = fs.GetInt64(f.flag)
		return
	}
	body[f.member], _ = fs.GetString(f.flag)
}

// sessionWrite declares a POST of a parley session command whose body holds
// fixed, the members its own run puts there, and one member for each of
// flags given.
func sessionWrite(decl command, flags []sessionFlag, fixed func(c *call) (map[string]any, error)) *cobra.Command {
	decl.Method = http.MethodPost
	decl.Auth = authUser
	own := decl.Flags
	decl.Flags = func(fs *pflag.FlagSet) {
		if own != nil {
			own(fs)
		}
		for _, f := range flags {
			f.add(fs)
		}
	}
	decl.Run = func(c *call) error {
		body, err := fixed(c)
		if err != nil {
			return err
		}
		fs := c.cmd.Flags()
		for _, f := range flags {
			f.addTo(body, fs)
		}
		return sendWith(c, nil, body)
	}
	return declare(decl)
}

// newSessionCommand builds parley session: an agent's record of its work
// on a workspace and the tokens it spent.
func newSessionCommand() *cobra.Command {
	return group("session", "Start, report the token usage of, complete and read agent sessions",
		sessionWrite(command{
			Name:        "start",
			Description: "Start an agent session on a workspace you may write to",
			Path:        "/sessions",
			Args:        []argSpec{{Name: "workspace-slug", Required: true}},
			Flags: func(fs *pflag.FlagSet) {
				fs.String("metadata", "", "a JSON document kept with the session")
			},
		}, []sessionFlag{
			{flag: "id", member: "id", usage: "the session's id, a UUID (default: one the hub draws); starting it again answers the stored session"},
			{flag: "run-id", member: "run_id", usage: "the id of the run the session belongs to"},
			{flag: "node-id", member: "node_id", usage: "the node of the run the session works on"},
			{flag: "archetype", member: "archetype", usage: "the kind of agent the session runs"},
			{flag: "model", member: "model", usage: "the model the session runs on"},
		}, startBody),
		sessionWrite(command{
			Name:        "usage",
			Description: "Report token usage of an active session",
			Path:        "/sessions/:session-id/usage",
			Args:        []argSpec{sessionIDArg},
			Flags: func(fs *pflag.FlagSet) {
				fs.String("model", "", "the model the tokens were spent on")
				// It fails only for a flag the set does not hold, and the
				// line above adds it.
				_ = cobra.MarkFlagRequired(fs, "model")
			},
		}, []sessionFlag{
			{flag: "input", member: "input_tokens", usage: "the input tokens spent", count: true},
			{flag: "output", member: "output_tokens", usage: "the output tokens spent", count: true},
			{flag: "cache-read", member: "cache_read_tokens", usage: "the input tokens read from the cache", count: true},
			{flag: "id", member: "id", usage: "the usage record's id, a UUID (default: one the hub draws); reporting it again changes nothing"},
		}, func(c *call) (map[string]any, error) {
			model, _ := c.cmd.Flags().GetString("model")
			return map[string]any{"model": model}, nil
		}),
		sessionWrite(command{
			Name:        "complete",
			Description: "End a session with its outcome; a session already ended stays as it is",
			Path:        "/sessions/:session-id/complete",
			Args:        []argSpec{sessionIDArg},
			Flags: func(fs *pflag.FlagSet) {
				fs.String("status", "completed", "the outcome: completed, failed or timeout")
			},
		}, []sessionFlag{
			{flag: "error", member: "error_message", usage: "what went wrong"},
			{flag: "duration-ms", member: "duration_ms", usage: "how long the session ran, in milliseconds (default: from its start to now)", count: true},
		}, func(c *call) (map[string]any, error) {
			status, _ := c.cmd.Flags().GetString("status")
			return map[string]any{"status": status}, nil
		}),
		declare(command{
			Name:        "get",
			Description: "Show a session with its token summary and the work linked to it",
			Method:      http.MethodGet,
			Path:        "/sessions/:session-id",
			Args:        []argSpec{sessionIDArg},
			Auth:        authUser,
		}),
		scmRead{
			command: command{
				Name:        "list",
				Description: "List the sessions you may read, newest first",
				Path:        "/sessions",
			},
			query: []queryFlag{
				{name: "workspace", param: "workspace", usage: "only the sessions of this workspace"},
				{name: "include-forks", param: "include_forks", kind: switchFlag, sends: "true", usage: "with --workspace, the sessions of its task forks too"},
				{name: "run-id", param: "run_id", usage: "only the sessions of this run"},
				{name: "status", param: "status", usage: "only the sessions in this status: active, completed, failed, timeout or terminated"},
				{name: "since", param: "since", usage: "only the sessions started at or after this RFC 3339 time"},
				limitFlag, cursorFlag,
			},
		}.build(),
	)
}

// startBody is the body of parley session start before its optional
// flags: the workspace and, when --metadata is given, the JSON document it
// holds, sent as that document and not as a string.
func startBody(c *call) (map[string]any, error) {
	body := map[string]any{"workspace_slug": c.args["workspace-slug"]}
	fs := c.cmd.Flags()
	if !fs.Changed("metadata") {
		return body, nil
	}
	metadata, _ := fs.GetString("metadata")
	if !json.Valid([]byte(metadata)) {
		return nil, usageError("%s takes --metadata as a JSON document, got %q", c.cmd.CommandPath(), metadata)
	}
	body["metadata"] = json.RawMessage(metadata)
	return body, nil
}
