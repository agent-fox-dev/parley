package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
	"github.com/agent-fox-dev/parley/internal/server"
)

const (
	// mcpServerName is the name the MCP server of a sandbox gives itself.
	mcpServerName = "parley"
	// mcpToolTimeout is the time bound of a call over MCP, whose client
	// cannot name one: longer than a call's over REST, where a script can.
	mcpToolTimeout = 10 * time.Minute
	// codeSandboxBusy is the JSON-RPC error code of a call that found every
	// place among the sandbox's commands taken, from the range the protocol
	// leaves to a server.
	codeSandboxBusy = -32000
)

// mcpMethods are the requests the MCP server of a sandbox answers. It
// serves tools and nothing else: no resources, prompts, completions or
// logging.
var mcpMethods = []string{"initialize", "ping", "tools/list", "tools/call"}

// serveMCP serves POST …/mcp: an MCP server over streamable HTTP for the one
// sandbox of u. It is stateless. The server is built for this request, from
// the caller's tool set, and answers the one message the request holds as
// JSON; no session id is set or read, so a restart of the hub between two
// requests is not seen by the client. What the gate decided before this is
// answered as plain HTTP with the hub's error object, and so is a tool
// server whose definitions cannot be read or served.
//
// The protocol is the SDK's. Its protection against DNS rebinding, which
// refuses a request that reaches a loopback address under another host
// name, is off: every request here carries a bearer credential, which a
// page in a browser does not have, and a hub behind a reverse proxy on the
// same host is reached exactly that way.
func (c *toolCaller) serveMCP(w http.ResponseWriter, r *http.Request, u useRequest) error {
	defs, err := c.s.toolDefinitions(r.Context(), u.rec, u.set)
	if err != nil {
		return err
	}
	if unservedMCPMessage(w, r) {
		return nil
	}
	// out drops what the SDK writes for a call nobody waits for any more.
	out := &abandonableWriter{ResponseWriter: w}
	srv := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: buildinfo.Get().Version}, &mcp.ServerOptions{
		Instructions: fmt.Sprintf("These tools work in the working directory of the sandbox %s of the Parley workspace %s.", u.rec.ID, u.slug),
		// Stated, so that the SDK adds neither its default logging
		// capability nor a tool list that changes.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	listed := make([]*mcp.Tool, 0, len(defs))
	for _, d := range defs {
		tool := &mcp.Tool{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
		if d.hasOutputSchema() {
			tool.OutputSchema = d.OutputSchema
		}
		if reason := addMCPTool(srv, tool, c.mcpHandler(r, u, d, out)); reason != "" {
			return c.s.toolServerFailed(r.Context(), u.rec.ID, fmt.Sprintf("the tool %q cannot be served over MCP: %s", d.Name, reason))
		}
		listed = append(listed, tool)
	}
	srv.AddReceivingMiddleware(listInOrder(listed))
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// The route's own limit has been applied to the body already.
		MaxRequestBodyBytes:        -1,
		DisableLocalhostProtection: true,
	})
	h.ServeHTTP(out, r)
	if out.abandoned.Load() {
		// As a call over REST does: the connection is dropped.
		panic(http.ErrAbortHandler)
	}
	return nil
}

// addMCPTool adds tool to srv. The SDK panics on a tool it does not take:
// an input schema that is not of the type object, a schema nested too deep,
// an annotation it refuses. The schemas are the tool server's, so a panic
// is its failure and not the hub's: reason says what the SDK refused, ""
// when it took the tool.
func addMCPTool(srv *mcp.Server, tool *mcp.Tool, h mcp.ToolHandler) (reason string) {
	defer func() {
		if p := recover(); p != nil {
			reason = fmt.Sprint(p)
		}
	}()
	srv.AddTool(tool, h)
	return ""
}

// abandonableWriter is the writer the SDK answers through. Once a call is
// abandoned it drops what is written, so that a call nobody waits for any
// more gets no answer, whatever the SDK makes of the handler's error.
type abandonableWriter struct {
	http.ResponseWriter
	abandoned atomic.Bool
}

