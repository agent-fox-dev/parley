package sandbox_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hubtest"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// eventStream is the Accept header of a request for the streamed form.
var eventStream = http.Header{"Accept": {"text/event-stream"}}

func TestExec_RunsTheCommandAndAnswersItsResult(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	h.cmds.set("echo", func(_ context.Context, _ string, req runtime.ExecRequest, stdout, _ io.Writer) int {
		_, _ = io.WriteString(stdout, strings.Join(req.Command[1:], " ")+"\n")
		return 0
	})
	h.cmds.set("fail", func(_ context.Context, _ string, _ runtime.ExecRequest, _, stderr io.Writer) int {
		_, _ = io.WriteString(stderr, "bad")
		return 3
	})
	h.cmds.set("cat", func(_ context.Context, _ string, req runtime.ExecRequest, stdout, _ io.Writer) int {
		_, _ = stdout.Write(req.Stdin)
		return 0
	})
	id := h.sandbox(t, nil)
	withMain := h.sandbox(t, nil)
	h.startMain(t, withMain)
	type sent struct{ dir, a, mode, stdin string }
	rows := []struct {
		name string
		// sandbox is the sandbox the command runs in; "" for id.
		sandbox string
		body    map[string]any
		want    map[string]any
		// sent is what the runtime must have been asked; nil checks nothing.
		sent *sent
	}{
		{"a command that prints and exits 0", "", map[string]any{"command": []string{"echo", "hi"}},
			map[string]any{"exit_code": 0.0, "stdout": "hi\n", "stderr": ""}, nil},
		{"a command that fails", "", map[string]any{"command": []string{"fail"}},
			map[string]any{"exit_code": 3.0, "stdout": "", "stderr": "bad"}, nil},
		{"a working directory and an environment", "", map[string]any{"command": []string{"true"}, "workdir": "/tmp", "env": map[string]string{"A": "1"}},
			map[string]any{"exit_code": 0.0}, &sent{dir: "/tmp", a: "1", mode: "profile"}},
		{"no working directory", "", map[string]any{"command": []string{"true"}},
			map[string]any{"exit_code": 0.0}, &sent{dir: sandbox.WorkDir, mode: "profile"}},
		{"a variable the sandbox has too", "", map[string]any{"command": []string{"true"}, "env": map[string]string{"MODE": "req"}},
			map[string]any{"exit_code": 0.0}, &sent{dir: sandbox.WorkDir, mode: "req"}},
		{"stdin", "", map[string]any{"command": []string{"cat"}, "stdin": "abc"},
			map[string]any{"exit_code": 0.0, "stdout": "abc"}, &sent{dir: sandbox.WorkDir, mode: "profile", stdin: "abc"}},
		{"stdin_base64", "", map[string]any{"command": []string{"true"}, "stdin_base64": base64.StdEncoding.EncodeToString([]byte{0x00, 0xff})},
			map[string]any{"exit_code": 0.0}, &sent{dir: sandbox.WorkDir, mode: "profile", stdin: "\x00\xff"}},
		{"a sandbox whose main process runs", withMain, map[string]any{"command": []string{"echo", "hi"}},
			map[string]any{"exit_code": 0.0, "stdout": "hi\n"}, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			target := id
			if row.sandbox != "" {
				target = row.sandbox
			}
			o := decoded(t, h.exec(t, target, row.body, nil), http.StatusOK)
			wantMembers(t, o, map[string]any{"truncated": false, "timed_out": false})
			wantMembers(t, o, row.want)
			for _, m := range []string{"stdout", "stderr"} {
				if _, ok := o[m].(string); !ok {
					t.Errorf("%s = %#v, want a string", m, o[m])
				}
				if _, ok := o[m+"_base64"]; ok {
					t.Errorf("the answer %v has %s_base64, want none for text", o, m)
				}
			}
			if d, ok := o["duration_ms"].(float64); !ok || d < 0 {
				t.Errorf("duration_ms = %#v, want a number of at least 0", o["duration_ms"])
			}
			if row.sent == nil {
				return
			}
			req := h.cmds.last(t)
			got := sent{dir: req.WorkDir, a: req.Env["A"], mode: req.Env["MODE"], stdin: string(req.Stdin)}
			if got != *row.sent {
				t.Errorf("the runtime ran %+v, want %+v", got, *row.sent)
			}
			if req.Env["PARLEY_SANDBOX_ID"] != target {
				t.Errorf("PARLEY_SANDBOX_ID = %q, want the sandbox's own %q", req.Env["PARLEY_SANDBOX_ID"], target)
			}
		})
	}
	if !h.mainRunning(t, withMain) {
		t.Error("the main process ended, want it running")
	}
}

