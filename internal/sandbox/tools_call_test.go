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
		answered := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, map[string]any{"timeout": "10s"})
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
	answered := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, map[string]any{"timeout": "10s"})
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
		first := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, nil)
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
