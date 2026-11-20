package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// Where a tool call keeps what it needs in the sandbox: outside the working
// directory, so that neither is ever part of a checkpoint or a commit, and
// in a place the workload user may write. An index lost with /tmp is built
// again by the next search.
const (
	// toolRequestDir holds the request file of each running call.
	toolRequestDir = "/tmp/parley-tools/requests"
	// toolIndexDir is where the tool server keeps its search index between
	// its processes.
	toolIndexDir = "/tmp/parley-tools/index"
)

// The bounds of a tool call.
const (
	// errorTypeToolNotFound answers a call of a tool no set holds, with 404.
	errorTypeToolNotFound = "tool_not_found"
	// toolBodyLimit is the body limit of the routes that call a tool: an
	// input of maxToolInput fits in it with room for its envelope.
	toolBodyLimit = 8 << 20
	// maxToolInput is the most the input of a call has, as JSON.
	maxToolInput = 4 << 20
	// defaultToolTimeout is the time bound of a call over REST whose body
	// names none, as an exec's: a script can pass its own.
	defaultToolTimeout = time.Minute
	// maxToolStderr is how much of the tool server's error output is kept
	// for the message of a failed call.
	maxToolStderr = 64 << 10
	// removeTimeout bounds the removal of a request file, which runs after
	// the caller may have left.
	removeTimeout = 10 * time.Second
)

// The outcomes of a tool call that could not give a result.
const (
	toolErrorTimeout = "timeout"
)

// errToolBusy is the refusal of a call that found every place among the
// sandbox's commands taken. Each route answers it in its own form.
var errToolBusy = errors.New("the sandbox runs as many commands as it may at once")

// errToolAbandoned ends a call nobody waits for any more: its caller left
// or the hub is stopping. No answer is written for it.
var errToolAbandoned = errors.New("the tool call's caller left")

// toolRequest is the body of a call over REST once its members are judged.
type toolRequest struct {
	// input is the tool's input, a JSON object.
	input json.RawMessage
	// timeout is the call's time bound.
	timeout time.Duration
}

// toolResult is what a tool gave: the one JSON object the tool server
// printed for a call. A result with OK false is a result too: the tool
// failed, the call did not.
type toolResult struct {
	OK bool `json:"ok"`
	// Data is the tool's structured result, a JSON object; {} for none.
	Data json.RawMessage `json:"data"`
	// Error is the tool's error code and Detail what it says of it; both
	// "" for a success.
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Text is the text the tool means for a model, when it differs from the
	// data; nil for none.
	Text *string `json:"text,omitempty"`
}

// EventToolCall is written when a tool call that started in a sandbox ends.
// It names the tool, how the call ended and how long it took, never the
// call's arguments or its result, which may hold file contents and secrets.
const EventToolCall = "parley.sandbox.tool_call"

// The metrics of the tool calls.
const (
	// ToolCallsMetric counts the tool calls that started in a sandbox,
	// under the labels tool and outcome (a toolOutcome… value).
	ToolCallsMetric = "parley_sandbox_tool_calls_total"
	// ToolCallSecondsMetric observes how long each of them took, in
	// seconds, under the label tool.
	ToolCallSecondsMetric = "parley_sandbox_tool_call_duration_seconds"
)

// The outcomes of a tool call: the tool gave a result with ok true, one
// with ok false, the call outlasted its bound, or it gave no result.
const (
	toolOutcomeOK      = "ok"
	toolOutcomeError   = "tool_error"
	toolOutcomeTimeout = "timeout"
	toolOutcomeFailed  = "failed"
)

// The routes a tool call comes through, as its event names them.
const (
	viaREST = "rest"
	viaMCP  = "mcp"
)

// eventTimeout bounds the write of a call's event, which runs after the
// caller may have left.
const eventTimeout = 5 * time.Second

// toolCall is one call of one tool, through either route.
type toolCall struct {
	// via is viaREST or viaMCP, slug the workspace the request names and
	// caller who sent it.
	via, slug string
	caller    identity.Principal
	rec       Record
	// set is the caller's tool set and tool the tool's name.
	set, tool string
	// input is the tool's input, a JSON object.
	input json.RawMessage
	// bound is the call's time bound, at most [sandbox].max_exec_timeout.
	bound time.Duration
}

// toolCaller makes the tool calls of both routes. It counts each call in
// the executor's count of the sandbox's commands, so that execs and tool
// calls share [sandbox].max_concurrent_execs.
type toolCaller struct {
	s *Service
	x *executor
}

