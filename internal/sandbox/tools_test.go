package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
)

// toolsAccess is the hub credential of the sandboxes whose tool set the
// tests decide: a caller covers it with sandboxes:write and git:write and no
// paths limit.
var toolsAccess = &sandbox.HubAccess{Scopes: []string{"sandboxes:write", "git:write"}}

func TestTools_GiveEveryToolToACallerWhoMayExecAndTheReadOnlySetToOtherReaders(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, func(rec *sandbox.Record) { rec.HubAccess = toolsAccess })
	pat := func(user string, paths []string, workspace string, permissions ...string) string {
		return h.IssuePAT(t, identity.PATSpec{UserID: user, Permissions: permissions, Paths: paths, Workspace: workspace})
	}
	for _, row := range []struct {
		name, cred, set string
	}{
		{"the admin token", h.Token, "full"},
		{"the API key of the owner", h.key, "full"},
		{"a PAT of the owner that covers the hub credential", pat(h.owner, nil, "", "sandboxes:write", "git:write"), "full"},
		{"a token bound to the workspace that covers the hub credential", pat(h.owner, nil, "ws", "sandboxes:write", "git:write"), "full"},
		{"a PAT of the owner with sandboxes:read", pat(h.owner, nil, "", "sandboxes:read", "git:write"), "read_only"},
		{"a PAT of a member who may only read the workspace", pat(h.member, nil, "", "sandboxes:write", "git:write"), "read_only"},
		{"a PAT of the owner that lacks a scope of the hub credential", pat(h.owner, nil, "", "sandboxes:write"), "read_only"},
		{"a PAT of the owner limited to paths the hub credential is not", pat(h.owner, []string{"src/**"}, "", "sandboxes:write", "git:write"), "read_only"},
		{"the API key of a member who may only read the workspace", h.memberKey, "read_only"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			a, l := h.list(t, id, row.cred)
			if a.status != http.StatusOK || l.Set != row.set {
				t.Fatalf("GET tools = %d %.300s; want 200 with the set %s", a.status, a.body, row.set)
			}
			if want := definitionNames(row.set == "read_only"); !slices.Equal(l.names(), want) {
				t.Errorf("the %s set lists %v; want %v", row.set, l.names(), want)
			}
			for _, changing := range []string{"write_file", "edit_file", "execute", "run_command"} {
				if has := slices.Contains(l.names(), changing); has != (row.set == "full") {
					t.Errorf("the %s set holds %s: %v", row.set, changing, has)
				}
			}
		})
	}
}

func TestTools_RefuseACallerTheLogsRouteRefusesTheSameWay(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	for _, u := range toolRoutes() {
		for _, row := range []struct {
			name, cred string
			status     int
		}{
			{"no credential", "", 401},
			{"a PAT without a sandboxes scope", h.pat(t, h.owner, "git:read"), 403},
			{"a token bound to another workspace", h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"sandboxes:write"}, Workspace: "ws2"}), 0},
			{"a PAT of a user outside the org", h.pat(t, h.outsider, "sandboxes:write"), 404},
		} {
			t.Run(u.String()+" with "+row.name, func(t *testing.T) {
				logs := h.call(t, http.MethodGet, sandboxPath("ws", id)+"/logs", row.cred, nil)
				before := requestCalls(h.rt)
				a := h.call(t, u.method, u.path("ws", id), row.cred, u.body())
				if logs.status < 400 || a.status != logs.status || a.err.ErrorType != logs.err.ErrorType {
					t.Errorf("%s = %d %s; want what the logs route answers, %d %s", u, a.status, a.body, logs.status, logs.body)
				}
				if row.status != 0 && a.status != row.status {
					t.Errorf("%s = %d; want %d", u, a.status, row.status)
				}
				if n := requestCalls(h.rt) - before; n != 0 {
					t.Errorf("the runtime got %d calls; want none", n)
				}
			})
		}
	}
}

