package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// The errors of a tool call that are not a command's own.
const (
	// toolTimeoutError is the error of a call that outlasted the bound of
	// every tool call.
	toolTimeoutError = "timeout"
	// toolCancelledError is the error of a call that was cancelled from
	// outside before it ended.
	toolCancelledError = "cancelled"
	// toolExitError is the error of a command that ended with an exit status
	// of its own and no envelope, as parley sandbox exec does for the
	// command it ran.
	toolExitError = "command_exit"
)

// toolResult is what a tool call gives: the tool's data, or why it failed.
// A failed call is a result too, never a protocol error, so that an agent
// reads why and goes on.
type toolResult struct {
	OK bool
	// Data is the tool's structured result; never nil.
	Data map[string]any
	// Error is the error's type and Detail its message; both "" for a
	// success.
	Error, Detail string
	// Text is the text the tool means for a model, when it gave one.
	Text string
}

// failedResult is a failed result with the error code and its detail.
func failedResult(code, format string, args ...any) toolResult {
	return toolResult{Data: map[string]any{}, Error: code, Detail: fmt.Sprintf(format, args...)}
}

// toolSession is the tools of one run of parley mcp and what a call of one
// runs with: the hub and the credential, read once when the session starts.
type toolSession struct {
	opts options
	st   settings
	// settings are the flags the session was started with.
	settings mcpSettings
	tools    []commandTool
	// sandboxTools are the tools of the session's sandbox, read from the
	// hub when the session starts; none without --sandbox.
	sandboxTools []sandboxTool
	// codeMode says whether code_mode is served, and scriptTimeout bounds
	// one of its scripts' wall time.
	codeMode      bool
	scriptTimeout time.Duration
}

// The bounds of --script-timeout, and its default: longer than code mode's
// own 30 seconds, because one change_land with wait can take minutes.
const (
	defaultScriptTimeout = 10 * time.Minute
	minScriptTimeout     = time.Second
	maxScriptTimeout     = 30 * time.Minute
)

// readScriptTimeout reads --script-timeout: a duration from 1 second to 30
// minutes, 10 minutes when it is not given.
func readScriptTimeout(value string) (time.Duration, error) {
	if value == "" {
		return defaultScriptTimeout, nil
	}
	d, err := parseDuration(value)
	if err != nil || d < minScriptTimeout || d > maxScriptTimeout {
		return 0, usageError("--script-timeout must be a duration from 1s to 30m, such as 90s; got %q", value)
	}
	return d, nil
}

// newToolSession builds the session of the settings s over the command tree
// of opts. The hub and the credential are set once they are resolved, which
// listing the tools does not need.
func newToolSession(opts options, s mcpSettings) (*toolSession, error) {
	if s.sandbox != "" && s.workspace == "" {
		return nil, usageError("--sandbox names a sandbox of a workspace: give the workspace with --workspace")
	}
	tools, err := commandTools(newRootCommand(opts), s)
	if err != nil {
		return nil, err
	}
	if len(tools) > maxMCPTools {
		return nil, usageError("the selection has %d tools, more than the %d one session serves; narrow --groups", len(tools), maxMCPTools)
	}
	scriptTimeout, err := readScriptTimeout(s.scriptTimeout)
	if err != nil {
		return nil, err
	}
	return &toolSession{opts: opts, settings: s, tools: tools, codeMode: !s.noCodeMode, scriptTimeout: scriptTimeout}, nil
}

// noStdin is the standard input of a command a tool call runs. The
// process's own carries the session, so a command that would read it is
// refused: read notes that it tried.
type noStdin struct{ read bool }

var errNoStdin = errors.New("a tool has no standard input")

func (n *noStdin) Read([]byte) (int, error) {
	n.read = true
	return 0, errNoStdin
}

// callCommand runs the command of t in this process, as the shell runs it:
// its own run step, with the call's parameters as its arguments and flags,
// under a context that ends after the bound of a tool call. What the
// command prints is the result's data, and the error it ends with a failed
// result.
func (s *toolSession) callCommand(ctx context.Context, t commandTool, input json.RawMessage) toolResult {
	args, err := t.commandLine(input)
	if err != nil {
		return failedResult(usageErrorType, "%v", err)
	}
	args = append([]string{"--endpoint-url=" + s.st.hubURL, "--api-key=" + s.st.credential}, args...)

	opts := s.opts
	stdin := &noStdin{}
	opts.stdin = stdin
	outer := ctx
	ctx, cancel := context.WithTimeout(ctx, s.opts.toolCallBound)
	defer cancel()
	// The command's progress lines and warnings are not part of a result.
	var stdout bytes.Buffer
	runErr := execute(ctx, args, &stdout, io.Discard, opts)

	res := toolResult{Data: printedData(stdout.Bytes())}
	switch {
	case stdin.read:
		return failedResult(usageErrorType, "%s was asked to read standard input, with a value of - or a flag that says so; a tool has none, so give the value itself", t.name)
	case runErr == nil:
		res.OK = true
	case outer.Err() != nil:
		// Not the call's own bound: the session ended, or the script the
		// call was made from ran out of time.
		return failedResult(toolCancelledError, "the call was cancelled before it ended")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failedResult(toolTimeoutError, "the call did not end within %s", s.opts.toolCallBound)
	default:
		res.Error, res.Detail = errorOf(runErr)
	}
	return res
}