func TestExec_RefusesAnInvalidBodyAndRunsNothing(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	id := h.sandbox(t, func(rec *sandbox.Record) { rec.ExposedSecrets = []string{"NPM_TOKEN"} })
	cmd := []string{"true"}
	b64 := func(n int) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, n)) }
	// oversized is a JSON body one byte over the 8 MiB limit.
	oversized := []byte(`{"command":["true"],"pad":"`)
	oversized = append(oversized, bytes.Repeat([]byte{'x'}, 8<<20+1-len(oversized)-2)...)
	oversized = append(oversized, `"}`...)
	rows := []struct {
		name   string
		body   any
		status int
	}{
		{"no command", map[string]any{}, 400},
		{"an empty command", map[string]any{"command": []string{}}, 400},
		{"a command that is a string", map[string]any{"command": "ls"}, 400},
		{"a command that is not a list of strings", map[string]any{"command": []any{1, 2}}, 400},
		{"a command of 257 arguments", map[string]any{"command": strs(257)}, 400},
		{"a command of 256 arguments", map[string]any{"command": strs(256)}, 200},
		{"an env of 65 entries", map[string]any{"command": cmd, "env": entries(65)}, 400},
		{"an env of 64 entries", map[string]any{"command": cmd, "env": entries(64)}, 200},
		{"an env value of 8193 bytes", map[string]any{"command": cmd, "env": map[string]string{"A": strings.Repeat("v", 8193)}}, 400},
		{"an env name with PARLEY_", map[string]any{"command": cmd, "env": map[string]string{"PARLEY_X": "1"}}, 400},
		{"an env name with OPENSHELL_", map[string]any{"command": cmd, "env": map[string]string{"OPENSHELL_X": "1"}}, 400},
		{"an env name the sandbox exposes", map[string]any{"command": cmd, "env": map[string]string{"NPM_TOKEN": "x"}}, 400},
		{"stdin and stdin_base64", map[string]any{"command": cmd, "stdin": "a", "stdin_base64": "YQ=="}, 400},
		{"a stdin_base64 that is not base64", map[string]any{"command": cmd, "stdin_base64": "***"}, 400},
		{"a stdin_base64 of 4194305 bytes", map[string]any{"command": cmd, "stdin_base64": b64(4194305)}, 400},
		{"a stdin_base64 of 4194304 bytes", map[string]any{"command": cmd, "stdin_base64": b64(4194304)}, 200},
		{"a stdin of 4194305 bytes", map[string]any{"command": cmd, "stdin": strings.Repeat("x", 4194305)}, 400},
		{"a timeout that is no duration", map[string]any{"command": cmd, "timeout": "soon"}, 400},
		{"a timeout of zero", map[string]any{"command": cmd, "timeout": "0s"}, 400},
		{"a timeout over the maximum", map[string]any{"command": cmd, "timeout": "31m"}, 400},
		{"a timeout at the maximum", map[string]any{"command": cmd, "timeout": "30m"}, 200},
		{"a body over 8 MiB", oversized, 413},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			before, _, _, _ := h.cmds.counts()
			a := h.exec(t, id, row.body, nil)
			ran, _, _, _ := h.cmds.counts()
			if a.status != row.status {
				t.Fatalf("exec = %d %.300s, want %d", a.status, a.body, row.status)
			}
			wantType, wantRan := "", 1
			switch row.status {
			case 400:
				wantType, wantRan = "invalid_request", 0
			case 413:
				wantType, wantRan = "payload_too_large", 0
			}
			if a.err.ErrorType != wantType {
				t.Errorf("error_type = %q, want %q", a.err.ErrorType, wantType)
			}
			if ran-before != wantRan {
				t.Errorf("the runtime ran %d commands, want %d", ran-before, wantRan)
			}
		})
	}
}

