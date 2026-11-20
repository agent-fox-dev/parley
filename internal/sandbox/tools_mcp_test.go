package sandbox_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
)

// initializeParams are the parameters an MCP client initializes with.
var initializeParams = map[string]any{
	"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
	"clientInfo": map[string]any{"name": "test", "version": "1"},
}

// mcpTools are the tools of a tools/list result.
type mcpTools struct {
	Tools      []map[string]json.RawMessage `json:"tools"`
	NextCursor *string                      `json:"nextCursor"`
}

// listed sends tools/list and returns its tools.
func (h *toolsHub) listed(t *testing.T, id, cred string, extra http.Header) mcpTools {
	t.Helper()
	a := h.mcp(t, id, cred, "tools/list", nil, extra)
	if a.status != http.StatusOK || a.rpcErr != nil {
		t.Fatalf("tools/list = %d %.400s; want a result", a.status, a.body)
	}
	var l mcpTools
	if err := json.Unmarshal(a.result, &l); err != nil {
		t.Fatalf("the result %s: %v", a.result, err)
	}
	return l
}

// names are the names of the listed tools, in order.
func (l mcpTools) names() []string {
	var out []string
	for _, tool := range l.Tools {
		var name string
		_ = json.Unmarshal(tool["name"], &name)
		out = append(out, name)
	}
	return out
}

func TestMCP_InitializeNamesTheHubAndPingAnswers(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	_, body := h.Call(t, http.MethodGet, "/version", "", nil)
	var version struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &version); err != nil || version.Version == "" {
		t.Fatalf("GET /version = %s (%v)", body, err)
	}

	a := h.mcp(t, id, h.key, "initialize", initializeParams, nil)
	if a.status != http.StatusOK || a.rpcErr != nil {
		t.Fatalf("initialize = %d %s; want a result", a.status, a.body)
	}
	if ct := a.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || a.header.Get("Mcp-Session-Id") != "" {
		t.Errorf("the answer has Content-Type %q and Mcp-Session-Id %q; want application/json and no session id", ct, a.header.Get("Mcp-Session-Id"))
	}
	var r struct {
		ServerInfo   struct{ Name, Version string }
		Capabilities map[string]json.RawMessage
		Instructions string
	}
	if err := json.Unmarshal(a.result, &r); err != nil {
		t.Fatal(err)
	}
	if r.ServerInfo.Name != "parley" || r.ServerInfo.Version != version.Version {
		t.Errorf("serverInfo = %+v; want parley in the hub's version %s", r.ServerInfo, version.Version)
	}
	if got := slices.Sorted(func(yield func(string) bool) {
		for k := range r.Capabilities {
			if !yield(k) {
				return
			}
		}
	}); !slices.Equal(got, []string{"tools"}) {
		t.Errorf("capabilities = %v; want tools alone", got)
	}
	in := r.Instructions
	if !strings.Contains(in, "ws") || !strings.Contains(in, id) || !strings.HasSuffix(in, ".") || strings.Count(in, ". ") != 0 || strings.Contains(in, "\n") {
		t.Errorf("instructions = %q; want one sentence naming the workspace ws and the sandbox %s", in, id)
	}

	a = h.mcp(t, id, h.key, "ping", nil, nil)
	if a.status != http.StatusOK || a.rpcErr != nil || !sameJSON(a.result, json.RawMessage(`{}`)) {
		t.Errorf("ping = %d %s; want an empty result", a.status, a.body)
	}
}

func TestMCP_ServesEveryRequestOnItsOwn(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	want := definitionNames(false)
	if got := h.listed(t, id, h.key, nil).names(); !slices.Equal(got, want) {
		t.Errorf("tools/list without an initialize = %v; want %v", got, want)
	}
	a := h.mcp(t, id, h.key, "tools/list", nil, http.Header{"Mcp-Session-Id": {"x"}})
	if a.status != http.StatusOK || a.rpcErr != nil || a.header.Get("Mcp-Session-Id") != "" {
		t.Errorf("tools/list with a session id = %d %.300s (Mcp-Session-Id %q); want the tools and no session id", a.status, a.body, a.header.Get("Mcp-Session-Id"))
	}
	note := map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}
	if a := h.mcpRaw(t, id, h.key, note, nil); a.status != http.StatusAccepted && a.status != http.StatusOK {
		t.Errorf("notifications/initialized = %d %s; want it accepted", a.status, a.body)
	}
	h.Restart(t)
	if got := h.listed(t, id, h.key, nil).names(); !slices.Equal(got, want) {
		t.Errorf("tools/list after a restart of the hub = %v; want %v", got, want)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		status, _, body := h.Exchange(t, method, "/api/v1"+sandboxPath("ws", id)+"/mcp", h.key, nil, mcpHeader)
		if status != http.StatusMethodNotAllowed {
			t.Errorf("%s …/mcp = %d %s; want 405", method, status, body)
		}
	}
}

