package sandbox_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
)

// requestFile is the shape of the path of a tool call's request file.
var requestFile = regexp.MustCompile(`^/tmp/parley-tools/requests/[0-9a-f]{32}\.json$`)

// lastCall is the last call the stand-in ran.
func lastCall(t *testing.T, h *toolsHub) runtimetest.ToolCall {
	t.Helper()
	calls := h.tools.Calls()
	if len(calls) == 0 {
		t.Fatal("the tool server ran no call")
	}
	return calls[len(calls)-1]
}

func TestToolCall_RunsTheToolServerOnAnUploadedRequestAndAnswersItsResult(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "max_exec_timeout = \"5m\"\n")
	id := h.tooled(t, nil)

	t.Run("as a caller with the full set", func(t *testing.T) {
		uploads, streams := h.opsOf(runtime.OpUpload, id), h.opsOf(runtime.OpExecStream, id)
		a := h.tool(t, id, h.key, "read_file", map[string]any{"input": map[string]any{"path": "a.txt"}})
		o := decoded(t, a, http.StatusOK)
		want := map[string]any{"tool": "read_file", "input": map[string]any{"path": "a.txt"}}
		if o["ok"] != true || !reflect.DeepEqual(o["data"], want) {
			t.Errorf("the call answered %s; want ok true and the tool server's data %v", a.body, want)
		}
		if ms, isNumber := o["duration_ms"].(float64); !isNumber || ms < 0 {
			t.Errorf("duration_ms = %v; want a number of at least 0", o["duration_ms"])
		}
		for _, absent := range []string{"error", "detail", "text"} {
			if _, has := o[absent]; has {
				t.Errorf("the answer %s has the member %s, which the tool server did not give", a.body, absent)
			}
		}
		if u, s := h.opsOf(runtime.OpUpload, id)-uploads, h.opsOf(runtime.OpExecStream, id)-streams; u != 1 || s != 1 {
			t.Errorf("the call made %d uploads and ran %d streamed commands; want one of each", u, s)
		}
		c := lastCall(t, h)
		if !requestFile.MatchString(c.Request) || !c.RequestFound || c.RequestMode != 0o600 {
			t.Errorf("the request file is %q, found %v, with mode %o; want a random name under /tmp/parley-tools/requests with mode 600", c.Request, c.RequestFound, c.RequestMode)
		}
		if !sameJSON(c.RequestBody, json.RawMessage(`{"name":"read_file","input":{"path":"a.txt"}}`)) {
			t.Errorf("the request file holds %s; want the name and the input", c.RequestBody)
		}
		wantCommand := []string{toolServerPath, "call", "--request", c.Request, "--dir", "/workspace", "--index-dir", "/tmp/parley-tools/index", "--timeout", "60s"}
		if !slices.Equal(c.Command, wantCommand) {
			t.Errorf("the tool server ran as %q; want %q", c.Command, wantCommand)
		}
		// The runtime lays a request's environment over the sandbox's: what
		// the command got is the sandbox's alone.
		if c.WorkDir != sandbox.WorkDir || len(c.Stdin) != 0 || !reflect.DeepEqual(c.Env, map[string]string{"PARLEY_SANDBOX_ID": id, "MODE": "profile"}) {
			t.Errorf("the tool server ran in %q with %d bytes of input and the environment %v; want %s, no input and the sandbox's environment",
				c.WorkDir, len(c.Stdin), c.Env, sandbox.WorkDir)
		}
	})
	t.Run("as a caller with the read-only set", func(t *testing.T) {
		before := lastCall(t, h).Request
		decoded(t, h.tool(t, id, h.memberKey, "read_file", map[string]any{"input": map[string]any{"path": "a.txt"}}), http.StatusOK)
		c := lastCall(t, h)
		if c.Command[len(c.Command)-1] != "--read-only" || !c.ReadOnly || len(c.Command) != 11 {
			t.Errorf("the tool server ran as %q; want the full set's command and then --read-only", c.Command)
		}
		if c.Request == before {
			t.Errorf("two calls used the request file %s", c.Request)
		}
	})
	for _, row := range []struct{ name, timeout, want string }{
		{"no timeout", "", "60s"}, {"a timeout of 10s", "10s", "10s"}, {"a timeout above the hub's maximum", "1h", "300s"},
	} {
		t.Run("with "+row.name, func(t *testing.T) {
			body := map[string]any{}
			if row.timeout != "" {
				body["timeout"] = row.timeout
			}
			decoded(t, h.tool(t, id, h.key, "read_file", body), http.StatusOK)
			if c := lastCall(t, h); c.Timeout != row.want || string(c.Input) != "{}" {
				t.Errorf("the tool server got the bound %q and the input %s; want %s and {}", c.Timeout, c.Input, row.want)
			}
		})
	}
	t.Run("without a body", func(t *testing.T) {
		decoded(t, h.tool(t, id, h.key, "list_files", nil), http.StatusOK)
		if c := lastCall(t, h); c.Name != "list_files" || string(c.Input) != "{}" {
			t.Errorf("the tool server was asked for %s with %s; want list_files with {}", c.Name, c.Input)
		}
	})
	t.Run("a result with a text and without data", func(t *testing.T) {
		h.tools.SetCall(printing(`{"ok":true,"text":"three files"}`, "", 0))
		defer h.tools.SetCall(nil)
		o := decoded(t, h.tool(t, id, h.key, "list_files", nil), http.StatusOK)
		if o["text"] != "three files" || !reflect.DeepEqual(o["data"], map[string]any{}) {
			t.Errorf("the call answered %v; want the tool server's text and data {}", o)
		}
	})
}