func TestTools_AnswerASandboxTheWorkspaceDoesNotHaveNotFound(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	h.tooled(t, nil)
	other := h.tooled(t, func(rec *sandbox.Record) { rec.Workspace, rec.FamilyRoot = "ws2", "ws2" })
	for _, u := range toolRoutes() {
		for _, row := range []struct{ name, id string }{{"an unknown id", unknownID}, {"the id of another workspace's sandbox", other}} {
			t.Run(u.String()+" with "+row.name, func(t *testing.T) {
				a := h.call(t, u.method, u.path("ws", row.id), h.key, u.body())
				if a.status != http.StatusNotFound || a.err.ErrorType != "sandbox_not_found" {
					t.Errorf("%s = %d %s; want 404 sandbox_not_found", u, a.status, a.body)
				}
			})
		}
	}
}

func TestTools_AreUnavailableOnASandboxWithoutAToolServer(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.sandbox(t, nil)
	noScope := h.pat(t, h.owner, "git:read")
	for _, u := range toolRoutes() {
		t.Run(u.String(), func(t *testing.T) {
			before := requestCalls(h.rt)
			a := h.call(t, u.method, u.path("ws", id), h.key, u.body())
			if a.status != http.StatusNotFound || a.err.ErrorType != "tools_unavailable" {
				t.Errorf("%s = %d %s; want 404 tools_unavailable", u, a.status, a.body)
			}
			a = h.call(t, u.method, u.path("ws", id), noScope, u.body())
			if a.status != http.StatusForbidden || a.err.ErrorType != "insufficient_scope" {
				t.Errorf("%s without a sandboxes scope = %d %s; want 403 insufficient_scope", u, a.status, a.body)
			}
			if n := requestCalls(h.rt) - before; n != 0 {
				t.Errorf("the runtime got %d calls; want none", n)
			}
		})
	}
	if a := h.exec(t, id, map[string]any{"command": []string{"true"}}, nil); a.status != http.StatusOK {
		t.Errorf("exec in the sandbox without tools = %d %s; want 200", a.status, a.body)
	}
	if l := h.tools.Listings(); len(l) != 0 {
		t.Errorf("the tool server was asked for its tools %d times; want never", len(l))
	}
}

func TestToolList_GivesTheCallersSetAsTheToolServerDefinesIt(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	for _, row := range []struct {
		name, cred string
		readOnly   bool
	}{{"full", h.key, false}, {"read_only", h.memberKey, true}} {
		t.Run(row.name, func(t *testing.T) {
			a, l := h.list(t, id, row.cred)
			if a.status != http.StatusOK || l.Set != row.name {
				t.Fatalf("GET tools = %d %.300s; want 200 with the set %s", a.status, a.body, row.name)
			}
			want := runtimetest.ToolDefinitions(row.readOnly)
			if len(l.Tools) != len(want) {
				t.Fatalf("%d tools %v; want the tool server's %d", len(l.Tools), l.names(), len(want))
			}
			for i, w := range want {
				got := l.Tools[i]
				if got.Name != w.Name || got.Description != w.Description || !sameJSON(got.InputSchema, w.InputSchema) || !sameJSON(got.OutputSchema, w.OutputSchema) {
					t.Errorf("tool %d = %s %q %s %s; want %s %q %s %s", i, got.Name, got.Description, got.InputSchema, got.OutputSchema,
						w.Name, w.Description, w.InputSchema, w.OutputSchema)
				}
			}
			var raw struct {
				Tools []map[string]json.RawMessage `json:"tools"`
			}
			if err := json.Unmarshal(a.body, &raw); err != nil {
				t.Fatal(err)
			}
			last := raw.Tools[len(raw.Tools)-1]
			if string(last["name"]) != `"code_mode"` || string(last["output_schema"]) != "null" {
				t.Errorf("the tool without an output schema is listed as %v; want output_schema null", last)
			}
		})
	}
}