// validate reads the body of a call over REST and leaves it in u: nothing,
// or a JSON object whose input is a JSON object and whose timeout is a
// positive duration, which is cut to [sandbox].max_exec_timeout, not
// refused: a call answers when its tool ends, and a caller that asked for
// longer still gets the longest the hub gives.
func (c *toolCaller) validate(r *http.Request, u *useRequest) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return server.InvalidRequest("request body could not be read")
	}
	body, err := readBody(raw)
	if err != nil {
		return err
	}
	req := toolRequest{input: json.RawMessage(`{}`), timeout: defaultToolTimeout}
	if in, set := body["input"]; set {
		if in = bytes.TrimSpace(in); len(in) == 0 || in[0] != '{' {
			return server.InvalidRequest("input: must be a JSON object")
		}
		if len(in) > maxToolInput {
			return server.PayloadTooLarge(fmt.Sprintf("input: must have at most %d bytes as JSON", maxToolInput))
		}
		req.input = in
	}
	if raw, set := body["timeout"]; set {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return server.InvalidRequest("timeout: must be a string")
		}
		d, err := timefmt.ParseDuration(text)
		if err != nil || d <= 0 {
			return server.InvalidRequest("timeout: must be a whole number above 0 followed by s, m, h or d, such as 60s")
		}
		req.timeout = d
	}
	u.tool = req
	return nil
}

// bound is the time bound of a call that asks for d: d, cut to
// [sandbox].max_exec_timeout.
func (c *toolCaller) bound(d time.Duration) time.Duration {
	return min(d, c.s.settings.MaxExecTimeout)
}

// lookup finds the tool name for a caller with set on the ready sandbox
// rec. A tool the full set holds and the caller's set does not is refused
// 403 insufficient_scope, and one neither holds 404 tool_not_found, both
// before anything is written to or run in the sandbox for the call.
func (c *toolCaller) lookup(ctx context.Context, rec Record, set, name string) (toolDefinition, error) {
	defs, err := c.s.toolDefinitions(ctx, rec, set)
	if err != nil {
		return toolDefinition{}, err
	}
	if d, ok := findTool(defs, name); ok {
		return d, nil
	}
	if set != toolSetFull {
		full, err := c.s.toolDefinitions(ctx, rec, toolSetFull)
		if err != nil {
			return toolDefinition{}, err
		}
		if _, ok := findTool(full, name); ok {
			return toolDefinition{}, server.InsufficientScope(fmt.Sprintf("the tool %q changes the sandbox, which this credential may only read", name))
		}
	}
	return toolDefinition{}, server.NewError(errorTypeToolNotFound, http.StatusNotFound, fmt.Sprintf("the sandbox has no tool %q", name))
}

func findTool(defs []toolDefinition, name string) (toolDefinition, bool) {
	for _, d := range defs {
		if d.Name == name {
			return d, true
		}
	}
	return toolDefinition{}, false
}

// toolAnswer is the answer of a call over REST.
type toolAnswer struct {
	toolResult
	DurationMS int64 `json:"duration_ms"`
}

// serve serves POST …/tools/:name: it finds the tool in the caller's set,
// makes the call and answers 200 with the tool's result, whether the tool
// succeeded, failed or outlasted its bound. A call nobody waits for any
// more drops its connection, as an exec does.
func (c *toolCaller) serve(w http.ResponseWriter, r *http.Request, u useRequest) error {
	name := server.Param(r, "name")
	if _, err := c.lookup(r.Context(), u.rec, u.set, name); err != nil {
		return err
	}
	res, ms, err := c.call(r, toolCall{
		via: viaREST, slug: u.slug, caller: u.caller, rec: u.rec, set: u.set, tool: name,
		input: u.tool.input, bound: c.bound(u.tool.timeout),
	})
	switch {
	case errors.Is(err, errToolBusy):
		// No Retry-After: the limit is a number of commands, not a rate.
		return server.RateLimited("the sandbox runs as many commands as it may at once; try again once one has ended")
	case errors.Is(err, errToolAbandoned):
		panic(http.ErrAbortHandler)
	case err != nil:
		return err
	}
	server.WriteJSON(w, http.StatusOK, toolAnswer{toolResult: res, DurationMS: ms})
	return nil
}

