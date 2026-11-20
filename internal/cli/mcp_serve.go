package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/agent-fox-dev/agentkit-go/core"
	"github.com/agent-fox-dev/agentkit-go/mcpserve"
	"github.com/agent-fox-dev/agentkit-go/runner"
	"github.com/agent-fox-dev/agentkit-go/schema"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

// mcpServerName is the name the MCP server of parley mcp gives itself.
const mcpServerName = "parley"

// mcpInstructions are the instructions a harness shows its model: what the
// tools are, the workspace and the sandbox the session is fixed on, and
// that code_mode chains them. At most three sentences, since they are sent
// with every session.
func mcpInstructions(s mcpSettings) string {
	text := "These tools are the commands of the parley CLI, run against the Parley hub with this session's credential."
	switch {
	case s.workspace != "" && s.sandbox != "":
		text += fmt.Sprintf(" A tool that takes a workspace works on %s when given none, and the file, search and shell tools work in its sandbox %s.", s.workspace, s.sandbox)
	case s.workspace != "":
		text += fmt.Sprintf(" A tool that takes a workspace works on %s when given none.", s.workspace)
	}
	if !s.noCodeMode {
		text += " Use code_mode to chain several tool calls in one script."
	}
	return text
}

// coreTools are the session's tools as agentkit-go runs them: each with its
// input schema and the call of its command as its handler.
func (s *toolSession) coreTools() ([]core.Tool, error) {
	tools := make([]core.Tool, 0, len(s.tools))
	for _, t := range s.tools {
		input, err := schema.Parse(t.schema())
		if err != nil {
			return nil, clientError("the input schema of the tool %s: %v", t.name, err)
		}
		tools = append(tools, core.Tool{
			Name:        t.name,
			Description: t.description,
			InputSchema: input,
			Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
				return coreResult(s.callCommand(ctx, t, in))
			},
		})
	}
	return tools, nil
}

// coreResult is r as agentkit-go carries a tool's result. A failed result
// keeps the data the command printed, such as the record a wait ended on.
func coreResult(r toolResult) core.ToolResult {
	if r.OK {
		res := core.OKResult(r.Data)
		res.Text = r.Text
		return res
	}
	res := core.ErrResult(r.Error, r.Detail)
	if len(r.Data) > 0 {
		res.Data = r.Data
	}
	return res
}

// notRunError is the error a call's line on stderr names when agentkit-go
// refused the call before the tool ran: an unknown tool, or parameters its
// schema does not allow.
const notRunError = "not_run"

// callLog writes one line to stderr for every tool call that ends, a direct
// one and one a script makes: the tool, the outcome and the duration, never
// the arguments or the result, which may hold what the agent works on.
type callLog struct {
	mu     sync.Mutex
	stderr io.Writer
	// errors holds, by the id of the call, the error of each failed call
	// whose tool ran, from when its result is known until its line is
	// written.
	errors map[string]string
}

// after notes the error of a failed call. The runner calls it for every
// call whose tool ran, before the call's end event.
func (l *callLog) after(_ context.Context, in core.AfterToolCallContext) core.AfterToolCallDecision {
	if !in.ToolResult.OK {
		l.mu.Lock()
		l.errors[in.ToolUseID] = in.ToolResult.Error
		l.mu.Unlock()
	}
	return core.AfterToolCallDecision{}
}

// event writes the line of a call when its end event comes, which the
// runner emits for every call, one it refused before the tool ran included.
func (l *callLog) event(e core.Event) {
	end, isEnd := e.(core.ToolExecutionEndEvent)
	if !isEnd {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	line := struct {
		Tool       string `json:"tool"`
		OK         bool   `json:"ok"`
		Error      string `json:"error,omitempty"`
		DurationMS int64  `json:"duration_ms"`
	}{Tool: end.Name, OK: !end.IsError, DurationMS: end.ElapsedMS}
	if end.IsError {
		line.Error = notRunError
		if code, ran := l.errors[end.ToolUseID]; ran {
			line.Error = code
		}
	}
	delete(l.errors, end.ToolUseID)
	// Strings, a boolean and a number: this cannot fail.
	out, _ := json.Marshal(line)
	_, _ = l.stderr.Write(append(out, '\n'))
}

// runner is the runner of the session's tools, which logs every call it
// runs to stderr (see callLog).
func (s *toolSession) runner(stderr io.Writer) (*runner.Runner, error) {
	tools, err := s.coreTools()
	if err != nil {
		return nil, err
	}
	log := &callLog{stderr: stderr, errors: map[string]string{}}
	r, err := runner.New(runner.Toolset{Tools: tools, After: log.after, Events: log.event})
	if err != nil {
		return nil, clientError("cannot serve the tools: %v", err)
	}
	return r, nil
}

// serve serves the session's tools in one MCP session on the process's
// standard input and output, until the client closes its end or the process
// is told to stop: SIGINT and SIGTERM end the session and, through the
// context every call runs under, the calls that run. Only the protocol is
// written to standard output.
func (s *toolSession) serve(ctx context.Context, stderr io.Writer, settings mcpSettings) error {
	r, err := s.runner(stderr)
	if err != nil {
		return err
	}
	server, err := mcpserve.New(r, mcpserve.Options{
		Name:         mcpServerName,
		Version:      buildinfo.Get().Version,
		Instructions: mcpInstructions(settings),
	})
	if err != nil {
		return clientError("cannot serve the tools over MCP: %v", err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.ServeStdio(ctx); err != nil && ctx.Err() == nil {
		return clientError("the MCP session ended: %v", err)
	}
	return nil
}