func TestToolCall_RefusesABodyThatIsNotACall(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	if a, _ := h.list(t, id, h.key); a.status != http.StatusOK {
		t.Fatalf("GET tools = %d %s; want 200", a.status, a.body)
	}
	for _, row := range []struct{ body, member string }{
		{`[]`, ""}, {`"x"`, ""}, {`{"input": []}`, "input"}, {`{"input": "x"}`, "input"}, {`{"input": 7}`, "input"},
		{`{"timeout": "0s"}`, "timeout"}, {`{"timeout": "-1s"}`, "timeout"}, {`{"timeout": "soon"}`, "timeout"}, {`{"timeout": 5}`, "timeout"},
	} {
		t.Run(row.body, func(t *testing.T) {
			before := requestCalls(h.rt)
			a := h.tool(t, id, h.key, "read_file", json.RawMessage(row.body))
			if a.status != http.StatusBadRequest || a.err.ErrorType != "invalid_request" || !strings.Contains(a.err.Message, row.member) {
				t.Errorf("the call = %d %s; want 400 invalid_request naming %q", a.status, a.body, row.member)
			}
			if n := requestCalls(h.rt) - before; n != 0 {
				t.Errorf("the runtime got %d calls; want none", n)
			}
		})
	}
}

func TestToolCall_RefusesAToolOutsideTheCallersSet(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	refused := func(t *testing.T, cred, name string, status int, errorType string) {
		t.Helper()
		uploads, calls := h.opsOf(runtime.OpUpload, id), len(h.tools.Calls())
		a := h.tool(t, id, cred, name, map[string]any{"input": map[string]any{}})
		if a.status != status || a.err.ErrorType != errorType {
			t.Errorf("the call of %s = %d %s; want %d %s", name, a.status, a.body, status, errorType)
		}
		if errorType == "tool_not_found" && !strings.Contains(a.err.Message, name) {
			t.Errorf("the message %q does not name the tool %s", a.err.Message, name)
		}
		if u, c := h.opsOf(runtime.OpUpload, id)-uploads, len(h.tools.Calls())-calls; u != 0 || c != 0 {
			t.Errorf("the refused call made %d uploads and ran the tool server %d times; want neither", u, c)
		}
	}
	for _, name := range []string{"write_file", "edit_file", "execute", "run_command"} {
		t.Run(name+" called with the read-only set", func(t *testing.T) {
			refused(t, h.memberKey, name, http.StatusForbidden, "insufficient_scope")
		})
		t.Run(name+" called with the full set", func(t *testing.T) {
			decoded(t, h.tool(t, id, h.key, name, nil), http.StatusOK)
		})
	}
	t.Run("a tool no set holds, called with the full set", func(t *testing.T) {
		refused(t, h.key, "no_such_tool", http.StatusNotFound, "tool_not_found")
	})
	t.Run("a tool no set holds, called with the read-only set", func(t *testing.T) {
		refused(t, h.memberKey, "no_such_tool", http.StatusNotFound, "tool_not_found")
	})
}

func TestToolCall_RefusesAnInputOverFourMebibytes(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	// input is an input whose JSON has n bytes.
	input := func(n int) map[string]any {
		in := map[string]any{"p": strings.Repeat("x", n-len(`{"p":""}`))}
		if raw, _ := json.Marshal(in); len(raw) != n {
			t.Fatalf("the input has %d bytes as JSON; want %d", len(raw), n)
		}
		return in
	}
	// The stand-in's own answer repeats the input, which would be a result
	// over the limit.
	h.tools.SetCall(printing(`{"ok":true,"data":{}}`, "", 0))
	if a := h.tool(t, id, h.key, "write_file", map[string]any{"input": input(4 << 20)}); a.status != http.StatusOK {
		t.Errorf("a call with an input of 4 MiB = %d %.200s; want 200", a.status, a.body)
	}
	uploads := h.opsOf(runtime.OpUpload, id)
	a := h.tool(t, id, h.key, "write_file", map[string]any{"input": input(4<<20 + 1)})
	if a.status != http.StatusRequestEntityTooLarge || a.err.ErrorType != "payload_too_large" {
		t.Errorf("a call with an input of 4 MiB and a byte = %d %.200s; want 413 payload_too_large", a.status, a.body)
	}
	if n := h.opsOf(runtime.OpUpload, id) - uploads; n != 0 {
		t.Errorf("the refused call made %d uploads; want none", n)
	}
}