func TestExec_KillsACommandThatOutlastsItsTimeout(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	h.cmds.set("block", h.cmds.blocking("partial", "late"))
	id := h.sandbox(t, nil)
	for _, row := range []struct {
		name       string
		timeout    string
		limit      time.Duration
		killedCode bool
	}{
		{"the default timeout", "", time.Minute, false},
		{"the body's timeout", "2s", 2 * time.Second, false},
		{"a runtime that reports the killed command's code", "2s", 2 * time.Second, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			h.quirks.killedCode.Store(row.killedCode)
			body := map[string]any{"command": []string{"block"}}
			if row.timeout != "" {
				body["timeout"] = row.timeout
			}
			_, _, _, kills := h.cmds.counts()
			answered := h.execing(context.Background(), id, body, nil)
			hubtest.Eventually(t, "the command to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 1 })
			h.clock.Advance(row.limit - time.Second)
			select {
			case a := <-answered:
				t.Fatalf("exec answered %d %s before its timeout", a.status, a.body)
			case <-time.After(20 * time.Millisecond):
			}
			h.clock.Advance(time.Second)
			o := decoded(t, receive(t, answered), http.StatusOK)
			wantMembers(t, o, map[string]any{
				"timed_out": true, "exit_code": nil, "stdout": "partial", "stderr": "late",
				"truncated": false, "duration_ms": float64(row.limit.Milliseconds()),
			})
			if _, _, _, after := h.cmds.counts(); after-kills != 1 {
				t.Errorf("the runtime killed %d commands, want 1", after-kills)
			}
		})
	}
}

func TestExec_TruncatesAStreamOverOneMebibyte(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	id := h.sandbox(t, nil)
	const mib = 1 << 20
	rows := []struct {
		name           string
		stdout, stderr []byte
		truncated      bool
		lenOut, lenErr int
	}{
		{"a stdout of exactly 1 MiB", bytes.Repeat([]byte{'a'}, mib), nil, false, mib, 0},
		{"a stdout one byte over", bytes.Repeat([]byte{'a'}, mib+1), nil, true, mib, 0},
		{"a stderr far over", []byte("0123456789"), bytes.Repeat([]byte{'b'}, 2000000), true, 10, mib},
		{"a cut inside a character", append(bytes.Repeat([]byte{'a'}, mib-1), "ézz"...), nil, true, mib - 1, 0},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h.cmds.set("emit", func(_ context.Context, _ string, _ runtime.ExecRequest, stdout, stderr io.Writer) int {
				// In pieces, as a command writes.
				for _, w := range []struct {
					to   io.Writer
					data []byte
				}{{stdout, row.stdout}, {stderr, row.stderr}} {
					for b := w.data; len(b) > 0; {
						n := min(len(b), 64<<10)
						_, _ = w.to.Write(b[:n])
						b = b[n:]
					}
				}
				return 5
			})
			o := decoded(t, h.exec(t, id, map[string]any{"command": []string{"emit"}}, nil), http.StatusOK)
			wantMembers(t, o, map[string]any{"truncated": row.truncated, "exit_code": 5.0})
			out, _ := o["stdout"].(string)
			errOut, _ := o["stderr"].(string)
			if len(out) != row.lenOut || len(errOut) != row.lenErr {
				t.Errorf("returned %d bytes of stdout and %d of stderr, want %d and %d", len(out), len(errOut), row.lenOut, row.lenErr)
			}
			if !bytes.HasPrefix(row.stdout, []byte(out)) || !bytes.HasPrefix(row.stderr, []byte(errOut)) {
				t.Error("a returned stream is not a prefix of what the command wrote")
			}
		})
	}
}