// call makes one tool call in the sandbox: it takes a place among the
// sandbox's commands, writes the request to a file of the sandbox, since
// the runtime's exec carries no input, runs the tool server once on it, and
// removes the file. It returns the tool's result and how long the call
// took on the hub's clock, or why there is no result: errToolBusy when no
// place was left, errToolAbandoned when nobody waits any more, the
// tool-server-failed answer, or the runtime's error.
func (c *toolCaller) call(r *http.Request, tc toolCall) (res toolResult, ms int64, err error) {
	s, id := c.s, tc.rec.ID
	if !c.x.take(id) {
		return toolResult{}, 0, errToolBusy
	}
	ctx, cancel := context.WithCancel(r.Context())
	started := s.deps.Now()
	e := &execution{
		started: started, caller: r.Context(), cancel: cancel,
		timer: s.newTimer(tc.bound), ended: make(chan struct{}),
		release: func() { c.x.release(id) },
	}
	defer e.end()
	go e.watch(s.deps.Shutdown)
	s.touch(ctx, id, started)
	defer func() {
		now := s.deps.Now()
		ms = now.Sub(started).Milliseconds()
		s.touch(r.Context(), id, now)
		c.record(r.Context(), tc, res, err, e.timedOut.Load(), now.Sub(started))
	}()

	file, err := newToolRequestFile()
	if err != nil {
		return toolResult{}, 0, err
	}
	request, err := json.Marshal(struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}{tc.tool, tc.input})
	if err != nil {
		return toolResult{}, 0, err
	}
	// The file is removed whether it was written whole, in part or not at
	// all, and whatever became of the call.
	defer c.remove(r.Context(), id, file)
	// cut is what a call that ended before its tool server answered gives:
	// nothing when nobody waits any more, the timeout's result when its
	// bound passed, and otherwise err, the runtime's failure.
	timedOut := toolResult{Error: toolErrorTimeout, Detail: "the call did not end within " + formatDuration(tc.bound), Data: json.RawMessage(`{}`)}
	cut := func(err error) (toolResult, int64, error) {
		switch {
		case e.abandoned():
			return toolResult{}, 0, errToolAbandoned
		case e.timedOut.Load():
			return timedOut, 0, nil
		}
		return toolResult{}, 0, err
	}
	if err := s.Client().Upload(ctx, id, file, 0o600, bytes.NewReader(request)); err != nil {
		return cut(err)
	}
	command := []string{
		tc.rec.Snapshot.ToolServer, "call", "--request", file, "--dir", WorkDir, "--index-dir", toolIndexDir,
		"--timeout", strconv.FormatInt(int64(tc.bound/time.Second), 10) + "s",
	}
	if tc.set == toolSetReadOnly {
		command = append(command, "--read-only")
	}
	// The runtime holds the sandbox's environment, so none is sent; the
	// working directory is always named.
	stream, err := s.Client().ExecStream(ctx, id, runtime.ExecRequest{Command: command, WorkDir: WorkDir})
	if err != nil {
		return cut(err)
	}
	e.stream = stream
	var stdout, stderr []byte
	for {
		o, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return cut(err)
		}
		switch o.Stream {
		case runtime.Stdout:
			// One byte over the limit is kept, to tell an output of the
			// limit from a longer one.
			stdout = appendUpTo(stdout, o.Data, maxToolOutput+1)
		case runtime.Stderr:
			stderr = appendUpTo(stderr, o.Data, maxToolStderr)
		}
	}
	// The timeout is asked before the exit code: a runtime may report the
	// code of the process it killed as an exit.
	code, err := stream.ExitCode()
	if err != nil || e.abandoned() || e.timedOut.Load() {
		return cut(err)
	}
	res, problem := readToolResult(code, stdout, stderr)
	if problem != "" {
		return toolResult{}, 0, s.toolServerFailed(r.Context(), id, "the tool server did not answer the call of "+tc.tool+": "+problem)
	}
	return res, 0, nil
}