func TestMCP_ListsTheCallersSetAsTheToolServerDefinesIt(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	for _, row := range []struct {
		name, cred string
		readOnly   bool
	}{{"with the full set", h.key, false}, {"with the read-only set", h.memberKey, true}} {
		t.Run(row.name, func(t *testing.T) {
			l := h.listed(t, id, row.cred, nil)
			defs := runtimetest.ToolDefinitions(row.readOnly)
			if len(l.Tools) != len(defs) || l.NextCursor != nil {
				t.Fatalf("tools/list gave %d tools %v and the cursor %v; want the tool server's %d on one page", len(l.Tools), l.names(), l.NextCursor, len(defs))
			}
			for i, d := range defs {
				got := l.Tools[i]
				var name, description string
				_ = json.Unmarshal(got["name"], &name)
				_ = json.Unmarshal(got["description"], &description)
				if name != d.Name || description != d.Description || !sameJSON(got["inputSchema"], d.InputSchema) {
					t.Errorf("tool %d = %v; want %s with its description and input schema", i, got, d.Name)
				}
				_, hasOutput := got["outputSchema"]
				if wantOutput := d.Name != "code_mode"; hasOutput != wantOutput || (wantOutput && !sameJSON(got["outputSchema"], d.OutputSchema)) {
					t.Errorf("tool %s has the output schema %s; want it %v as the tool server gave it", d.Name, got["outputSchema"], wantOutput)
				}
			}
			for _, changing := range []string{"write_file", "edit_file", "execute", "run_command"} {
				if has := slices.Contains(l.names(), changing); has == row.readOnly {
					t.Errorf("the list holds %s: %v", changing, has)
				}
			}
		})
	}
}

func TestMCP_CallsAToolAndAnswersItsResult(t *testing.T) {
	t.Parallel()
	for _, hub := range []struct{ name, settings, timeout string }{
		{"on a hub with the default maximum", "", "600s"},
		{"on a hub whose maximum is below ten minutes", "max_exec_timeout = \"5m\"\n", "300s"},
	} {
		t.Run(hub.name, func(t *testing.T) {
			t.Parallel()
			h := startToolsHub(t, hub.settings)
			id := h.tooled(t, nil)
			for _, row := range []struct {
				name, tool, prints string
				isError            bool
				text, structured   string
			}{
				{"a success with a text", "read_file", `{"ok":true,"data":{"n":1},"text":"one line"}`, false, "one line", `{"n":1}`},
				{"a success without a text", "read_file", `{"ok":true,"data":{"n":1}}`, false, `{"n":1}`, `{"n":1}`},
				{"a failure", "read_file", `{"ok":false,"error":"not_found","detail":"no such file","data":{}}`, true, "not_found: no such file", `{}`},
				{"a tool without an output schema", "code_mode", `{"ok":true,"data":{"n":2}}`, false, `{"n":2}`, ""},
			} {
				t.Run(row.name, func(t *testing.T) {
					h.tools.SetCall(printing(row.prints, "", 0))
					r := toolResultOf(t, h.mcpCall(t, id, h.key, row.tool, map[string]any{"path": "a.txt"}))
					if r.IsError != row.isError || len(r.Content) != 1 || r.Content[0].Type != "text" {
						t.Fatalf("the result is %+v; want isError %v and one text", r, row.isError)
					}
					if got := r.Content[0].Text; got != row.text && !(strings.HasPrefix(row.text, "{") && sameJSON(json.RawMessage(got), json.RawMessage(row.text))) {
						t.Errorf("the text is %q; want %q", got, row.text)
					}
					if row.structured == "" {
						if len(r.StructuredContent) != 0 {
							t.Errorf("structuredContent = %s; want none for a tool without an output schema", r.StructuredContent)
						}
					} else if !sameJSON(r.StructuredContent, json.RawMessage(row.structured)) {
						t.Errorf("structuredContent = %s; want the data %s", r.StructuredContent, row.structured)
					}
					c := lastCall(t, h)
					if c.Name != row.tool || !sameJSON(c.Input, json.RawMessage(`{"path":"a.txt"}`)) || c.Timeout != hub.timeout {
						t.Errorf("the tool server was asked for %s with %s under %s; want %s with the arguments under %s", c.Name, c.Input, c.Timeout, row.tool, hub.timeout)
					}
				})
			}
			t.Run("without arguments", func(t *testing.T) {
				h.tools.SetCall(nil)
				toolResultOf(t, h.mcpCall(t, id, h.key, "list_files", nil))
				if c := lastCall(t, h); string(c.Input) != "{}" {
					t.Errorf("the tool server got the input %s; want {}", c.Input)
				}
			})
			t.Run("with the read-only set", func(t *testing.T) {
				h.tools.SetCall(nil)
				toolResultOf(t, h.mcpCall(t, id, h.memberKey, "read_file", nil))
				if c := lastCall(t, h); !c.ReadOnly {
					t.Errorf("the tool server ran as %q; want --read-only", c.Command)
				}
			})
		})
	}
}

