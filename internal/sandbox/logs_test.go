package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
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

func TestLogs_ReadsTheStoredLines(t *testing.T) {
	t.Parallel()
	h := startUseSandboxHub(t, "")
	id := h.sandbox(t, nil)
	h.seedLines(t, id)
	all := []string{"p1", "d1", "p2", "p3"}

	t.Run("in pages, oldest first", func(t *testing.T) {
		a := h.logsAs(t, h.key, id, "")
		if lines, next := logPage(t, a); !slices.Equal(lines, all) || next != "" {
			t.Fatalf("logs = %q with next_cursor %q, want %q and null", lines, next, all)
		}
		var page struct{ Items []map[string]any }
		if err := json.Unmarshal(a.body, &page); err != nil {
			t.Fatalf("decode %s: %v", a.body, err)
		}
		p1, d1 := page.Items[0], page.Items[1]
		if p1["time"] != "2026-11-18T10:00:00Z" || p1["source"] != "process" || p1["stream"] != "stdout" {
			t.Errorf("the first item is %v, want a stdout process line at 10:00:00", p1)
		}
		if d1["source"] != "policy" || d1["stream"] != nil || d1["action"] != "deny" || d1["host"] != "example.com" {
			t.Errorf("the second item is %v, want the policy entry", d1)
		}

		lines, next := logPage(t, h.logsAs(t, h.key, id, "limit=3"))
		if !slices.Equal(lines, all[:3]) || next == "" {
			t.Fatalf("limit=3 = %q with next_cursor %q, want %q and a cursor", lines, next, all[:3])
		}
		if lines, next = logPage(t, h.logsAs(t, h.key, id, "limit=3&cursor="+url.QueryEscape(next))); !slices.Equal(lines, all[3:]) || next != "" {
			t.Errorf("the next page = %q with next_cursor %q, want [p3] and null", lines, next)
		}

		if a := h.logsAs(t, h.key, h.sandbox(t, nil), ""); a.status != http.StatusOK || !strings.Contains(string(a.body), `"items":[]`) {
			t.Errorf("the logs of a sandbox without lines = %d %s, want 200 with items []", a.status, a.body)
		}
		for _, q := range []string{"limit=0", "limit=201"} {
			if a := h.logsAs(t, h.key, id, q); a.status != http.StatusBadRequest {
				t.Errorf("%s = %d %s, want 400", q, a.status, a.body)
			}
		}
	})

	t.Run("selected by source, since and tail", func(t *testing.T) {
		for _, tc := range []struct {
			query string
			pages [][]string
		}{
			{"source=process", [][]string{{"p1", "p2", "p3"}}},
			{"source=policy", [][]string{{"d1"}}},
			{"since=2026-11-18T10:00:02Z", [][]string{{"p2", "p3"}}},
			{"since=2026-11-18T11:00:02%2B01:00", [][]string{{"p2", "p3"}}},
			{"since=2026-11-18T10:00:01.5Z", [][]string{{"p2", "p3"}}},
			{"since=2026-11-18T10:00:04Z", [][]string{nil}},
			{"tail=2", [][]string{{"p2", "p3"}}},
			{"tail=99", [][]string{all}},
			{"source=process&tail=2", [][]string{{"p2", "p3"}}},
			{"source=process&since=2026-11-18T10:00:01Z&tail=1", [][]string{{"p3"}}},
			{"tail=3&limit=2", [][]string{{"d1", "p2"}, {"p3"}}},
		} {
			t.Run(tc.query, func(t *testing.T) {
				var got [][]string
				lines, next := logPage(t, h.logsAs(t, h.key, id, tc.query))
				got = append(got, lines)
				for next != "" && len(got) <= len(tc.pages) {
					lines, next = logPage(t, h.logsAs(t, h.key, id, tc.query+"&cursor="+url.QueryEscape(next)))
					got = append(got, lines)
				}
				if !reflect.DeepEqual(got, tc.pages) {
					t.Errorf("the pages are %q, want %q", got, tc.pages)
				}
			})
		}
	})

	t.Run("refuses an invalid query and opens no stream", func(t *testing.T) {
		for _, q := range []string{
			"source=runtime", "since=yesterday", "tail=0", "tail=-1", "tail=x", "follow=yes",
			"follow=true&cursor=abc", "follow=true&limit=10",
		} {
			a := h.logsAs(t, h.key, id, q)
			if a.status != http.StatusBadRequest || a.err.ErrorType != "invalid_request" ||
				!strings.HasPrefix(a.header.Get("Content-Type"), "application/json") {
				t.Errorf("%s = %d %s (Content-Type %q), want 400 invalid_request as JSON", q, a.status, a.body, a.header.Get("Content-Type"))
			}
		}
	})

	t.Run("from the hub's store in every state, without the runtime", func(t *testing.T) {
		ha := &sandbox.HubAccess{Scopes: []string{"git:write"}}
		// The credential holds both sandbox scopes but not the sandbox's
		// hub credential: it may read the logs and act in no sandbox.
		narrow := h.pat(t, h.owner, "sandboxes:read", "sandboxes:write")
		for _, state := range []string{
			sandbox.StateProvisioning, sandbox.StateReady, sandbox.StateStopped,
			sandbox.StateCompleted, sandbox.StateFailed, sandbox.StateDeleted,
		} {
			sid := h.sandbox(t, func(r *sandbox.Record) {
				r.State, r.HubAccess = state, ha
				if state != sandbox.StateProvisioning && state != sandbox.StateReady {
					r.EndedAt = &r.CreatedAt
				}
			})
			h.seedLines(t, sid)
			if state == sandbox.StateReady {
				if a := h.execAs(t, narrow, sid, map[string]any{"command": []string{"true"}}, nil); a.status != http.StatusForbidden {
					t.Fatalf("an exec with the narrow credential = %d %s, want 403", a.status, a.body)
				}
			}
			for _, reachable := range []bool{true, false} {
				t.Run(state+", runtime reachable "+strconv.FormatBool(reachable), func(t *testing.T) {
					h.rt.SetUnreachable(!reachable)
					defer h.rt.SetUnreachable(false)
					before := requestCalls(h.rt)
					if lines, _ := logPage(t, h.logsAs(t, narrow, sid, "")); !slices.Equal(lines, all) {
						t.Errorf("logs = %q, want %q", lines, all)
					}
					if n := requestCalls(h.rt) - before; n != 0 {
						t.Errorf("the read made %d runtime calls, want none", n)
					}
				})
			}
		}
	})
}