func TestToolCall_RemovesItsRequestFileHoweverItEnds(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	for _, row := range []struct {
		name   string
		call   func(context.Context, runtimetest.ToolCall, io.Writer, io.Writer) int
		status int
	}{
		{"a call that succeeds", nil, 200},
		{"a tool that fails", printing(`{"ok":false,"error":"not_found","detail":"no such file","data":{}}`, "", 0), 200},
		{"a tool server that exits 1", printing("", "boom", 1), 502},
	} {
		t.Run(row.name, func(t *testing.T) {
			h.tools.SetCall(row.call)
			removed := len(h.tools.Removed())
			if a := h.tool(t, id, h.key, "read_file", nil); a.status != row.status {
				t.Fatalf("the call = %d %s; want %d", a.status, a.body, row.status)
			}
			if left := h.requestFiles(id); len(left) != 0 {
				t.Errorf("the sandbox still holds the request files %v", left)
			}
			if got := h.tools.Removed()[removed:]; len(got) != 1 || got[0] != lastCall(t, h).Request {
				t.Errorf("the hub removed %v; want the request file %s once", got, lastCall(t, h).Request)
			}
		})
	}
	t.Run("a call that outlasts its bound", func(t *testing.T) {
		_, waiting, _ := h.held()
		answered := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, map[string]any{"timeout": "10s"}, nil)
		eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
		h.clock.Advance(10 * time.Second)
		if a := receive(t, answered); a.status != http.StatusOK {
			t.Fatalf("the call = %d %s; want 200", a.status, a.body)
		}
		if left := h.requestFiles(id); len(left) != 0 {
			t.Errorf("the sandbox still holds the request files %v", left)
		}
	})
	t.Run("a removal that fails changes nothing but the log", func(t *testing.T) {
		h.tools.SetCall(nil)
		want := decoded(t, h.tool(t, id, h.key, "read_file", map[string]any{"input": map[string]any{"path": "SECRET-IN-FILE"}}), http.StatusOK)
		h.cmds.set("rm", func(_ context.Context, _ string, _ runtime.ExecRequest, _, stderr io.Writer) int {
			_, _ = io.WriteString(stderr, "rm: read-only file system")
			return 1
		})
		defer h.cmds.set("rm", h.tools.Run)
		warnings := strings.Count(h.Log.String(), "request file")
		got := decoded(t, h.tool(t, id, h.key, "read_file", map[string]any{"input": map[string]any{"path": "SECRET-IN-FILE"}}), http.StatusOK)
		delete(want, "duration_ms")
		delete(got, "duration_ms")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("with a failed removal the call answered %v; want %v", got, want)
		}
		log := h.Log.String()
		if n := strings.Count(log, "request file") - warnings; n != 1 || !strings.Contains(log, id) {
			t.Errorf("the failed removal left %d lines about the request file; want one naming the sandbox", n)
		}
		if strings.Contains(log, "SECRET-IN-FILE") {
			t.Error("the hub's log holds the content of a request file")
		}
	})
}

func TestToolCall_GivesAToolsFailureAsItsResult(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	h.tools.SetCall(printing(`{"ok":false,"error":"not_found","detail":"no such file","data":{}}`, "", 0))
	o := decoded(t, h.tool(t, id, h.key, "read_file", map[string]any{"input": map[string]any{"path": "nope"}}), http.StatusOK)
	wantMembers(t, o, map[string]any{"ok": false, "error": "not_found", "detail": "no such file", "data": map[string]any{}})
}

func TestToolCall_EndsACallThatOutlastsItsBound(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "max_concurrent_execs = 1\n")
	id := h.tooled(t, nil)
	_, waiting, killed := h.held()
	answered := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, map[string]any{"timeout": "10s"}, nil)
	eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
	h.clock.Advance(9 * time.Second)
	select {
	case a := <-answered:
		t.Fatalf("the call answered %d %s before its bound", a.status, a.body)
	case <-time.After(20 * time.Millisecond):
	}
	h.clock.Advance(time.Second)
	o := decoded(t, receive(t, answered), http.StatusOK)
	wantMembers(t, o, map[string]any{"ok": false, "error": "timeout", "data": map[string]any{}, "duration_ms": 10000.0})
	eventually(t, "the tool server to be killed", func() bool { return killed.Load() == 1 })
	h.tools.SetCall(nil)
	if a := h.tool(t, id, h.key, "read_file", nil); a.status != http.StatusOK {
		t.Errorf("the next call = %d %s; want 200: the first gave its place back", a.status, a.body)
	}
}

