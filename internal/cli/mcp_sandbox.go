package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/agentkit-go/core"
	"github.com/agent-fox-dev/agentkit-go/schema"
)

// codeModeName is the name of the code mode tool.
const codeModeName = "code_mode"

// sandboxChangingTools are the tools of a sandbox's full set that its
// read-only set lacks. The hub lists only the caller's own set, so with
// --read-only a caller of the full set leaves these out by name.
var sandboxChangingTools = []string{"write_file", "edit_file", "execute", "run_command"}

// sandboxTool is one tool of the session's sandbox, as the hub lists it.
type sandboxTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// sandboxToolsPath is the hub's route of the tools of the session's
// sandbox.
func (s *toolSession) sandboxToolsPath() string {
	return "/workspaces/" + pathSegment(s.settings.workspace) + "/sandboxes/" + pathSegment(s.settings.sandbox) + "/tools"
}

// hubClient is a client of the session's hub with its credential, whose
// requests may take as long as timeout.
func (s *toolSession) hubClient(timeout time.Duration) *client {
	cl := newClient(s.st.hubURL, s.st.credential, timeout)
	if s.opts.transport != nil {
		cl.http.Transport = s.opts.transport(cl.http.Transport)
	}
	return cl
}

// loadSandboxTools reads the tools of the session's sandbox from the hub,
// once: the set the hub gives the credential, without the tools that change
// the sandbox when the session is read-only. Without a sandbox it does
// nothing. The hub's refusal (a sandbox without tools, not ready, unknown or
// not accessible) is returned as it is, and a tool with the name of a
// command tool is an error naming both.
func (s *toolSession) loadSandboxTools(ctx context.Context) error {
	if s.settings.sandbox == "" {
		return nil
	}
	resp, err := s.hubClient(s.opts.requestTimeout).request(ctx, http.MethodGet, s.sandboxToolsPath(), nil, nil)
	if err != nil {
		return err
	}
	var listed struct {
		Tools []sandboxTool `json:"tools"`
	}
	if err := json.Unmarshal(resp.body, &listed); err != nil {
		return clientError("the hub's list of the sandbox's tools is not the list expected: %v", err)
	}
	for _, t := range listed.Tools {
		if s.settings.readOnly && slices.Contains(sandboxChangingTools, t.Name) {
			continue
		}
		if t.Name == codeModeName && s.codeMode {
			// The tool server has a code mode of its own, bound to its
			// tools alone. The session's is bound to those and to the
			// commands, so it takes the name and the other is left out.
			continue
		}
		if i := slices.IndexFunc(s.tools, func(ct commandTool) bool { return ct.name == t.Name }); i >= 0 {
			return clientError("the sandbox's tool %s has the name of the tool of the command %q", t.Name, strings.Join(s.tools[i].words, " "))
		}
		s.sandboxTools = append(s.sandboxTools, t)
	}
	if n := len(s.tools) + len(s.sandboxTools); n > maxMCPTools {
		return usageError("the selection and the sandbox have %d tools, more than the %d one session serves; narrow --groups", n, maxMCPTools)
	}
	return nil
}

// sandboxCoreTools are the sandbox's tools as agentkit-go runs them: each
// under its own name, description and input schema, with the hub's call
// route as its handler.
func (s *toolSession) sandboxCoreTools() ([]core.Tool, error) {
	tools := make([]core.Tool, 0, len(s.sandboxTools))
	for _, t := range s.sandboxTools {
		input, err := schema.Parse(t.InputSchema)
		if err != nil {
			return nil, clientError("the input schema of the sandbox's tool %s: %v", t.Name, err)
		}
		tools = append(tools, core.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: input,
			Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
				return coreResult(s.callSandboxTool(ctx, t.Name, in))
			},
		})
	}
	return tools, nil
}

// callSandboxTool makes one call of the sandbox's tool name through the
// hub, with the JSON parameters input, and gives the hub's result as it is.
// The hub runs the tool in the sandbox and bounds it; an error the hub
// answers instead of a result is a failed result with its error type.
func (s *toolSession) callSandboxTool(ctx context.Context, name string, input json.RawMessage) toolResult {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	ctx, cancel := context.WithTimeout(ctx, s.opts.toolCallBound)
	defer cancel()
	body := struct {
		Input json.RawMessage `json:"input"`
	}{input}
	resp, err := s.hubClient(s.opts.toolCallBound).request(ctx, http.MethodPost, s.sandboxToolsPath()+"/"+pathSegment(name), nil, body)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return failedResult(toolTimeoutError, "the call did not end within %s", s.opts.toolCallBound)
		}
		code, detail := errorOf(err)
		return failedResult(code, "%s", detail)
	}
	var answer struct {
		OK     bool           `json:"ok"`
		Data   map[string]any `json:"data"`
		Error  string         `json:"error"`
		Detail string         `json:"detail"`
		Text   string         `json:"text"`
	}
	if err := json.Unmarshal(resp.body, &answer); err != nil {
		return failedResult(clientErrorType, "the hub's answer to the call of %s is not a tool result: %v", name, err)
	}
	if answer.Data == nil {
		answer.Data = map[string]any{}
	}
	return toolResult{OK: answer.OK, Data: answer.Data, Error: answer.Error, Detail: answer.Detail, Text: answer.Text}
}
