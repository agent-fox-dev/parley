package sandbox_test

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hubtest"

	"golang.org/x/net/websocket"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

func TestAttach_ServesAShellOverAWebSocket(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	id := h.sandbox(t, nil)

	t.Run("starts /bin/sh in the working directory on a terminal of the requested size", func(t *testing.T) {
		for _, c := range []struct {
			query string
			size  runtime.TerminalSize
		}{
			{"", runtime.TerminalSize{Cols: 80, Rows: 24}},
			{"cols=120&rows=40", runtime.TerminalSize{Cols: 120, Rows: 40}},
			{"cols=1&rows=1000", runtime.TerminalSize{Cols: 1, Rows: 1000}},
		} {
			ws := h.attach(t, id, c.query)
			term := h.terms.last(t, id)
			if !slices.Equal(term.req.Command, []string{"/bin/sh"}) || term.req.WorkDir != sandbox.WorkDir || term.size != c.size {
				t.Errorf("attach?%s opened %v in %q on %v, want [/bin/sh] in %s on %v", c.query, term.req.Command, term.req.WorkDir, term.size, sandbox.WorkDir, c.size)
			}
			// The runtime holds the sandbox's environment and lays the
			// request's over it; the hub adds nothing to it, so the shell
			// gets the sandbox's.
			if len(term.req.Env) != 0 {
				t.Errorf("attach?%s sent the environment %v, want the sandbox's alone", c.query, term.req.Env)
			}
			if spec, ok := h.rt.Spec("parley", id); !ok || spec.Env["PARLEY_SANDBOX_ID"] != id {
				t.Errorf("the runtime holds the environment %v for the sandbox (%v), want PARLEY_SANDBOX_ID=%s", spec.Env, ok, id)
			}
			_ = ws.Close()
		}
	})

	t.Run("copies bytes both ways in binary frames", func(t *testing.T) {
		ws := h.attach(t, id, "")
		term := h.terms.last(t, id)
		for _, in := range [][]byte{[]byte("echo hi\n"), {0xff, 0x00, 0x1b}} {
			sendFrame(t, ws, in)
			f, err := receiveWithin(ws, 5*time.Second)
			if err != nil || !f.binary || string(f.data) != string(in) {
				t.Errorf("sent %q, got %q (binary %v, err %v), want it back in a binary frame", in, f.data, f.binary, err)
			}
		}
		term.write(t, "out1")
		term.write(t, "out2")
		if got := receiveBytes(t, ws, 8); string(got) != "out1out2" {
			t.Errorf("the caller got %q, want out1out2", got)
		}
	})

	t.Run("resizes on a resize frame and ignores any other text frame", func(t *testing.T) {
		ws := h.attach(t, id, "")
		term := h.terms.last(t, id)
		want := []runtime.TerminalSize{{Cols: 100, Rows: 30}}
		sendFrame(t, ws, `{"resize":{"cols":100,"rows":30}}`)
		hubtest.Eventually(t, "the resize", func() bool { return slices.Equal(term.resized(), want) })
		for _, text := range []string{"hello", `{"resize":{"cols":0,"rows":30}}`, `{"resize":{"cols":100,"rows":1001}}`, `{"resize":{}}`, `{"other":1}`} {
			sendFrame(t, ws, text)
			sendFrame(t, ws, []byte("x"))
			if got := receiveBytes(t, ws, 1); string(got) != "x" {
				t.Errorf("after %s the caller got %q, want x", text, got)
			}
			if got := term.resized(); !slices.Equal(got, want) {
				t.Errorf("after %s the resizes are %v, want %v", text, got, want)
			}
		}
	})

	t.Run("ends the connection and closes the terminal on a frame over 1 MiB", func(t *testing.T) {
		ws := h.attach(t, id, "")
		term := h.terms.last(t, id)
		// The hub may close before the whole frame is written.
		go func() { _ = websocket.Message.Send(ws, make([]byte, 1<<20+1)) }()
		within(t, time.Second, "the terminal to close", func() bool { return term.closedByHub() && openTerminals(h.rt, id) == 0 })
		f, err := receiveWithin(ws, time.Second)
		var netErr net.Error
		if err == nil || (errors.As(err, &netErr) && netErr.Timeout()) {
			t.Errorf("the receive after the frame = %d bytes, %v; want the end of the connection within 1 s", len(f.data), err)
		}
	})

	for _, c := range []struct {
		name string
		end  func(*openedTerminal)
	}{
		{"closes the connection after the output once the shell exits", (*openedTerminal).exit},
		{"closes the connection after the output once the terminal fails", (*openedTerminal).fail},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := h.attach(t, id, "")
			term := h.terms.last(t, id)
			term.write(t, "bye")
			c.end(term)
			if got := receiveBytes(t, ws, 3); string(got) != "bye" {
				t.Errorf("the caller got %q, want bye", got)
			}
			f, err := receiveWithin(ws, time.Second)
			var netErr net.Error
			if err == nil || (errors.As(err, &netErr) && netErr.Timeout()) {
				t.Errorf("the receive after the output = %q, %v; want the end of the connection within 1 s", f.data, err)
			}
		})
	}
}