func TestToolCall_CountsAmongTheSandboxesCommands(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "max_concurrent_execs = 1\n")
	h.cmds.set("block", h.cmds.blocking("", ""))
	id := h.tooled(t, nil)
	if a, _ := h.list(t, id, h.key); a.status != http.StatusOK {
		t.Fatalf("GET tools = %d %s; want 200", a.status, a.body)
	}
	busy := func(t *testing.T, a answer) {
		t.Helper()
		if a.status != http.StatusTooManyRequests || a.err.ErrorType != "rate_limited" || a.header.Get("Retry-After") != "" {
			t.Errorf("= %d %s (Retry-After %q); want 429 rate_limited without Retry-After", a.status, a.body, a.header.Get("Retry-After"))
		}
	}

	t.Run("while a tool call runs", func(t *testing.T) {
		release, waiting, _ := h.held()
		first := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, nil, nil)
		eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
		uploads := h.opsOf(runtime.OpUpload, id)
		busy(t, h.tool(t, id, h.key, "read_file", nil))
		busy(t, h.exec(t, id, map[string]any{"command": []string{"true"}}, nil))
		if n := h.opsOf(runtime.OpUpload, id) - uploads; n != 0 {
			t.Errorf("the refused call made %d uploads; want none", n)
		}
		release()
		if a := receive(t, first); a.status != http.StatusOK {
			t.Fatalf("the first call = %d %s; want 200", a.status, a.body)
		}
		h.tools.SetCall(nil)
		if a := h.tool(t, id, h.key, "read_file", nil); a.status != http.StatusOK {
			t.Errorf("a call once the first ended = %d %s; want 200", a.status, a.body)
		}
	})
	t.Run("while an exec runs", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		running := h.execing(ctx, id, map[string]any{"command": []string{"block"}}, nil)
		eventually(t, "the command to wait", func() bool { _, _, w, _ := h.cmds.counts(); return w == 1 })
		busy(t, h.tool(t, id, h.key, "read_file", nil))
		cancel()
		<-running
		eventually(t, "a call to be accepted", func() bool { return h.tool(t, id, h.key, "read_file", nil).status == http.StatusOK })
	})
}

func TestToolCall_FailsWhenTheToolServerOrTheRuntimeDoes(t *testing.T) {
	t.Parallel()
	const secretValue = "tok_SECRETVALUE"
	h := startToolsHub(t, "max_concurrent_execs = 1\n")
	id := h.tooled(t, nil)
	h.svc.SetValues(id, sandbox.Values{Exposed: []string{secretValue}})
	for _, row := range []struct {
		name string
		call func(context.Context, runtimetest.ToolCall, io.Writer, io.Writer) int
	}{
		{"cannot be started", printing("", "exec: no such file "+secretValue+" "+strings.Repeat("x", 3000), 127)},
		{"exits 1 after printing a result", printing(`{"ok":true,"data":{}}`, "boom", 1)},
		{"exits 2", printing("", "cannot read the request", 2)},
		{"prints something that is not JSON", printing("not json", "", 0)},
		{"prints nothing", printing("", "", 0)},
		{"prints an array", printing(`[]`, "", 0)},
		{"prints an object without ok", printing(`{}`, "", 0)},
		{"prints an ok that is not a boolean", printing(`{"ok":"yes"}`, "", 0)},
		{"prints an ok that is null", printing(`{"ok":null}`, "", 0)},
		{"prints data that is not an object", printing(`{"ok":true,"data":[1]}`, "", 0)},
		{"prints two objects", printing(`{"ok": true}{"ok": true}`, "", 0)},
		{"prints more than 4 MiB", printing(`{"ok":true,"data":{"p":"`+strings.Repeat("d", 4<<20)+`"}}`, "", 0)},
	} {
		t.Run("a tool server that "+row.name, func(t *testing.T) {
			h.tools.SetCall(row.call)
			a := h.tool(t, id, h.key, "read_file", nil)
			if a.status != http.StatusBadGateway || a.err.ErrorType != "tool_server_failed" {
				t.Fatalf("the call = %d %.300s; want 502 tool_server_failed", a.status, a.body)
			}
			if msg := a.err.Message; len(msg) > 1024 || msg == "" || strings.Contains(msg, secretValue) {
				t.Errorf("the message has %d bytes and holds the secret: %v; want at most 1024 without it: %.200q", len(msg), strings.Contains(msg, secretValue), msg)
			}
			if left := h.requestFiles(id); len(left) != 0 {
				t.Errorf("the sandbox still holds the request files %v", left)
			}
		})
	}
	h.tools.SetCall(nil)
	for _, op := range []string{runtime.OpUpload, runtime.OpExecStream} {
		t.Run("a runtime that fails "+op, func(t *testing.T) {
			h.rt.FailOperation(op, runtime.ErrUnavailable, "gateway gone "+secretValue)
			defer h.rt.FailOperation(op, nil, "")
			a := h.tool(t, id, h.key, "read_file", nil)
			if a.status != http.StatusBadGateway || a.err.ErrorType != "sandbox_runtime_error" || strings.Contains(a.err.Message, secretValue) {
				t.Errorf("the call = %d %.300s; want 502 sandbox_runtime_error without the secret", a.status, a.body)
			}
		})
	}
	t.Run("and then works, every place given back", func(t *testing.T) {
		if a := h.tool(t, id, h.key, "read_file", nil); a.status != http.StatusOK {
			t.Errorf("the call once the tool server works = %d %.300s; want 200", a.status, a.body)
		}
	})
	if strings.Contains(h.Log.String(), secretValue) {
		t.Error("the hub's log holds the secret value")
	}
}