func TestExec_AnswersAStreamThatIsNotUTF8InBase64(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	id := h.sandbox(t, nil)
	binary := []byte{0xff, 0xfe, 0x00}
	for _, row := range []struct {
		name         string
		binary, text string // the members of the binary and the text stream
	}{
		{"binary stdout", "stdout", "stderr"},
		{"binary stderr", "stderr", "stdout"},
	} {
		t.Run(row.name, func(t *testing.T) {
			h.cmds.set("bin", func(_ context.Context, _ string, _ runtime.ExecRequest, stdout, stderr io.Writer) int {
				streams := map[string]io.Writer{"stdout": stdout, "stderr": stderr}
				_, _ = streams[row.binary].Write(binary)
				_, _ = io.WriteString(streams[row.text], "ok")
				return 0
			})
			o := decoded(t, h.exec(t, id, map[string]any{"command": []string{"bin"}}, nil), http.StatusOK)
			wantMembers(t, o, map[string]any{row.binary: "", row.text: "ok"})
			encoded, _ := o[row.binary+"_base64"].(string)
			if got, err := base64.StdEncoding.DecodeString(encoded); err != nil || !bytes.Equal(got, binary) {
				t.Errorf("%s_base64 = %q, want the base64 of % x", row.binary, encoded, binary)
			}
			if _, ok := o[row.text+"_base64"]; ok {
				t.Errorf("the answer has %s_base64, want none for text", row.text)
			}
		})
	}
}

