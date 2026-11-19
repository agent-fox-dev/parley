package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// members are the sorted member names of the JSON object of v.
func members(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %+v: %v", v, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s is not an object: %v", raw, err)
	}
	return slices.Sorted(maps.Keys(obj))
}

func TestLogCollector_StoresWhatASandboxLogs(t *testing.T) {
	t.Parallel()
	h := startLogHub(t, 0)
	ctx := context.Background()

	t.Run("the main process's lines, in order, with the runtime's time", func(t *testing.T) {
		parts := make(chan string)
		h.cmds.set("talk", func(ctx context.Context, _ string, _ runtime.ExecRequest, out, _ io.Writer) int {
			for {
				select {
				case p := <-parts:
					_, _ = io.WriteString(out, p)
				case <-ctx.Done():
					return 0
				}
			}
		})
		id := h.sandbox(t, nil)
		h.follow(t, id)
		if err := h.rt.Client("parley", nil).RunMain(ctx, id, []string{"talk"}); err != nil {
			t.Fatalf("start the main process: %v", err)
		}
		// The runtime's time has a part of a second, which the store drops.
		h.clock.Advance(1500 * time.Millisecond)
		at := h.clock.Now()
		stored := h.svc.LogLinesStored()
		h.emit(id, runtime.Stdout, "one\n")
		h.emit(id, runtime.Stderr, "two\n")
		parts <- "no newline yet"
		hubtest.WaitClosed(t, stored, "the collector to say that it stored lines")
		eventually(t, "two lines", func() bool { return len(h.lines(t, id, "")) == 2 })
		parts <- " done\n"
		eventually(t, "three lines", func() bool { return len(h.lines(t, id, "")) >= 3 })

		lines := h.lines(t, id, "")
		want := []struct{ stream, line string }{{"stdout", "one"}, {"stderr", "two"}, {"stdout", "no newline yet done"}}
		if len(lines) != len(want) {
			t.Fatalf("lines = %+v, want %v", lines, want)
		}
		for i, w := range want {
			l := lines[i]
			if l.Source != sandbox.LogSourceProcess || l.Stream != w.stream || l.Line != w.line {
				t.Errorf("line %d = %+v, want process %s %q", i, l, w.stream, w.line)
			}
			if !l.Time.Equal(at.Truncate(time.Second)) || l.Time.Location() != time.UTC {
				t.Errorf("line %d has time %v, want %v in UTC", i, l.Time, at.Truncate(time.Second))
			}
		}
		if got := members(t, lines[0]); !slices.Equal(got, []string{"line", "source", "stream", "time"}) {
			t.Errorf("a process line has the members %v, want line, source, stream and time", got)
		}
	})

	t.Run("the policy log's entries", func(t *testing.T) {
		id := h.sandbox(t, nil)
		h.follow(t, id)
		h.rt.EmitPolicyLog("parley", id, runtime.LogEntry{Action: "deny", Program: "/usr/bin/curl", Host: "example.com", Port: 443})
		h.rt.EmitPolicyLog("parley", id, runtime.LogEntry{
			Text: "allow POST api.vendor.test:443/v1/messages", Action: "allow", Program: "/usr/bin/agent",
			Host: "api.vendor.test", Port: 443, Method: "POST", Path: "/v1/messages", Provider: "model",
		})
		eventually(t, "two policy lines", func() bool { return len(h.lines(t, id, sandbox.LogSourcePolicy)) == 2 })

		lines := h.lines(t, id, "")
		deny, allow := lines[0], lines[1]
		if deny.Source != sandbox.LogSourcePolicy || deny.Stream != "" || deny.Action != "deny" || deny.Program != "/usr/bin/curl" ||
			deny.Host != "example.com" || deny.Port != 443 || deny.Credential != "" || deny.Line != "deny example.com:443 /usr/bin/curl" {
			t.Errorf("the deny entry is stored as %+v", deny)
		}
		if got := members(t, deny); !slices.Equal(got, []string{"action", "host", "line", "port", "program", "source", "stream", "time"}) {
			t.Errorf("a policy line without a credential has the members %v", got)
		}
		if raw, _ := json.Marshal(deny); !strings.Contains(string(raw), `"stream":null`) {
			t.Errorf("a policy line is %s, want stream null", raw)
		}
		if allow.Action != "allow" || allow.Credential != "model" || allow.Line != "allow POST api.vendor.test:443/v1/messages" {
			t.Errorf("the credentialed entry is stored as %+v", allow)
		}
		if got := members(t, allow); !slices.Contains(got, "credential") {
			t.Errorf("a credentialed policy line has the members %v, want credential among them", got)
		}
	})

	t.Run("exposed secret values", func(t *testing.T) {
		const value = "npm_LONGSECRET1"
		id := h.sandbox(t, nil)
		h.svc.SetValues(id, sandbox.Values{Exposed: []string{value, "1234567"}})
		h.follow(t, id)
		h.emit(id, runtime.Stdout, "token="+value+" again "+value+"\n")
		h.emit(id, runtime.Stdout, "pin=1234567\n")
		h.rt.EmitPolicyLog("parley", id, runtime.LogEntry{
			Text: "allow GET registry.test/?t=" + value, Action: "allow", Program: "/bin/" + value,
			Host: value + ".registry.test", Port: 443, Provider: value,
		})
		eventually(t, "three lines", func() bool { return len(h.lines(t, id, "")) == 3 })

		process := h.lines(t, id, sandbox.LogSourceProcess)
		if len(process) != 2 || process[0].Line != "token=[redacted] again [redacted]" || process[1].Line != "pin=1234567" {
			t.Errorf("the process lines are %+v, want the long value redacted and the short one kept", process)
		}
		if policy := h.lines(t, id, sandbox.LogSourcePolicy); len(policy) != 1 || !strings.Contains(policy[0].Line, "[redacted]") {
			t.Errorf("the policy lines are %+v, want the value redacted", policy)
		}
		var n int
		err := h.Store.Main().QueryRowContext(ctx, `SELECT COUNT(*) FROM sandbox_logs WHERE instr(
			coalesce(source, '') || coalesce(stream, '') || coalesce(line, '') || coalesce(action, '') ||
			coalesce(program, '') || coalesce(host, '') || coalesce(credential, ''), ?) > 0`, value).Scan(&n)
		if err != nil || n != 0 {
			t.Errorf("%d stored rows hold the value (%v), want none", n, err)
		}
		if strings.Contains(h.Log.String(), value) {
			t.Error("the hub's log holds the value")
		}
	})

	for _, n := range []int{16384, 16385, 40000} {
		t.Run("a line of "+strconv.Itoa(n)+" bytes", func(t *testing.T) {
			line := strings.Repeat("0123456789abcdef", n/16+1)[:n]
			id := h.sandbox(t, nil)
			h.follow(t, id)
			h.emit(id, runtime.Stdout, line+"\n")
			pieces := (n + sandbox.MaxLogLine - 1) / sandbox.MaxLogLine
			eventually(t, "the line's pieces", func() bool { return len(h.lines(t, id, "")) >= pieces })
			got := h.texts(t, id)
			if len(got) != pieces {
				t.Fatalf("the line is stored as %d lines, want %d", len(got), pieces)
			}
			for i, p := range got {
				if len(p) > sandbox.MaxLogLine {
					t.Errorf("piece %d has %d bytes, more than %d", i, len(p), sandbox.MaxLogLine)
				}
			}
			if strings.Join(got, "") != line {
				t.Error("the pieces put together are not the line")
			}
		})
	}
}