func TestToolCall_AnswersTheFirstCheckARequestFails(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "max_concurrent_execs = 1\n")
	h.cmds.set("block", h.cmds.blocking("", ""))
	ready, plain := h.tooled(t, nil), h.sandbox(t, nil)
	stopped := h.tooled(t, func(rec *sandbox.Record) { rec.State = sandbox.StateStopped })
	if a, _ := h.list(t, ready, h.key); a.status != http.StatusOK {
		t.Fatalf("GET tools = %d %s; want 200", a.status, a.body)
	}
	running := h.execing(t.Context(), ready, map[string]any{"command": []string{"block"}}, nil)
	eventually(t, "the command to wait", func() bool { _, _, w, _ := h.cmds.counts(); return w == 1 })
	noScope := h.pat(t, h.owner, "git:read")
	const bad, good = `[]`, `{}`
	for _, row := range []struct {
		name, cred, id, tool, body string
		status                     int
		errorType                  string
	}{
		{"no sandboxes scope and an unknown sandbox", noScope, unknownID, "read_file", good, 403, "insufficient_scope"},
		{"an unknown sandbox and a body that is not an object", h.key, unknownID, "read_file", bad, 404, "sandbox_not_found"},
		{"a sandbox without tools and a body that is not an object", h.key, plain, "read_file", bad, 404, "tools_unavailable"},
		{"a body that is not an object and a stopped sandbox", h.key, stopped, "read_file", bad, 400, "invalid_request"},
		{"a stopped sandbox and an unknown tool", h.key, stopped, "no_such_tool", good, 409, "sandbox_not_ready"},
		{"an unknown tool with every place taken", h.key, ready, "no_such_tool", good, 404, "tool_not_found"},
		{"a tool outside the set with every place taken", h.memberKey, ready, "execute", good, 403, "insufficient_scope"},
		{"a tool of the set with every place taken", h.key, ready, "read_file", good, 429, "rate_limited"},
	} {
		t.Run(row.name, func(t *testing.T) {
			a := h.tool(t, row.id, row.cred, row.tool, json.RawMessage(row.body))
			if a.status != row.status || a.err.ErrorType != row.errorType {
				t.Errorf("the call = %d %s; want %d %s", a.status, a.body, row.status, row.errorType)
			}
		})
	}
	select {
	case a := <-running:
		t.Errorf("the command ended with %d %s during the refusals", a.status, a.body)
	default:
	}
}

// toolEvents are the tool call events of the sandbox id, oldest first.
func (h *toolsHub) toolEvents(t *testing.T, id string) []hubEvent {
	t.Helper()
	return h.sandboxEvents(t, "parley.sandbox.tool_call", id)
}