func (w *abandonableWriter) WriteHeader(code int) {
	if !w.abandoned.Load() {
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *abandonableWriter) Write(b []byte) (int, error) {
	if w.abandoned.Load() {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap gives http.ResponseController the gate's writer.
func (w *abandonableWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// unservedMCPMessage answers the request r itself, and reports true, when
// its body is a JSON-RPC request for a method other than mcpMethods, with
// the protocol's method-not-found error, or a batch, with its
// invalid-request error. The SDK knows more methods than a sandbox serves,
// answers one it does not know outside JSON-RPC, and takes a batch, in which
// such a method would pass. Notifications, which a client sends after
// initialize, and everything that is not one request are left to the SDK.
func unservedMCPMessage(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false
	}
	refuse := func(id json.RawMessage, code int64, message string) bool {
		server.WriteJSON(w, http.StatusOK, struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   jsonrpc.Error   `json:"error"`
		}{"2.0", id, jsonrpc.Error{Code: code, Message: message}})
		return true
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		return refuse(json.RawMessage("null"), jsonrpc.CodeInvalidRequest, "batches are not served")
	}
	// A map keeps the members apart by their exact names, as the SDK reads
	// them; a struct would take Method for method.
	var msg map[string]json.RawMessage
	if json.Unmarshal(body, &msg) != nil {
		return false
	}
	var method string
	id := msg["id"]
	if json.Unmarshal(msg["method"], &method) != nil || len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return false
	}
	if slices.Contains(mcpMethods, method) || strings.HasPrefix(method, "notifications/") {
		return false
	}
	return refuse(id, jsonrpc.CodeMethodNotFound, fmt.Sprintf("method %q is not served", method))
}

// listInOrder answers tools/list with tools as they are, in the tool
// server's order and on one page: the SDK would list them by name.
func listInOrder(tools []*mcp.Tool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				return &mcp.ListToolsResult{Tools: tools}, nil
			}
			return next(ctx, method, req)
		}
	}
}

// mcpHandler handles the tools/call of the tool d for the request r: it
// makes the call as the REST route does and gives the tool's result as an
// MCP tool result. What is not a tool result is a JSON-RPC error, and a call
// nobody waits for any more makes out drop the answer.
func (c *toolCaller) mcpHandler(r *http.Request, u useRequest, d toolDefinition, out *abandonableWriter) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input := json.RawMessage(bytes.TrimSpace(req.Params.Arguments))
		switch {
		case len(input) == 0 || bytes.Equal(input, []byte("null")):
			input = json.RawMessage(`{}`)
		case input[0] != '{':
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("the arguments of %s must be a JSON object", d.Name)}
		case len(input) > maxToolInput:
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("the arguments of %s must have at most %d bytes as JSON", d.Name, maxToolInput)}
		}
		// The request's context, not the handler's, says whether the caller
		// is still there: the SDK does not tie the two for every client.
		res, _, err := c.call(r, toolCall{
			via: viaMCP, caller: u.caller, rec: u.rec, set: u.set, tool: d.Name,
			input: input, bound: c.bound(mcpToolTimeout),
		})
		switch {
		case errors.Is(err, errToolBusy):
			return nil, &jsonrpc.Error{Code: codeSandboxBusy, Message: "sandbox busy"}
		case errors.Is(err, errToolAbandoned):
			out.abandoned.Store(true)
			return nil, err
		case err != nil:
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: c.failureMessage(r.Context(), u.rec.ID, err)}
		}
		out := &mcp.CallToolResult{IsError: !res.OK, Content: []mcp.Content{&mcp.TextContent{Text: toolText(res)}}}
		if d.hasOutputSchema() {
			out.StructuredContent = res.Data
		}
		return out, nil
	}
}

// failureMessage is the message of the answer the REST route gives a failed
// call that ended with err: the tool-server-failed answer's, or the
// runtime-error answer's, both scrubbed.
func (c *toolCaller) failureMessage(ctx context.Context, id string, err error) string {
	var answer *server.Error
	if errors.As(c.s.answerUse(ctx, id, err), &answer) {
		return answer.Message()
	}
	return "the tool call failed"
}

// toolText is the text of a tool result for a model: the tool's own text
// when it gave one, and otherwise the JSON of its data for a success and
// its error code and detail for a failure.
func toolText(res toolResult) string {
	switch {
	case res.Text != nil:
		return *res.Text
	case res.OK:
		return string(res.Data)
	}
	return res.Error + ": " + res.Detail
}