// record writes the event of a call that started and has ended, and counts
// it in the two metrics. res and err are what the call returned, timedOut
// whether its bound passed, and took how long it ran. The event's metadata
// is built from six values the hub decides, so that nothing of the call's
// arguments or result can reach it. A failure to write the event is logged
// and changes nothing about the call.
func (c *toolCaller) record(ctx context.Context, tc toolCall, res toolResult, err error, timedOut bool, took time.Duration) {
	s := c.s
	meta := map[string]any{"tool": tc.tool, "ok": false, "duration_ms": took.Milliseconds(), "via": tc.via, "set": tc.set}
	outcome := toolOutcomeFailed
	var answer *server.Error
	switch {
	case err == nil && res.OK:
		meta["ok"], outcome = true, toolOutcomeOK
	case err == nil && timedOut:
		meta["error"], outcome = toolErrorTimeout, toolOutcomeTimeout
	case err == nil:
		meta["error"], outcome = res.Error, toolOutcomeError
	case errors.As(err, &answer) || errors.Is(err, errToolAbandoned):
		// A call nobody waits for any more gave no result either.
		meta["error"] = errorTypeToolServerFailed
	default:
		meta["error"] = errorTypeRuntime
	}
	_ = s.deps.Recorder.Add(ToolCallsMetric, map[string]string{"tool": tc.tool, "outcome": outcome}, 1)
	_ = s.deps.Recorder.Observe(ToolCallSecondsMetric, map[string]string{"tool": tc.tool}, took.Seconds())

	in := event.Input{
		Type: EventToolCall, Workspace: &tc.rec.Workspace, ActorType: actorOf(tc.caller),
		ResourceType: "sandbox", ResourceID: tc.rec.ID, SessionID: tc.rec.SessionID, Metadata: meta,
	}
	if tc.caller.User != nil {
		in.ActorID = &tc.caller.User.ID
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventTimeout)
	defer cancel()
	if _, err := s.deps.Events.EmitNow(ctx, in); err != nil {
		s.logger().Error("cannot write a tool call's event", "sandbox_id", tc.rec.ID, "tool", tc.tool, "error", s.scrub(err.Error()))
	}
}

// remove removes the request file of a call from the sandbox id. The
// caller may have left, so it runs under a context of its own. A failure
// is logged and changes nothing about the call.
func (c *toolCaller) remove(ctx context.Context, id, file string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	res, err := c.s.Client().Exec(ctx, id, runtime.ExecRequest{Command: []string{"rm", "-f", file}, WorkDir: WorkDir})
	var reason string
	switch {
	case err != nil:
		reason = err.Error()
	case res.ExitCode != 0:
		reason = fmt.Sprintf("rm exited with status %d: %s", res.ExitCode, bytes.TrimSpace(res.Stderr))
	default:
		return
	}
	c.s.logger().Warn("cannot remove a tool call's request file", "sandbox_id", id, "file", file,
		"error", c.s.scrub(c.s.ScrubSandbox(ctx, id, reason)))
}

// newToolRequestFile returns the path of a new request file: a name of 32
// lowercase hexadecimal characters from a cryptographic random source, so
// that no process in the sandbox can know it before it is written.
func newToolRequestFile() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return toolRequestDir + "/" + hex.EncodeToString(b[:]) + ".json", nil
}

// appendUpTo appends to dst as much of b as keeps it within limit bytes.
func appendUpTo(dst, b []byte, limit int) []byte {
	if room := limit - len(dst); len(b) > room {
		b = b[:max(room, 0)]
	}
	return append(dst, b...)
}

// readToolResult reads what the tool server printed for a call that ended
// with the exit status code: one JSON object with a boolean ok. problem
// says why the output is not a result, "" when it is.
func readToolResult(code int, stdout, stderr []byte) (res toolResult, problem string) {
	if code != 0 {
		return res, fmt.Sprintf("it exited with status %d: %s", code, bytes.TrimSpace(stderr))
	}
	if len(stdout) > maxToolOutput {
		return res, fmt.Sprintf("it printed more than %d bytes", maxToolOutput)
	}
	var members map[string]json.RawMessage
	if err := decodeOne(stdout, '{', &members); err != nil {
		return res, "it printed something that is not one JSON object"
	}
	if json.Unmarshal(members["ok"], &res.OK) != nil || len(members["ok"]) == 0 {
		return res, "its result has no boolean ok"
	}
	res.Data = json.RawMessage(`{}`)
	if data := nullAsAbsent(members["data"]); data != nil {
		if data[0] != '{' {
			return res, "the data of its result is not an object"
		}
		res.Data = data
	}
	for name, into := range map[string]*string{"error": &res.Error, "detail": &res.Detail} {
		if raw := nullAsAbsent(members[name]); raw != nil && json.Unmarshal(raw, into) != nil {
			return res, "the " + name + " of its result is not a string"
		}
	}
	if raw := nullAsAbsent(members["text"]); raw != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return res, "the text of its result is not a string"
		}
		res.Text = &text
	}
	return res, ""
}