// startFollowHub starts a log hub without the provisioner, whose timers of
// a second would otherwise share the clock with those of the followed log
// streams.
func startFollowHub(t *testing.T) *logHub {
	t.Helper()
	h := &logHub{}
	h.execHub = newExecHub(t, "", func(cfg *hubtest.Config) {
		compose := cfg.Compose
		cfg.Compose = func(c *hub.Composition) {
			compose(c)
			c.AdjustSandbox(func(d *sandbox.Deps) { d.HoldProvisioning = true })
		}
	})
	return h
}

func TestLogs_FollowsTheStoredLines(t *testing.T) {
	t.Parallel()
	h := startFollowHub(t)
	ctx := context.Background()
	// waits is the number of times the followed streams have waited for
	// new lines; poll has the stream that waits the n-th time read the
	// store, as its timer does.
	waits := func() int { return h.timers.created(sandbox.LogFollowInterval) }
	poll := func(n int) {
		t.Helper()
		eventually(t, "the stream to wait for new lines", func() bool { return waits() >= n })
		h.clock.Advance(sandbox.LogFollowInterval)
	}
	wantFrame := func(t *testing.T, s *frameReader, line string) frame {
		t.Helper()
		f, ok := s.next(t)
		if !ok || f.event != "log" || f.data["line"] != line {
			t.Fatalf("the next frame is %+v (stream open %t), want a log frame of %s", f, ok, line)
		}
		return f
	}
	wantEnd := func(t *testing.T, s *frameReader) {
		t.Helper()
		start := time.Now()
		if rest := s.rest(t); len(rest) != 0 {
			t.Errorf("the stream sent %+v more, want it to end", rest)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("the stream took %v to end, want at most 2 s", took)
		}
	}

	t.Run("sends the selected lines, then each new one", func(t *testing.T) {
		id := h.sandbox(t, nil)
		h.seedLines(t, id)
		base := waits()
		s := h.followLogs(t, ctx, id, "follow=true&source=process&tail=1")
		if cc := s.resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", cc)
		}
		wantFrame(t, s, "p3")

		// The tail placed only the first lines: both new lines are sent.
		h.storeLines(t, id, policyLine(4, "d2"), processLine(4, "stdout", "p4"), processLine(4, "stderr", "p5"))
		poll(base + 1)
		f := wantFrame(t, s, "p4")
		if got := members(t, f.data); !slices.Equal(got, []string{"line", "source", "stream", "time"}) {
			t.Errorf("the frame's data has the members %v, want line, source, stream and time", got)
		}
		wantFrame(t, s, "p5")

		// A line the collector stores reaches the stream without its timer.
		eventually(t, "the stream to wait for new lines", func() bool { return waits() >= base+2 })
		h.follow(t, id)
		h.emit(id, runtime.Stdout, "p6\n")
		wantFrame(t, s, "p6")

		if applied, err := h.svc.Transition(ctx, nil, id, sandbox.StateReady, sandbox.StateDeleted, "", nil); err != nil || !applied {
			t.Fatalf("Transition to deleted = %t, %v", applied, err)
		}
		wantEnd(t, s)
	})

	t.Run("ends once the sandbox is deleted or the hub stops", func(t *testing.T) {
		id := h.sandbox(t, nil)
		h.seedLines(t, id)
		base := waits()
		s := h.followLogs(t, ctx, id, "follow=true")
		for _, line := range []string{"p1", "d1", "p2", "p3"} {
			wantFrame(t, s, line)
		}

		eventually(t, "the stream to wait for new lines", func() bool { return waits() >= base+1 })
		if applied, err := h.svc.Transition(ctx, nil, id, sandbox.StateReady, sandbox.StateStopped, "", nil); err != nil || !applied {
			t.Fatalf("Transition to stopped = %t, %v", applied, err)
		}
		// The transition wakes the stream, which waits again once it read
		// the record.
		eventually(t, "the stream to wait again", func() bool { return waits() >= base+2 })
		h.storeLines(t, id, processLine(5, "stdout", "p5"))
		poll(base + 2)
		wantFrame(t, s, "p5")

		h.storeLines(t, id, processLine(6, "stdout", "p6"))
		if applied, err := h.svc.Transition(ctx, nil, id, sandbox.StateStopped, sandbox.StateDeleted, "", nil); err != nil || !applied {
			t.Fatalf("Transition to deleted = %t, %v", applied, err)
		}
		wantFrame(t, s, "p6")
		wantEnd(t, s)

		other := h.sandbox(t, nil)
		h.seedLines(t, other)
		s = h.followLogs(t, ctx, other, "follow=true&tail=1")
		wantFrame(t, s, "p3")
		h.storeLines(t, other, processLine(7, "stdout", "last"))
		h.Stop()
		wantFrame(t, s, "last")
		wantEnd(t, s)
		if code := h.ExitCode(t, 10*time.Second); code != 0 {
			t.Fatalf("the hub stopped with %d, want 0", code)
		}
	})
}