func TestMCP_GivesACallThatOutlastsItsBoundAsAResult(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	h.listed(t, id, h.key, nil)
	_, waiting, _ := h.held()
	answered := h.sending(context.Background(), http.MethodPost, sandboxPath("ws", id)+"/mcp", h.key,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "execute"}})
	eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
	h.clock.Advance(10 * 60 * 1e9)
	a := rpcAnswer{answer: receive(t, answered)}
	readRPC(&a)
	r := toolResultOf(t, a)
	if !r.IsError || len(r.Content) != 1 || !strings.HasPrefix(r.Content[0].Text, "timeout: ") {
		t.Errorf("the result is %+v; want isError with a text that begins with timeout", r)
	}
}

func TestMCP_RefusesAToolOutsideTheSetAndArgumentsOverFourMebibytes(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	h.listed(t, id, h.key, nil)
	h.listed(t, id, h.memberKey, nil)
	for _, row := range []struct {
		name, cred, tool string
		arguments        any
		names            string
	}{
		{"a tool outside the caller's set", h.memberKey, "execute", nil, "execute"},
		{"a tool no set holds", h.key, "no_such_tool", nil, "no_such_tool"},
		{"arguments over 4 MiB", h.key, "write_file", map[string]any{"p": strings.Repeat("x", 4<<20)}, "4194304"},
		{"arguments that are not an object", h.key, "write_file", []any{1}, "object"},
	} {
		t.Run(row.name, func(t *testing.T) {
			uploads, events := h.opsOf(runtime.OpUpload, id), len(h.sandboxEvents(t, "parley.sandbox.tool_call", id))
			a := h.mcpCall(t, id, row.cred, row.tool, row.arguments)
			wantRPCError(t, a, -32602)
			if !strings.Contains(a.rpcErr.Message, row.names) {
				t.Errorf("the message %q does not name %s", a.rpcErr.Message, row.names)
			}
			if n := h.opsOf(runtime.OpUpload, id) - uploads; n != 0 {
				t.Errorf("the refused call made %d uploads; want none", n)
			}
			if n := len(h.sandboxEvents(t, "parley.sandbox.tool_call", id)) - events; n != 0 {
				t.Errorf("the refused call left %d events; want none", n)
			}
		})
	}
}