func TestExec_KillsTheCommandWhenTheCallerLeavesOrTheHubStops(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	h.cmds.set("block", h.cmds.blocking("", ""))
	id := h.sandbox(t, nil)
	h.startMain(t, id)
	block := map[string]any{"command": []string{"block"}}

	ctx, cancel := context.WithCancel(context.Background())
	answered := h.execing(ctx, id, block, nil)
	hubtest.Eventually(t, "the command to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 1 })
	cancel()
	left := time.Now()
	for _, _, _, kills := h.cmds.counts(); kills != 1; _, _, _, kills = h.cmds.counts() {
		if time.Since(left) > time.Second {
			t.Fatalf("the runtime killed %d commands 1 s after the caller left, want 1", kills)
		}
		time.Sleep(2 * time.Millisecond)
	}
	<-answered
	if !h.mainRunning(t, id) {
		t.Error("the main process ended, want it running")
	}
	if a := h.exec(t, id, map[string]any{"command": []string{"true"}}, nil); a.status != http.StatusOK {
		t.Errorf("the next exec = %d %s, want 200", a.status, a.body)
	}

	answered = h.execing(context.Background(), id, block, nil)
	hubtest.Eventually(t, "the command to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 1 })
	h.Stop()
	if code := h.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0", code)
	}
	if a := receive(t, answered); a.status != 0 {
		t.Errorf("the caller got %d %s, want no answer", a.status, a.body)
	}
	if _, _, _, kills := h.cmds.counts(); kills != 2 {
		t.Errorf("the runtime killed %d commands, want 2", kills)
	}
	if !h.mainRunning(t, id) {
		t.Error("the main process ended, want it running")
	}
}

func TestExec_RefusesACommandOverTheSandboxLimit(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 2)
	h.cmds.set("block", h.cmds.blocking("", ""))
	id, other := h.sandbox(t, nil), h.sandbox(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	block := map[string]any{"command": []string{"block"}}
	collected, streamed := h.execing(ctx, id, block, nil), h.execing(ctx, id, block, eventStream)
	hubtest.Eventually(t, "both commands to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 2 })
	defer func() {
		cancel()
		<-collected
		<-streamed
	}()

	a := h.exec(t, id, map[string]any{"command": []string{"true"}}, nil)
	if a.status != http.StatusTooManyRequests || a.err.ErrorType != "rate_limited" || a.header.Get("Retry-After") != "" {
		t.Errorf("a third exec = %d %s (Retry-After %q), want 429 rate_limited without Retry-After", a.status, a.body, a.header.Get("Retry-After"))
	}
	if ran, _, _, _ := h.cmds.counts(); ran != 2 {
		t.Errorf("the runtime ran %d commands, want 2", ran)
	}
	if a := h.exec(t, other, map[string]any{"command": []string{"true"}}, nil); a.status != http.StatusOK {
		t.Errorf("an exec in another sandbox = %d %s, want 200", a.status, a.body)
	}
	a = h.exec(t, id, map[string]any{"command": []string{"true"}}, eventStream)
	if a.status != http.StatusTooManyRequests || a.err.ErrorType != "rate_limited" || !strings.HasPrefix(a.header.Get("Content-Type"), "application/json") {
		t.Errorf("a third streamed exec = %d %s (Content-Type %q), want 429 rate_limited as JSON", a.status, a.body, a.header.Get("Content-Type"))
	}
}

func TestExec_StopsCountingACommandOnceItEnded(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 1)
	h.cmds.set("block", h.cmds.blocking("", ""))
	h.cmds.set("nine", func(context.Context, string, runtime.ExecRequest, io.Writer, io.Writer) int { return 9 })
	id := h.sandbox(t, nil)
	h.startMain(t, id)
	terminal, err := h.rt.Client("parley", nil).ExecInteractive(context.Background(), id, runtime.ExecRequest{Command: []string{"/bin/sh"}}, runtime.TerminalSize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("open a terminal: %v", err)
	}
	defer func() { _ = terminal.Close() }()
	next := func(t *testing.T) {
		t.Helper()
		if a := h.exec(t, id, map[string]any{"command": []string{"true"}}, nil); a.status != http.StatusOK {
			t.Errorf("the next exec = %d %s, want 200", a.status, a.body)
		}
	}
	t.Run("beside the main process and a terminal", next)
	// waitBlocked waits until a command of block waits.
	waitBlocked := func(t *testing.T) {
		hubtest.Eventually(t, "the command to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 1 })
	}
	for _, row := range []struct {
		name string
		end  func(t *testing.T)
	}{
		{"after an exit 0", func(t *testing.T) { next(t) }},
		{"after an exit 9", func(t *testing.T) {
			wantMembers(t, decoded(t, h.exec(t, id, map[string]any{"command": []string{"nine"}}, nil), http.StatusOK), map[string]any{"exit_code": 9.0})
		}},
		{"after a timeout", func(t *testing.T) {
			answered := h.execing(context.Background(), id, map[string]any{"command": []string{"block"}, "timeout": "3s"}, nil)
			waitBlocked(t)
			h.clock.Advance(3 * time.Second)
			wantMembers(t, decoded(t, receive(t, answered), http.StatusOK), map[string]any{"timed_out": true})
		}},
		{"after the caller left", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			answered := h.execing(ctx, id, map[string]any{"command": []string{"block"}}, nil)
			waitBlocked(t)
			cancel()
			<-answered
			// The hub releases the place after the caller saw the
			// connection end; ask until it has.
			hubtest.Eventually(t, "an exec to be accepted", func() bool {
				return h.exec(t, id, map[string]any{"command": []string{"true"}}, nil).status == http.StatusOK
			})
		}},
		{"after the runtime failed", func(t *testing.T) {
			answered := h.execing(context.Background(), id, map[string]any{"command": []string{"block"}}, nil)
			waitBlocked(t)
			for _, s := range h.rt.Streams() {
				if s.Op == runtime.OpExecStream && s.Sandbox == id {
					h.rt.BreakStream(s.ID)
				}
			}
			a := receive(t, answered)
			if a.status != http.StatusBadGateway || !strings.Contains(string(a.body), "sandbox_runtime_error") {
				t.Errorf("exec = %d %s, want 502 sandbox_runtime_error", a.status, a.body)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			row.end(t)
			next(t)
		})
	}
}

// receive waits up to 5 s for an answer on answered.
func receive(t *testing.T, answered <-chan answer) answer {
	t.Helper()
	select {
	case a := <-answered:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("waited 5 s for the answer")
		return answer{}
	}
}

func TestExec_StreamsTheOutputWhileTheCommandRuns(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	goOn := make(chan struct{})
	h.cmds.set("talk", func(ctx context.Context, _ string, _ runtime.ExecRequest, stdout, stderr io.Writer) int {
		_, _ = io.WriteString(stdout, "a")
		select {
		case <-goOn:
		case <-ctx.Done():
			return 137
		}
		_, _ = io.WriteString(stderr, "b")
		_, _ = stdout.Write(bytes.Repeat([]byte{'c'}, 100000))
		return 4
	})
	id := h.sandbox(t, nil)
	s := h.streaming(t, context.Background(), id, map[string]any{"command": []string{"talk"}})
	if got := s.resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	first, ok := s.next(t)
	if !ok || first.event != "stdout" || !reflect.DeepEqual(first.data, map[string]any{"data": "a"}) {
		t.Fatalf("the first frame = %+v (%v), want stdout {data: a}", first, ok)
	}
	if _, running, _, _ := h.cmds.counts(); running != 1 {
		t.Fatalf("%d commands run after the first frame, want the command still running", running)
	}
	close(goOn)
	frames := append([]frame{first}, s.rest(t)...)
	last := frames[len(frames)-1]
	if last.event != "exit" {
		t.Fatalf("the last frame is %q, want exit", last.event)
	}
	wantMembers(t, last.data, map[string]any{"exit_code": 4.0, "timed_out": false})
	if _, ok := last.data["duration_ms"].(float64); !ok {
		t.Errorf("the exit frame %v has no duration_ms", last.data)
	}
	var stdout, stderr []byte
	var order []string
	for _, f := range frames[:len(frames)-1] {
		b := f.output(t)
		if len(b) > 32768 {
			t.Errorf("a %s frame carries %d bytes, want at most 32768", f.event, len(b))
		}
		switch f.event {
		case "stdout":
			stdout = append(stdout, b...)
		case "stderr":
			stderr = append(stderr, b...)
		default:
			t.Errorf("a frame before the last is %q, want stdout or stderr", f.event)
		}
		if len(order) == 0 || order[len(order)-1] != f.event {
			order = append(order, f.event)
		}
	}
	if want := "a" + strings.Repeat("c", 100000); string(stdout) != want {
		t.Errorf("the stdout frames carry %d bytes, want the %d the command wrote", len(stdout), len(want))
	}
	if string(stderr) != "b" {
		t.Errorf("the stderr frames carry %q, want b", stderr)
	}
	if !slices.Equal(order, []string{"stdout", "stderr", "stdout"}) {
		t.Errorf("the frames came as %v, want stdout, stderr, stdout, in the order of the output", order)
	}
}

func TestExec_StreamsEveryByteOfTheOutput(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	id := h.sandbox(t, nil)
	pattern := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = "0123456789abcdefghijklmnopqrstuvwxyz\n"[i%37]
		}
		return b
	}
	rows := []struct {
		name string
		// stdout and stderr are the chunks the command writes.
		stdout, stderr [][]byte
		// encoded is the index of the stdout frame that must carry
		// data_base64; -1 for none.
		encoded int
	}{
		{"3 MiB of stdout and 2 MiB of stderr", [][]byte{pattern(3 << 20)}, [][]byte{pattern(2 << 20)}, -1},
		{"a chunk that is not UTF-8", [][]byte{[]byte("ok"), {0xff, 0x00, 0xfe}, []byte("done")}, nil, 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h.cmds.set("emit", func(_ context.Context, _ string, _ runtime.ExecRequest, stdout, stderr io.Writer) int {
				for _, c := range row.stdout {
					_, _ = stdout.Write(c)
				}
				for _, c := range row.stderr {
					_, _ = stderr.Write(c)
				}
				return 0
			})
			frames := h.streaming(t, context.Background(), id, map[string]any{"command": []string{"emit"}}).rest(t)
			if len(frames) == 0 || frames[len(frames)-1].event != "exit" {
				t.Fatalf("the stream ended without an exit frame")
			}
			if _, ok := frames[len(frames)-1].data["truncated"]; ok {
				t.Error("the exit frame has a truncated member, want none")
			}
			got := map[string][]byte{}
			n := 0
			for _, f := range frames[:len(frames)-1] {
				got[f.event] = append(got[f.event], f.output(t)...)
				if f.event != "stdout" {
					continue
				}
				_, text := f.data["data"]
				_, encoded := f.data["data_base64"]
				if want := n == row.encoded; encoded != want || text == want {
					t.Errorf("stdout frame %d = %v, want data_base64 %v and data %v", n, f.data, want, !want)
				}
				n++
			}
			for name, chunks := range map[string][][]byte{"stdout": row.stdout, "stderr": row.stderr} {
				if want := bytes.Join(chunks, nil); sha256.Sum256(got[name]) != sha256.Sum256(want) {
					t.Errorf("the %s frames carry %d bytes that differ from the %d the command wrote", name, len(got[name]), len(want))
				}
			}
		})
	}
}