func TestLogCollector_ReopensABrokenStreamAndResumesAfterARestart(t *testing.T) {
	t.Parallel()
	h := startLogHub(t, 0)
	s, s2 := h.sandbox(t, nil), h.sandbox(t, nil)
	h.follow(t, s)
	eventually(t, "the collector to read both logs of "+s2, func() bool { return h.logStreams(s2) == 2 })

	h.emit(s, runtime.Stdout, "a\n")
	h.emit(s, runtime.Stdout, "b\n")
	eventually(t, "a and b", func() bool { return len(h.lines(t, s, "")) == 2 })
	h.breakLog(t, s)
	eventually(t, "the collector to stop reading "+s, func() bool { return h.logStreams(s) == 0 })
	h.emit(s, runtime.Stdout, "c\n")
	h.emit(s2, runtime.Stdout, "x\n")
	eventually(t, "x of the other sandbox, without a round", func() bool { return slices.Equal(h.texts(t, s2), []string{"x"}) })
	h.round(t)
	eventually(t, "c after the stream was opened again", func() bool { return len(h.lines(t, s, "")) >= 3 })
	if got := h.texts(t, s); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("after the break the lines are %q, want a, b, c", got)
	}

	h.Stop()
	if code := h.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0", code)
	}
	h.clock.Advance(time.Second)
	h.emit(s, runtime.Stdout, "d\n")
	h.Restart(t)
	eventually(t, "d after the restart", func() bool { return len(h.lines(t, s, "")) >= 4 })
	h.follow(t, s2)
	if got := h.texts(t, s); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Errorf("after the restart the lines are %q, want a, b, c, d", got)
	}
	if got := h.texts(t, s2); !slices.Equal(got, []string{"x"}) {
		t.Errorf("after the restart the other sandbox's lines are %q, want x", got)
	}
}