func TestMCP_AnswersAFailedCallAndABusySandboxAsErrors(t *testing.T) {
	t.Parallel()
	const secretValue = "tok_SECRETVALUE"
	h := startToolsHub(t, "max_concurrent_execs = 1\n")
	id := h.tooled(t, nil)
	h.svc.SetValues(id, sandbox.Values{Exposed: []string{secretValue}})
	h.listed(t, id, h.key, nil)

	t.Run("a tool server that exits 1", func(t *testing.T) {
		h.tools.SetCall(printing("", "boom "+secretValue+" "+strings.Repeat("x", 3000), 1))
		a := h.mcpCall(t, id, h.key, "read_file", nil)
		wantRPCError(t, a, -32603)
		if msg := a.rpcErr.Message; len(msg) > 1024 || strings.Contains(msg, secretValue) || !strings.Contains(msg, "tool server") {
			t.Errorf("the message has %d bytes: %.200q; want at most 1024 about the tool server and without the secret", len(msg), msg)
		}
	})
	t.Run("a runtime that cannot be reached", func(t *testing.T) {
		h.tools.SetCall(nil)
		h.rt.FailOperation(runtime.OpExecStream, runtime.ErrUnavailable, "gateway gone "+secretValue)
		defer h.rt.FailOperation(runtime.OpExecStream, nil, "")
		a := h.mcpCall(t, id, h.key, "read_file", nil)
		wantRPCError(t, a, -32603)
		if strings.Contains(a.rpcErr.Message, secretValue) {
			t.Errorf("the message %q holds the secret", a.rpcErr.Message)
		}
	})
	t.Run("a sandbox that runs as many commands as it may", func(t *testing.T) {
		release, waiting, _ := h.held()
		first := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, nil)
		eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
		a := h.mcpCall(t, id, h.key, "read_file", nil)
		wantRPCError(t, a, -32000)
		if a.rpcErr.Message != "sandbox busy" {
			t.Errorf("the message is %q; want sandbox busy", a.rpcErr.Message)
		}
		release()
		receive(t, first)
	})
}

func TestMCP_AnswersWhatIsDecidedBeforeTheMessageAsPlainHTTP(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	ready, plain := h.tooled(t, nil), h.sandbox(t, nil)
	stopped := h.tooled(t, func(rec *sandbox.Record) { rec.State = sandbox.StateStopped })
	list := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
	padded := func(n int) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{"_meta": map[string]any{"pad": strings.Repeat("x", n)}}}
	}
	for _, row := range []struct {
		name, cred, id string
		body           any
		status         int
		errorType      string
	}{
		{"no credential", "", ready, list, 401, "unauthenticated"},
		{"a PAT without a sandboxes scope", h.pat(t, h.owner, "git:read"), ready, list, 403, "insufficient_scope"},
		{"an unknown sandbox", h.key, unknownID, list, 404, "sandbox_not_found"},
		{"a sandbox without tools", h.key, plain, list, 404, "tools_unavailable"},
		{"a stopped sandbox", h.key, stopped, list, 409, "sandbox_not_ready"},
		{"a body over 8 MiB", h.key, ready, padded(8 << 20), 413, "payload_too_large"},
		{"a body over 8 MiB on a stopped sandbox", h.key, stopped, padded(8 << 20), 413, "payload_too_large"},
	} {
		t.Run(row.name, func(t *testing.T) {
			a := h.mcpRaw(t, row.id, row.cred, row.body, nil)
			if a.status != row.status || a.err.ErrorType != row.errorType || a.isRPC {
				t.Errorf("= %d %.300s; want %d %s as the hub's error object, not JSON-RPC", a.status, a.body, row.status, row.errorType)
			}
		})
	}
	t.Run("a body of 5 MiB is read", func(t *testing.T) {
		a := h.mcpRaw(t, ready, h.key, padded(5<<20), nil)
		if a.status != http.StatusOK || !a.isRPC || a.rpcErr != nil {
			t.Errorf("= %d %.300s; want the tools", a.status, a.body)
		}
	})
}

func TestMCP_AnswersAnyOtherMethodNotFound(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	for _, row := range []struct {
		method string
		params any
	}{
		{"resources/list", nil}, {"resources/read", map[string]any{"uri": "file:///a"}}, {"resources/templates/list", nil},
		{"prompts/list", nil}, {"prompts/get", map[string]any{"name": "p"}},
		{"completion/complete", map[string]any{"ref": map[string]any{"type": "ref/prompt", "name": "p"}, "argument": map[string]any{"name": "a", "value": "b"}}},
		{"logging/setLevel", map[string]any{"level": "info"}}, {"no/such", nil},
	} {
		t.Run(row.method, func(t *testing.T) {
			wantRPCError(t, h.mcp(t, id, h.key, row.method, row.params, nil), -32601)
		})
	}
}