func TestExec_StreamKillsACommandThatOutlastsItsTimeout(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	h.cmds.set("block", h.cmds.blocking("partial", ""))
	id := h.sandbox(t, nil)
	for _, row := range []struct {
		name       string
		killedCode bool
	}{
		{"a runtime that reports a killed command without a code", false},
		{"a runtime that reports the killed command's code", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			h.quirks.killedCode.Store(row.killedCode)
			_, _, _, before := h.cmds.counts()
			s := h.streaming(t, context.Background(), id, map[string]any{"command": []string{"block"}, "timeout": "2s"})
			if f, ok := s.next(t); !ok || f.event != "stdout" || string(f.output(t)) != "partial" {
				t.Fatalf("the first frame = %+v (%v), want stdout partial", f, ok)
			}
			h.clock.Advance(2 * time.Second)
			frames := s.rest(t)
			if len(frames) != 1 || frames[0].event != "exit" {
				t.Fatalf("after the timeout the stream sent %+v, want one exit frame", frames)
			}
			wantMembers(t, frames[0].data, map[string]any{"exit_code": nil, "timed_out": true, "duration_ms": 2000.0})
			if _, _, _, kills := h.cmds.counts(); kills-before != 1 {
				t.Errorf("the runtime killed %d commands, want 1", kills-before)
			}
		})
	}
}