// printedData is the data of what a command printed: a JSON object as it
// is, any other JSON value as the member result, and nothing as {}. Output
// that is not one JSON document, such as the lines of a log, is the member
// result as text.
func printedData(out []byte) map[string]any {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal(out, &value); err != nil {
		return map[string]any{"result": string(out)}
	}
	if object, isObject := value.(map[string]any); isObject {
		return object
	}
	return map[string]any{"result": value}
}

// errorOf is the error type and the message of the error a command ended
// with: the hub's envelope's, http_<status> for an answer of the hub
// without one, or the CLI's own.
func errorOf(err error) (code, detail string) {
	var exit *commandExit
	if errors.As(err, &exit) {
		return toolExitError, exit.Error()
	}
	var he *hubError
	if errors.As(err, &he) {
		var env cliEnvelope
		if json.Unmarshal(he.body, &env) == nil && env.Error.ErrorType != "" {
			return env.Error.ErrorType, env.Error.Message
		}
		return "http_" + strconv.Itoa(he.status), he.Error()
	}
	var ce *cliError
	if errors.As(err, &ce) {
		if ce.status != 0 {
			return "http_" + strconv.Itoa(ce.status), ce.message
		}
		return ce.errorType, ce.message
	}
	return clientErrorType, err.Error()
}

// commandLine is the command's line for the JSON parameters input: its
// words, --json, each flag given as the shell takes it, and the arguments in
// their declared order. A command that takes words after -- gets them
// there, and its arguments before; any other command gets its arguments
// after --, so that one that begins with a dash stays an argument. A
// parameter the tool does not have, one of the wrong type and a missing
// required one are errors that name it.
func (t commandTool) commandLine(input json.RawMessage) ([]string, error) {
	given := map[string]json.RawMessage{}
	if trimmed := bytes.TrimSpace(input); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if trimmed[0] != '{' || json.Unmarshal(trimmed, &given) != nil {
			return nil, fmt.Errorf("the parameters of %s must be a JSON object", t.name)
		}
	}
	known := map[string]bool{}
	// A tool's result is the command's JSON document: a command that prints
	// something else without --json, as sandbox exec does, prints it with.
	line := append(append([]string{}, t.words...), "--json")
	var positional, trailing []string
	takesTrailing, skipped := false, ""
	for _, p := range t.params {
		known[p.name] = true
		takesTrailing = takesTrailing || p.trailing
		raw, has := given[p.name]
		if has && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			has = false
		}
		if !has {
			switch {
			case p.fallback != "":
				positional = append(positional, p.fallback)
			case p.required:
				return nil, fmt.Errorf("%s needs the parameter %s", t.name, p.name)
			case p.isArg && !p.trailing:
				skipped = p.name
			}
			continue
		}
		values, err := p.values(raw)
		switch {
		case err != nil:
			return nil, fmt.Errorf("the parameter %s of %s %v", p.name, t.name, err)
		case p.isArg && p.required && len(values) == 0:
			return nil, fmt.Errorf("the parameter %s of %s needs at least one element", p.name, t.name)
		case p.trailing:
			trailing = values
		case !p.isArg && p.kind == "boolean" && values[0] == "false":
			// As on a shell's command line, where a flag that is not
			// wanted is not written: some commands tell a flag given as
			// false from one not given.
		case !p.isArg:
			for _, v := range values {
				line = append(line, "--"+p.source+"="+v)
			}
		case skipped != "":
			// The arguments are positional: one cannot be given after one
			// that was left out.
			return nil, fmt.Errorf("%s takes the parameter %s only together with %s", t.name, p.name, skipped)
		default:
			positional = append(positional, values...)
		}
	}
	for name := range given {
		if !known[name] {
			return nil, fmt.Errorf("%s has no parameter %s", t.name, name)
		}
	}
	if !takesTrailing {
		return append(append(line, "--"), positional...), nil
	}
	// Here -- belongs to the command, so an argument before it cannot be
	// kept from being read as a flag.
	for _, v := range positional {
		if strings.HasPrefix(v, "-") {
			return nil, fmt.Errorf("an argument of %s cannot begin with a dash: %q", t.name, v)
		}
	}
	line = append(line, positional...)
	if trailing != nil {
		line = append(append(line, "--"), trailing...)
	}
	return line, nil
}

// values are the words the JSON value raw of the parameter gives its flag
// or argument: one, or one per element of an array.
func (p toolParam) values(raw json.RawMessage) ([]string, error) {
	switch p.kind {
	case "boolean":
		var v bool
		if json.Unmarshal(raw, &v) != nil {
			return nil, errors.New("must be true or false")
		}
		return []string{strconv.FormatBool(v)}, nil
	case "integer":
		var v json.Number
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return nil, errors.New("must be an integer")
		}
		if n, err := v.Int64(); err == nil {
			return []string{strconv.FormatInt(n, 10)}, nil
		}
		// 3.0 is an integer too, as some clients write one.
		if f, err := v.Float64(); err == nil && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return []string{strconv.FormatInt(int64(f), 10)}, nil
		}
		return nil, errors.New("must be an integer")
	case "array":
		var v []string
		if json.Unmarshal(raw, &v) != nil || bytes.TrimSpace(raw)[0] != '[' {
			return nil, errors.New("must be an array of strings")
		}
		return v, nil
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return nil, errors.New("must be a string")
	}
	if strings.ContainsRune(v, 0) {
		return nil, errors.New("must not hold a NUL character")
	}
	return []string{v}, nil
}