// sameJSON reports whether a and b hold the same JSON value.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}

func TestToolList_ReadsTheDefinitionsOncePerSandboxAndSetWhileItStaysReady(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	ctx := context.Background()
	listings := func() (full, readOnly int) {
		for _, l := range h.tools.Listings() {
			if l.ReadOnly {
				readOnly++
			} else {
				full++
			}
		}
		return full, readOnly
	}
	want := func(step string, full, readOnly int) {
		t.Helper()
		if f, r := listings(); f != full || r != readOnly {
			t.Fatalf("%s: the tool server listed its tools %d times and %d times read-only; want %d and %d", step, f, r, full, readOnly)
		}
	}
	list := func(cred string) {
		t.Helper()
		if a, _ := h.list(t, id, cred); a.status != http.StatusOK {
			t.Fatalf("GET tools = %d %s; want 200", a.status, a.body)
		}
	}

	list(h.key)
	list(h.key)
	want("two listings of the full set", 1, 0)
	first := h.tools.Listings()[0]
	if !slices.Equal(first.Command, []string{toolServerPath, "tools"}) || first.WorkDir != sandbox.WorkDir || len(first.Stdin) != 0 {
		t.Errorf("the definitions were read with %q in %q with %d bytes of input; want %q in %s without input",
			first.Command, first.WorkDir, len(first.Stdin), []string{toolServerPath, "tools"}, sandbox.WorkDir)
	}
	list(h.memberKey)
	list(h.memberKey)
	want("two listings of the read-only set", 1, 1)
	if l := h.tools.Listings()[1]; !slices.Equal(l.Command, []string{toolServerPath, "tools", "--read-only"}) {
		t.Errorf("the read-only definitions were read with %q; want --read-only", l.Command)
	}

	for _, move := range [][2]string{
		{sandbox.StateReady, sandbox.StateStopped}, {sandbox.StateStopped, sandbox.StateProvisioning}, {sandbox.StateProvisioning, sandbox.StateReady},
	} {
		if applied, err := h.svc.Transition(ctx, nil, id, move[0], move[1], "", nil); err != nil || !applied {
			t.Fatalf("move the sandbox from %s to %s: %v, %v", move[0], move[1], applied, err)
		}
	}
	list(h.key)
	list(h.key)
	want("after the sandbox left ready and came back", 2, 1)

	h.Restart(t)
	list(h.key)
	list(h.key)
	want("after the hub restarted", 3, 1)
}

func TestToolList_ChangesNothingAndNeedsNoPlaceAmongTheCommands(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "max_concurrent_execs = 1\n")
	h.cmds.set("block", h.cmds.blocking("", ""))
	id := h.tooled(t, nil)
	running := h.execing(t.Context(), id, map[string]any{"command": []string{"block"}}, nil)
	eventually(t, "the command to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 1 })
	before := storedRecord(t, h.Hub, id).LastActivityAt
	h.clock.Advance(5 * 1e9)

	for _, cred := range []string{h.key, h.memberKey, h.key} {
		if a, _ := h.list(t, id, cred); a.status != http.StatusOK {
			t.Fatalf("GET tools while a command runs = %d %s; want 200", a.status, a.body)
		}
	}
	if at := storedRecord(t, h.Hub, id).LastActivityAt; !at.Equal(before) {
		t.Errorf("last_activity_at = %s after the listings; want it left at %s", at, before)
	}
	if evs := h.sandboxEvents(t, "parley.sandbox.tool_call", id); len(evs) != 0 {
		t.Errorf("the listings left %d tool call events; want none", len(evs))
	}
	if _, body := h.Call(t, http.MethodGet, "/metrics", h.Token, nil); bytes.Contains(body, []byte("parley_sandbox_tool_calls_total{")) {
		t.Errorf("the listings were counted as tool calls:\n%s", body)
	}
	select {
	case a := <-running:
		t.Errorf("the command ended with %d %s while the tools were listed", a.status, a.body)
	default:
	}
}