func TestExec_StreamKillsTheCommandWhenTheCallerLeavesOrTheHubStops(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 1)
	h.cmds.set("block", h.cmds.blocking("x", ""))
	id := h.sandbox(t, nil)
	h.startMain(t, id)
	block := map[string]any{"command": []string{"block"}}
	// open opens a stream of block and reads its first frame.
	open := func(t *testing.T) *frameReader {
		t.Helper()
		s := h.streaming(t, context.Background(), id, block)
		if f, ok := s.next(t); !ok || f.event != "stdout" {
			t.Fatalf("the first frame = %+v (%v), want stdout", f, ok)
		}
		return s
	}

	_ = open(t).resp.Body.Close()
	left := time.Now()
	for _, _, _, kills := h.cmds.counts(); kills != 1; _, _, _, kills = h.cmds.counts() {
		if time.Since(left) > time.Second {
			t.Fatalf("the runtime killed %d commands 1 s after the caller left, want 1", kills)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The one place of the sandbox is free again once the hub saw the
	// caller leave.
	hubtest.Eventually(t, "an exec to be accepted", func() bool {
		return h.exec(t, id, map[string]any{"command": []string{"true"}}, nil).status == http.StatusOK
	})

	s := open(t)
	h.Stop()
	if code := h.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0", code)
	}
	for _, f := range s.rest(t) {
		if f.event == "exit" {
			t.Errorf("the stream sent an exit frame %v while the hub stopped, want none", f.data)
		}
	}
	if _, _, _, kills := h.cmds.counts(); kills != 2 {
		t.Errorf("the runtime killed %d commands, want 2", kills)
	}
	if !h.mainRunning(t, id) {
		t.Error("the main process ended, want it running")
	}
}