func TestToolCall_LeavesOneEventForEveryCallThatStarted(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	session := recordSession
	id := h.tooled(t, func(rec *sandbox.Record) { rec.SessionID = &session })
	h.listed(t, id, h.key, nil)
	h.listed(t, id, h.memberKey, nil)
	rest := func(cred, tool string, body any) func(t *testing.T) {
		return func(t *testing.T) { h.tool(t, id, cred, tool, body) }
	}
	for _, row := range []struct {
		name  string
		setup func(t *testing.T)
		call  func(t *testing.T)
		// actor is the user id of the caller.
		actor string
		want  map[string]any
	}{
		{"a call over REST with the full set that succeeds", nil, rest(h.key, "read_file", nil), h.owner,
			map[string]any{"tool": "read_file", "ok": true, "via": "rest", "set": "full"}},
		{"a call over MCP with the read-only set", nil, func(t *testing.T) { toolResultOf(t, h.mcpCall(t, id, h.memberKey, "search_files", nil)) }, h.member,
			map[string]any{"tool": "search_files", "ok": true, "via": "mcp", "set": "read_only"}},
		{"a tool that fails", func(*testing.T) {
			h.tools.SetCall(printing(`{"ok":false,"error":"not_found","detail":"no such file"}`, "", 0))
		},
			rest(h.key, "read_file", nil), h.owner,
			map[string]any{"tool": "read_file", "ok": false, "error": "not_found", "via": "rest", "set": "full"}},
		{"a tool server that exits 1", func(*testing.T) { h.tools.SetCall(printing("", "boom", 1)) }, rest(h.key, "edit_file", nil), h.owner,
			map[string]any{"tool": "edit_file", "ok": false, "error": "tool_server_failed", "via": "rest", "set": "full"}},
		{"a tool server that exits 1 over MCP", func(*testing.T) { h.tools.SetCall(printing("", "boom", 1)) },
			func(t *testing.T) { wantRPCError(t, h.mcpCall(t, id, h.key, "edit_file", nil), -32603) }, h.owner,
			map[string]any{"tool": "edit_file", "ok": false, "error": "tool_server_failed", "via": "mcp", "set": "full"}},
		{"a runtime that cannot run the tool server", func(t *testing.T) {
			h.rt.FailOperation(runtime.OpExecStream, runtime.ErrUnavailable, "gateway gone")
			t.Cleanup(func() { h.rt.FailOperation(runtime.OpExecStream, nil, "") })
		}, rest(h.key, "execute", nil), h.owner,
			map[string]any{"tool": "execute", "ok": false, "error": "sandbox_runtime_error", "via": "rest", "set": "full"}},
		{"a script that calls other tools", nil, rest(h.key, "code_mode", map[string]any{"input": map[string]any{"script": "read_file(); search_files()"}}), h.owner,
			map[string]any{"tool": "code_mode", "ok": true, "via": "rest", "set": "full"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h.tools.SetCall(nil)
			if row.setup != nil {
				row.setup(t)
			}
			before := len(h.toolEvents(t, id))
			row.call(t)
			evs := h.toolEvents(t, id)
			if len(evs)-before != 1 {
				t.Fatalf("the call left %d events; want one", len(evs)-before)
			}
			e := evs[len(evs)-1]
			if ms, isNumber := e.metadata["duration_ms"].(float64); !isNumber || ms < 0 {
				t.Errorf("duration_ms = %v; want a number of at least 0", e.metadata["duration_ms"])
			}
			delete(e.metadata, "duration_ms")
			if !reflect.DeepEqual(e.metadata, row.want) {
				t.Errorf("the event's metadata is %v; want %v and duration_ms", e.metadata, row.want)
			}
			if e.actorType != "api_key" || e.actorID == nil || *e.actorID != row.actor {
				t.Errorf("the event's actor is %s %v; want the caller's API key", e.actorType, e.actorID)
			}
			if e.resourceType != "sandbox" || e.workspace == nil || *e.workspace != "ws" || e.session == nil || *e.session != session {
				t.Errorf("the event is about %s in %v of the session %v; want the sandbox in ws of %s", e.resourceType, e.workspace, e.session, session)
			}
		})
	}
	t.Run("a call that outlasts its bound", func(t *testing.T) {
		_, waiting, _ := h.held()
		before := len(h.toolEvents(t, id))
		answered := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, map[string]any{"timeout": "10s"}, nil)
		eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
		h.clock.Advance(10 * time.Second)
		receive(t, answered)
		evs := h.toolEvents(t, id)
		if len(evs)-before != 1 {
			t.Fatalf("the call left %d events; want one", len(evs)-before)
		}
		want := map[string]any{"tool": "execute", "ok": false, "error": "timeout", "duration_ms": 10000.0, "via": "rest", "set": "full"}
		if got := evs[len(evs)-1].metadata; !reflect.DeepEqual(got, want) {
			t.Errorf("the event's metadata is %v; want %v", got, want)
		}
	})
	t.Run("only who may see the workspace reads the events", func(t *testing.T) {
		outsiderKey := h.IssueKey(t, h.outsider)
		for _, row := range []struct {
			name, cred string
			sees       bool
		}{{"a member of the org", h.memberKey, true}, {"a user outside the org", outsiderKey, false}} {
			a := h.call(t, http.MethodGet, "/events/log?limit=200", row.cred, nil)
			if a.status != http.StatusOK {
				t.Fatalf("GET /events/log as %s = %d %s; want 200", row.name, a.status, a.body)
			}
			if sees := strings.Contains(string(a.body), `"parley.sandbox.tool_call"`); sees != row.sees {
				t.Errorf("%s sees the tool call events: %v; want %v", row.name, sees, row.sees)
			}
		}
	})
}