func TestLogCollector_StopsReadingASandboxThatLeavesReady(t *testing.T) {
	t.Parallel()
	h := startLogHub(t, 0)
	ctx := context.Background()
	var left []string
	for _, state := range []string{sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed, sandbox.StateDeleted} {
		t.Run(state, func(t *testing.T) {
			id := h.sandbox(t, nil)
			h.follow(t, id)
			h.emit(id, runtime.Stdout, "kept\n")
			eventually(t, "the line", func() bool { return len(h.lines(t, id, "")) == 1 })
			if applied, err := h.svc.Transition(ctx, nil, id, sandbox.StateReady, state, "", nil); err != nil || !applied {
				t.Fatalf("Transition to %s = %t, %v", state, applied, err)
			}
			h.round(t)
			eventually(t, "the collector to stop reading "+id, func() bool { return h.logStreams(id) == 0 })
			h.emit(id, runtime.Stdout, "late\n")
			left = append(left, id)
		})
	}
	// A round that follows another sandbox has passed every sandbox above.
	h.follow(t, h.sandbox(t, nil))
	for _, id := range left {
		if got := h.texts(t, id); !slices.Equal(got, []string{"kept"}) || h.logStreams(id) != 0 {
			t.Errorf("sandbox %s has the lines %q and %d open log streams, want kept and none", id, got, h.logStreams(id))
		}
	}
}

func TestLogRetention_DeletesOldLinesAndLinesWithoutARecord(t *testing.T) {
	t.Parallel()
	h := startLogHub(t, 20*time.Millisecond)
	ctx := context.Background()
	id := h.sandbox(t, nil)
	gone := sandbox.NewID()
	now := h.clock.Now()
	const day = 24 * time.Hour
	err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		var lines []sandbox.LogLine
		for _, at := range []time.Time{now.Add(-8 * day), now.Add(-7*day - time.Second), now.Add(-7*day + time.Second), now} {
			lines = append(lines, sandbox.LogLine{Time: at, Source: sandbox.LogSourceProcess, Stream: "stdout", Line: "at " + at.String()})
		}
		if err := sandbox.InsertLogLines(ctx, tx, id, lines); err != nil {
			return err
		}
		return sandbox.InsertLogLines(ctx, tx, gone, []sandbox.LogLine{{Time: now, Source: sandbox.LogSourceProcess, Stream: "stdout", Line: "orphan"}})
	})
	if err != nil {
		t.Fatalf("store the lines: %v", err)
	}
	times := func() []time.Time {
		var out []time.Time
		for _, l := range h.lines(t, id, "") {
			out = append(out, l.Time)
		}
		return out
	}
	kept := []time.Time{now.Add(-7*day + time.Second), now}

	h.pass(t)
	if got := times(); !slices.EqualFunc(got, kept, time.Time.Equal) {
		t.Errorf("after a pass the lines have the times %v, want %v", got, kept)
	}
	if got := h.lines(t, gone, ""); len(got) != 0 {
		t.Errorf("the lines of a removed record are %+v, want none", got)
	}

	if applied, err := h.svc.Transition(ctx, nil, id, sandbox.StateReady, sandbox.StateDeleted, "", nil); err != nil || !applied {
		t.Fatalf("Transition to deleted = %t, %v", applied, err)
	}
	h.pass(t)
	if got := times(); !slices.EqualFunc(got, kept, time.Time.Equal) {
		t.Errorf("the lines of a deleted sandbox have the times %v after a pass, want %v", got, kept)
	}

	err = h.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := h.svc.RunPurgeHooks(ctx, tx, id); err != nil {
			return err
		}
		return sandbox.Remove(ctx, tx, id)
	})
	if err != nil {
		t.Fatalf("purge the sandbox: %v", err)
	}
	if got := h.lines(t, id, ""); len(got) != 0 {
		t.Errorf("after the purge the sandbox has the lines %+v, want none", got)
	}
}