func TestAttach_EndsOnlyItsShellWhenTheCallerLeavesOrTheHubStops(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	h.cmds.set("block", h.cmds.blocking("", ""))
	id := h.sandbox(t, nil)
	h.startMain(t, id)
	ws1 := h.attach(t, id, "")
	ws2 := h.attach(t, id, "")
	terms := h.terms.of(id)
	first, second := terms[0], terms[1]
	ctx, cancel := context.WithCancel(context.Background())
	answered := h.execing(ctx, id, map[string]any{"command": []string{"block"}}, nil)
	defer func() {
		cancel()
		<-answered
	}()
	hubtest.Eventually(t, "the command to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 1 })

	_ = ws1.Close()
	within(t, time.Second, "the first terminal to close", func() bool { return first.closedByHub() && openTerminals(h.rt, id) == 1 })
	if second.closedByHub() {
		t.Error("the second terminal closed, want it open")
	}
	if _, _, waiting, kills := h.cmds.counts(); waiting != 1 || kills != 0 {
		t.Errorf("%d commands wait and %d were killed, want the exec still running", waiting, kills)
	}
	if !h.mainRunning(t, id) {
		t.Error("the main process ended, want it running")
	}
	sendFrame(t, ws2, []byte("y"))
	if got := receiveBytes(t, ws2, 1); string(got) != "y" {
		t.Errorf("the second terminal gave back %q, want y", got)
	}
	if st := storedRecord(t, h.Hub, id).State; st != sandbox.StateReady {
		t.Errorf("the sandbox is %s, want ready", st)
	}

	h.Stop()
	within(t, time.Second, "the second terminal to close once the hub stops", func() bool { return second.closedByHub() && openTerminals(h.rt, id) == 0 })
	if code := h.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0", code)
	}
	if !h.mainRunning(t, id) {
		t.Error("the main process ended, want it running")
	}
}

func TestAttach_RefusesARequestBeforeTheUpgrade(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	ready := h.sandbox(t, nil)
	stopped := h.sandbox(t, func(r *sandbox.Record) { r.State = sandbox.StateStopped })
	covered := h.sandbox(t, func(r *sandbox.Record) { r.HubAccess = &sandbox.HubAccess{Scopes: []string{"git:write"}} })
	narrow := h.pat(t, h.owner, "sandboxes:write", "sandboxes:read")
	upgrade := http.Header{
		"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
		"Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}, "Sec-Websocket-Version": {"13"},
	}
	// x/net's handshake reads only the first Connection header.
	secondConnection := maps.Clone(upgrade)
	secondConnection["Connection"] = []string{"keep-alive", "Upgrade"}
	// Serial subtests: each counts the terminals opened around its request.
	for _, c := range []struct {
		name, id, cred, query string
		header                http.Header
		fail                  bool
		status                int
		errorType             string
	}{
		{"a plain GET", ready, h.key, "", nil, false, 400, "invalid_request"},
		{"Upgrade only in a second Connection header", ready, h.key, "", secondConnection, false, 400, "invalid_request"},
		{"cols 0", ready, h.key, "cols=0", upgrade, false, 400, "invalid_request"},
		{"rows 1001", ready, h.key, "rows=1001", upgrade, false, 400, "invalid_request"},
		{"cols abc", ready, h.key, "cols=abc", upgrade, false, 400, "invalid_request"},
		{"a stopped sandbox", stopped, h.key, "", upgrade, false, 409, "sandbox_not_ready"},
		{"a PAT without the hub credential's git:write", covered, narrow, "", upgrade, false, 403, "insufficient_scope"},
		{"a terminal the runtime cannot open", ready, h.key, "", upgrade, true, 502, "sandbox_runtime_error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.fail {
				h.rt.FailOperation(runtime.OpExecInteractive, runtime.ErrUnavailable, "no terminal")
				defer h.rt.FailOperation(runtime.OpExecInteractive, nil, "")
			}
			opened := h.terms.count()
			var a answer
			a.status, a.header, a.body = h.Exchange(t, http.MethodGet, "/api/v1"+sandboxPath("ws", c.id)+"/attach?"+c.query, c.cred, nil, c.header)
			if a.status != c.status || errorObject(t, a)["error_type"] != c.errorType {
				t.Errorf("attach = %d %s, want %d %s", a.status, a.body, c.status, c.errorType)
			}
			if n := h.terms.count() - opened; n != 0 || openTerminals(h.rt, c.id) != 0 {
				t.Errorf("the hub opened %d terminals and %d are open, want none", n, openTerminals(h.rt, c.id))
			}
		})
	}
}