func TestToolCall_KeepsArgumentsAndResultsOutOfItsEvent(t *testing.T) {
	t.Parallel()
	const in, out = "IN-7f3a", "OUT-9c1e"
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	for _, prints := range []string{
		`{"ok":true,"data":{"content":"` + out + `"},"text":"` + out + `"}`,
		`{"ok":false,"error":"not_found","detail":"` + out + `","data":{"path":"` + out + `"}}`,
		// A tool server's error is a code; what is not one is cut short.
		`{"ok":false,"error":"` + strings.Repeat("e", 100) + out + `","data":{}}`,
	} {
		h.tools.SetCall(printing(prints, out, 0))
		decoded(t, h.tool(t, id, h.key, "read_file", map[string]any{"input": map[string]any{"path": in}}), http.StatusOK)
		toolResultOf(t, h.mcpCall(t, id, h.key, "read_file", map[string]any{"path": in}))
	}
	rows, err := h.Store.Main().Query(`SELECT metadata FROM hub_events WHERE type = 'parley.sandbox.tool_call' AND resource_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		n++
		if strings.Contains(raw, in) || strings.Contains(raw, out) {
			t.Errorf("the event's metadata %s holds an argument or a part of the result", raw)
		}
		var meta map[string]any
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatal(err)
		}
		for k := range meta {
			if !slices.Contains([]string{"tool", "ok", "error", "duration_ms", "via", "set"}, k) {
				t.Errorf("the event's metadata %s has the member %s", raw, k)
			}
		}
	}
	if err := rows.Err(); err != nil || n != 6 {
		t.Errorf("read %d events (%v); want the six calls'", n, err)
	}
	if log := h.Log.String(); strings.Contains(log, in) || strings.Contains(log, out) {
		t.Error("the hub's log holds an argument or a part of a result")
	}
}

// toolCallSamples are the samples of the tool call counter in the hub's
// metrics that are above 0, as their label sets.
func (h *toolsHub) toolCallSamples(t *testing.T) []string {
	t.Helper()
	_, body := h.Call(t, http.MethodGet, "/metrics", h.Token, nil)
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if labels, value, ok := strings.Cut(line, "} "); ok && strings.HasPrefix(labels, "parley_sandbox_tool_calls_total{") && value != "0" {
			out = append(out, strings.TrimPrefix(labels, "parley_sandbox_tool_calls_total{")+" "+value)
		}
	}
	slices.Sort(out)
	return out
}

func TestToolCall_RecordsNothingForARequestThatNeverRan(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "max_concurrent_execs = 1\n")
	h.cmds.set("block", h.cmds.blocking("", ""))
	id := h.tooled(t, nil)
	stopped := h.tooled(t, func(rec *sandbox.Record) { rec.State = sandbox.StateStopped })
	for _, row := range []struct {
		name string
		call func(t *testing.T) int
		want int
	}{
		{"a listing over MCP", func(t *testing.T) int { return h.mcp(t, id, h.memberKey, "tools/list", nil, nil).status }, 200},
		{"a tool outside the caller's set", func(t *testing.T) int { return h.tool(t, id, h.memberKey, "execute", nil).status }, 403},
		{"a tool no set holds", func(t *testing.T) int { return h.tool(t, id, h.key, "no_such_tool", nil).status }, 404},
		{"a body that is not a call", func(t *testing.T) int { return h.tool(t, id, h.key, "read_file", json.RawMessage(`[]`)).status }, 400},
		{"an input over 4 MiB", func(t *testing.T) int {
			return h.tool(t, id, h.key, "read_file", map[string]any{"input": map[string]any{"p": strings.Repeat("x", 4<<20)}}).status
		}, 413},
		{"a stopped sandbox", func(t *testing.T) int { return h.tool(t, stopped, h.key, "read_file", nil).status }, 409},
		{"a tool outside the set over MCP", func(t *testing.T) int {
			a := h.mcpCall(t, id, h.memberKey, "execute", nil)
			wantRPCError(t, a, -32602)
			return a.status
		}, 200},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := row.call(t); got != row.want {
				t.Errorf("answered %d; want %d", got, row.want)
			}
		})
	}
	t.Run("a call over the sandbox's limit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		running := h.execing(ctx, id, map[string]any{"command": []string{"block"}}, nil)
		eventually(t, "the command to wait", func() bool { _, _, w, _ := h.cmds.counts(); return w == 1 })
		if a := h.tool(t, id, h.key, "read_file", nil); a.status != http.StatusTooManyRequests {
			t.Errorf("a call over REST = %d %s; want 429", a.status, a.body)
		}
		wantRPCError(t, h.mcpCall(t, id, h.key, "read_file", nil), -32000)
		cancel()
		<-running
	})
	for _, sb := range []string{id, stopped} {
		if evs := h.toolEvents(t, sb); len(evs) != 0 {
			t.Errorf("the refused requests left %d tool call events for %s; want none", len(evs), sb)
		}
	}
	if got := h.toolCallSamples(t); len(got) != 0 {
		t.Errorf("the refused requests were counted as tool calls: %v", got)
	}
	// The counter is there to count the next call.
	eventually(t, "a call to be accepted", func() bool { return h.tool(t, id, h.key, "read_file", nil).status == http.StatusOK })
	if got := h.toolCallSamples(t); len(got) != 1 {
		t.Errorf("after one call the counter has the samples %v; want one", got)
	}
}

func TestToolCall_EndsACallNobodyWaitsForAndRecordsItAsFailed(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "max_concurrent_execs = 2\n")
	id := h.tooled(t, nil)
	h.listed(t, id, h.key, nil)
	_, waiting, killed := h.held()
	failed := map[string]any{"tool": "execute", "ok": false, "error": "tool_server_failed", "set": "full"}
	wantEvents := func(t *testing.T, vias ...string) {
		t.Helper()
		evs := h.toolEvents(t, id)
		if len(evs) != len(vias) {
			t.Fatalf("the sandbox has %d tool call events; want %d", len(evs), len(vias))
		}
		var got []string
		for i := range evs {
			via, _ := evs[i].metadata["via"].(string)
			got = append(got, via)
			delete(evs[i].metadata, "duration_ms")
			failed["via"] = via
			if !reflect.DeepEqual(evs[i].metadata, failed) {
				t.Errorf("event %d has the metadata %v; want %v", i, evs[i].metadata, failed)
			}
		}
		slices.Sort(got)
		slices.Sort(vias)
		if !slices.Equal(got, vias) {
			t.Errorf("the events came through %v; want %v", got, vias)
		}
	}

	t.Run("when its caller leaves", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		answered := h.sending(ctx, http.MethodPost, toolPath(id, "execute"), h.key, nil, nil)
		eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
		cancel()
		if a := <-answered; a.status != 0 {
			t.Errorf("the caller got %d %s; want no answer", a.status, a.body)
		}
		eventually(t, "the tool server to be killed", func() bool { return killed.Load() == 1 })
		eventually(t, "the call's event", func() bool { return len(h.toolEvents(t, id)) == 1 })
		wantEvents(t, "rest")
		if left := h.requestFiles(id); len(left) != 0 {
			t.Errorf("the sandbox still holds the request files %v", left)
		}
	})
	t.Run("when its caller leaves an MCP call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		answered := h.sending(ctx, http.MethodPost, sandboxPath("ws", id)+"/mcp", h.key,
			map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "execute"}}, mcpHeader)
		eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
		cancel()
		<-answered
		eventually(t, "the tool server to be killed", func() bool { return killed.Load() == 2 })
		eventually(t, "the call's event", func() bool { return len(h.toolEvents(t, id)) == 2 })
		wantEvents(t, "rest", "mcp")
		if got, want := h.toolCallSamples(t), []string{`tool="execute",outcome="failed" 2`}; !slices.Equal(got, want) {
			t.Errorf("the tool call counter has the samples %v; want %v", got, want)
		}
	})
	t.Run("when the hub begins to stop", func(t *testing.T) {
		overREST := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, nil, nil)
		overMCP := h.sending(context.Background(), http.MethodPost, sandboxPath("ws", id)+"/mcp", h.key,
			map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "execute"}}, mcpHeader)
		eventually(t, "the calls to run", func() bool { return waiting.Load() == 2 })
		h.Stop()
		if code := h.ExitCode(t, 10*time.Second); code != 0 {
			t.Fatalf("the hub stopped with %d; want 0", code)
		}
		for name, answered := range map[string]<-chan answer{"REST": overREST, "MCP": overMCP} {
			if a := receive(t, answered); a.status != 0 {
				t.Errorf("the caller over %s got %d %s; want no answer", name, a.status, a.body)
			}
		}
		if killed.Load() != 4 {
			t.Errorf("the runtime killed %d tool servers; want 4", killed.Load())
		}
		h.Restart(t)
		wantEvents(t, "rest", "mcp", "rest", "mcp")
	})
}