func TestToolList_FailsWhenTheToolServerDoesAndKeepsNothingOfIt(t *testing.T) {
	t.Parallel()
	const secretValue = "tok_SECRETVALUE"
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	h.svc.SetValues(id, sandbox.Values{Exposed: []string{secretValue}})
	prints := func(stdout, stderr string, code int) func(context.Context, runtimetest.ToolListing, io.Writer, io.Writer) int {
		return func(_ context.Context, _ runtimetest.ToolListing, out, errOut io.Writer) int {
			_, _ = io.WriteString(out, stdout)
			_, _ = io.WriteString(errOut, stderr)
			return code
		}
	}
	good, _ := json.Marshal(runtimetest.ToolDefinitions(false))
	for _, row := range []struct {
		name string
		fn   func(context.Context, runtimetest.ToolListing, io.Writer, io.Writer) int
	}{
		{"cannot be started", prints("", "exec: no such file "+secretValue+" "+strings.Repeat("x", 3000), 127)},
		{"exits 1 after printing its tools", prints(string(good), "boom", 1)},
		{"exits 2", prints("", "usage", 2)},
		{"prints something that is not JSON", prints("not json", "", 0)},
		{"prints an object", prints(`{"ok":true}`, "", 0)},
		{"prints two arrays", prints(string(good)+string(good), "", 0)},
		{"prints a definition without a name", prints(`[{"description":"x","input_schema":{"type":"object"}}]`, "", 0)},
		{"prints a definition whose input schema is not an object", prints(`[{"name":"x","description":"x","input_schema":"any"}]`, "", 0)},
		{"prints more than 4 MiB", prints(`[{"name":"x","description":"`+strings.Repeat("d", 4<<20)+`","input_schema":{"type":"object"}}]`, "", 0)},
	} {
		t.Run("a tool server that "+row.name, func(t *testing.T) {
			h.tools.SetTools(row.fn)
			runs := len(h.tools.Listings())
			a, _ := h.list(t, id, h.key)
			if a.status != http.StatusBadGateway || a.err.ErrorType != "tool_server_failed" {
				t.Fatalf("GET tools = %d %.300s; want 502 tool_server_failed", a.status, a.body)
			}
			if msg := a.err.Message; len(msg) > 1024 || msg == "" || strings.Contains(msg, secretValue) {
				t.Errorf("the message has %d bytes and holds the secret: %v; want at most 1024 without it: %.200q", len(msg), strings.Contains(msg, secretValue), msg)
			}
			if n := len(h.tools.Listings()) - runs; n != 1 {
				t.Errorf("the tool server ran %d times; want once: a failed listing keeps nothing", n)
			}
		})
	}
	t.Run("a runtime that cannot be reached", func(t *testing.T) {
		h.tools.SetTools(nil)
		h.rt.FailOperation(runtime.OpExec, runtime.ErrUnavailable, "gateway gone "+secretValue)
		a, _ := h.list(t, id, h.key)
		if a.status != http.StatusBadGateway || a.err.ErrorType != "sandbox_runtime_error" || strings.Contains(a.err.Message, secretValue) {
			t.Errorf("GET tools = %d %.300s; want 502 sandbox_runtime_error without the secret", a.status, a.body)
		}
		h.rt.FailOperation(runtime.OpExec, nil, "")
	})
	t.Run("and then works", func(t *testing.T) {
		h.tools.SetTools(nil)
		if a, l := h.list(t, id, h.key); a.status != http.StatusOK || len(l.Tools) != 13 {
			t.Errorf("GET tools once the tool server works = %d %.300s; want 200 with thirteen tools", a.status, a.body)
		}
	})
	if log := h.Log.String(); strings.Contains(log, secretValue) {
		t.Errorf("the hub's log holds the secret value")
	}
}