func TestExec_StreamRefusesARequestAsTheCollectedFormDoes(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	ha := &sandbox.HubAccess{Scopes: []string{"git:write"}}
	ready := h.sandbox(t, func(rec *sandbox.Record) { rec.HubAccess = ha })
	stopped := h.sandbox(t, func(rec *sandbox.Record) { rec.State = sandbox.StateStopped })
	narrow := h.pat(t, h.owner, "sandboxes:write", "sandboxes:read")
	cmd := []string{"true"}
	rows := []struct {
		name, cred, id string
		body           map[string]any
		status         int
		errorType      string
	}{
		{"a body without a command", h.key, ready, map[string]any{}, 400, "invalid_request"},
		{"a timeout over the maximum", h.key, ready, map[string]any{"command": cmd, "timeout": "31m"}, 400, "invalid_request"},
		{"a stopped sandbox", h.key, stopped, map[string]any{"command": cmd}, 409, "sandbox_not_ready"},
		{"a PAT that does not cover the sandbox's credential", narrow, ready, map[string]any{"command": cmd}, 403, "insufficient_scope"},
		{"an unknown sandbox", h.key, sandbox.NewID(), map[string]any{"command": cmd}, 404, "sandbox_not_found"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for _, form := range []struct {
				name   string
				header http.Header
			}{{"collected", nil}, {"streamed", eventStream}} {
				a := h.execAs(t, row.cred, row.id, row.body, form.header)
				if a.status != row.status || a.err.ErrorType != row.errorType || !strings.HasPrefix(a.header.Get("Content-Type"), "application/json") {
					t.Errorf("the %s form = %d %s (Content-Type %q), want %d %s as JSON", form.name, a.status, a.body, a.header.Get("Content-Type"), row.status, row.errorType)
				}
			}
			if ran, _, _, _ := h.cmds.counts(); ran != 0 {
				t.Errorf("the runtime ran %d commands, want none", ran)
			}
		})
	}
}

func TestExec_StreamEndsWithoutAnExitFrameWhenTheRuntimeFails(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 1)
	h.cmds.set("block", h.cmds.blocking("x", ""))
	id := h.sandbox(t, nil)
	s := h.streaming(t, context.Background(), id, map[string]any{"command": []string{"block"}})
	first, ok := s.next(t)
	if !ok || first.event != "stdout" || string(first.output(t)) != "x" {
		t.Fatalf("the first frame = %+v (%v), want stdout x", first, ok)
	}
	for _, st := range h.rt.Streams() {
		if st.Op == runtime.OpExecStream && st.Sandbox == id {
			h.rt.BreakStream(st.ID)
		}
	}
	if rest := s.rest(t); len(rest) != 0 {
		t.Errorf("after the runtime failed the stream sent %+v, want it to end", rest)
	}
	if a := h.exec(t, id, map[string]any{"command": []string{"true"}}, nil); a.status != http.StatusOK {
		t.Errorf("the next exec = %d %s, want 200", a.status, a.body)
	}
}
