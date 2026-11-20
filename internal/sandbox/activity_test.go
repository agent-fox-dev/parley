package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

func TestActivity_RecordsWhenASandboxWasLastUsed(t *testing.T) {
	t.Parallel()
	h := startExecHub(t, 8)
	hold := make(chan struct{})
	h.cmds.set("hold", func(ctx context.Context, _ string, _ runtime.ExecRequest, _, _ io.Writer) int {
		select {
		case <-hold:
			return 0
		case <-ctx.Done():
			return 137
		}
	})

	t.Run("an exec, when its command starts and when it ends", func(t *testing.T) {
		for _, form := range []string{"collected", "streamed"} {
			t.Run(form, func(t *testing.T) {
				id := h.sandbox(t, nil)
				start := h.clock.Now()
				body := map[string]any{"command": []string{"hold"}, "timeout": "30m"}
				var (
					answered <-chan answer
					s        *frameReader
				)
				if form == "collected" {
					answered = h.execing(context.Background(), id, body, nil)
				} else {
					s = h.streaming(t, context.Background(), id, body)
				}
				eventually(t, "the command to run", func() bool { _, running, _, _ := h.cmds.counts(); return running == 1 })
				if got := h.lastActivity(t, id); !got.Equal(start) {
					t.Errorf("while the command runs last_activity_at = %v, want its start %v", got, start)
				}
				h.clock.Advance(5 * time.Minute)
				hold <- struct{}{}
				if form == "collected" {
					if a := receive(t, answered); a.status != http.StatusOK {
						t.Fatalf("exec = %d %s, want 200", a.status, a.body)
					}
				} else if frames := s.rest(t); len(frames) == 0 || frames[len(frames)-1].event != "exit" {
					t.Fatalf("the stream sent %+v, want it to end with an exit frame", frames)
				}
				h.activityAt(t, id, start.Add(5*time.Minute), "once the command ended")
			})
		}
	})

	t.Run("an attach, when it opens, on input at most once a minute, and when it closes", func(t *testing.T) {
		id := h.sandbox(t, nil)
		open := h.clock.Now()
		ws := h.attach(t, id, "")
		if got := h.lastActivity(t, id); !got.Equal(open) {
			t.Errorf("once the connection opened last_activity_at = %v, want %v", got, open)
		}
		h.clock.Advance(30 * time.Second)
		sendFrame(t, ws, []byte("a"))
		receiveBytes(t, ws, 1)
		if got := h.lastActivity(t, id); !got.Equal(open) {
			t.Errorf("after input 30 s after the last write last_activity_at = %v, want it left at %v", got, open)
		}
		h.clock.Advance(30 * time.Second)
		sendFrame(t, ws, []byte("b"))
		receiveBytes(t, ws, 1)
		h.activityAt(t, id, open.Add(time.Minute), "after input 60 s after the last write")
		h.clock.Advance(20 * time.Minute)
		_ = ws.Close()
		h.activityAt(t, id, open.Add(21*time.Minute), "once the connection closed")
	})

	t.Run("a transfer, when it ends", func(t *testing.T) {
		id := h.sandbox(t, nil)
		h.put(t, id, tarEntry{name: "/workspace/a.txt", body: "hello"}, tarEntry{name: "/workspace/d/b.txt", body: "bye"})
		for _, row := range []struct {
			name string
			do   func(t *testing.T) answer
		}{
			{"PUT files a.txt", func(t *testing.T) answer {
				return h.files(t, http.MethodPut, id, "path=up.txt", "application/octet-stream", strings.NewReader("12345"))
			}},
			{"PUT files archive=tar", func(t *testing.T) answer {
				return h.files(t, http.MethodPut, id, "path=up&archive=tar", "application/x-tar", bytes.NewReader(tarOf(t, tarEntry{name: "c.txt", body: "c"})))
			}},
			{"GET files a.txt", func(t *testing.T) answer { return h.mustDownload(t, id, "path=a.txt") }},
			{"GET files archive=tar", func(t *testing.T) answer { return h.mustDownload(t, id, "path=d&archive=tar") }},
		} {
			t.Run(row.name, func(t *testing.T) {
				h.setLastActivity(t, id, recordStart.Add(-time.Hour))
				h.clock.Advance(time.Minute)
				if a := row.do(t); a.status != http.StatusOK {
					t.Fatalf("the transfer = %d %s, want 200", a.status, a.body)
				}
				h.activityAt(t, id, h.clock.Now(), "once the transfer ended")
			})
		}
	})

	t.Run("never earlier than the time it holds", func(t *testing.T) {
		id := h.sandbox(t, nil)
		ctx := context.Background()
		sandbox.Touch(h.svc, ctx, id, recordStart.Add(20*time.Minute))
		sandbox.Touch(h.svc, ctx, id, recordStart.Add(5*time.Minute))
		if got := h.lastActivity(t, id); !got.Equal(recordStart.Add(20 * time.Minute)) {
			t.Errorf("after 20m then 5m last_activity_at = %v, want the later %v", got, recordStart.Add(20*time.Minute))
		}
		var wg sync.WaitGroup
		for _, m := range rand.Perm(50) {
			wg.Go(func() { sandbox.Touch(h.svc, ctx, id, recordStart.Add(time.Duration(m+1)*time.Minute)) })
		}
		wg.Wait()
		if got := h.lastActivity(t, id); !got.Equal(recordStart.Add(50 * time.Minute)) {
			t.Errorf("after 50 concurrent updates last_activity_at = %v, want the latest %v", got, recordStart.Add(50*time.Minute))
		}
	})
}

func TestActivity_RecordsAToolCallWhenItStartsAndWhenItEnds(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "max_concurrent_execs = 1\n")
	id := h.tooled(t, nil)
	if a, _ := h.list(t, id, h.key); a.status != http.StatusOK {
		t.Fatalf("GET tools = %d %s; want 200", a.status, a.body)
	}
	before := h.lastActivity(t, id)

	release, waiting, _ := h.held()
	start := h.clock.Now()
	answered := h.sending(context.Background(), http.MethodPost, toolPath(id, "execute"), h.key, map[string]any{"timeout": "30m"})
	eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
	if got := h.lastActivity(t, id); !got.Equal(start) || !got.After(before) {
		t.Errorf("while the call runs last_activity_at = %v; want its start %v", got, start)
	}
	h.clock.Advance(3 * time.Second)
	for _, row := range []struct {
		name, cred, tool string
		status           int
	}{
		{"a call over the limit", h.key, "read_file", http.StatusTooManyRequests},
		{"a call of an unknown tool", h.key, "no_such_tool", http.StatusNotFound},
		{"a call of a tool outside the set", h.memberKey, "execute", http.StatusForbidden},
	} {
		if a := h.tool(t, id, row.cred, row.tool, nil); a.status != row.status {
			t.Errorf("%s = %d %s; want %d", row.name, a.status, a.body, row.status)
		}
		if got := h.lastActivity(t, id); !got.Equal(start) {
			t.Errorf("after %s last_activity_at = %v; want it left at %v", row.name, got, start)
		}
	}
	release()
	if a := receive(t, answered); a.status != http.StatusOK {
		t.Fatalf("the call = %d %s; want 200", a.status, a.body)
	}
	if got, want := h.lastActivity(t, id), start.Add(3*time.Second); !got.Equal(want) {
		t.Errorf("after the call last_activity_at = %v; want its end %v", got, want)
	}
}

func TestActivity_LeavesTheTimeAloneWhenNothingUsesTheSandbox(t *testing.T) {
	t.Parallel()
	h := &logHub{execHub: startExecHub(t, 1)}
	h.cmds.set("block", h.cmds.blocking("", ""))
	ha := &sandbox.HubAccess{Scopes: []string{"git:write"}}
	id := h.sandbox(t, func(rec *sandbox.Record) { rec.HubAccess = ha })
	h.startMain(t, id)
	h.follow(t, id)
	before := recordStart.Add(-time.Hour)

	t.Run("a line of the main process's output moves it", func(t *testing.T) {
		h.clock.Advance(time.Minute)
		h.emit(id, runtime.Stdout, "x\n")
		h.activityAt(t, id, h.clock.Now(), "after a line of the main process")
	})

	narrow := h.pat(t, h.owner, "sandboxes:write", "sandboxes:read")
	for _, row := range []struct {
		name string
		do   func(t *testing.T)
	}{
		{"a line of the policy log", func(t *testing.T) {
			n := len(h.lines(t, id, sandbox.LogSourcePolicy))
			h.rt.EmitPolicyLog("parley", id, runtime.LogEntry{Action: "deny", Program: "/usr/bin/curl", Host: "example.com", Port: 443})
			eventually(t, "the policy line to be stored", func() bool { return len(h.lines(t, id, sandbox.LogSourcePolicy)) == n+1 })
		}},
		{"an hour in which the main process writes nothing", func(t *testing.T) {
			h.clock.Advance(time.Hour)
			h.round(t)
		}},
		{"a read of the logs", func(t *testing.T) {
			if a := h.logsAs(t, h.key, id, ""); a.status != http.StatusOK {
				t.Fatalf("logs = %d %s, want 200", a.status, a.body)
			}
		}},
		{"a followed read of the logs, opened and closed", func(t *testing.T) {
			_ = h.followLogs(t, context.Background(), id, "follow=true").resp.Body.Close()
		}},
		{"an exec without a command", func(t *testing.T) {
			if a := h.exec(t, id, map[string]any{}, nil); a.status != http.StatusBadRequest {
				t.Fatalf("exec = %d %s, want 400", a.status, a.body)
			}
		}},
		{"an exec with a credential that does not cover the sandbox's", func(t *testing.T) {
			if a := h.execAs(t, narrow, id, map[string]any{"command": []string{"true"}}, nil); a.status != http.StatusForbidden {
				t.Fatalf("exec = %d %s, want 403", a.status, a.body)
			}
		}},
		{"an upload outside the working directory", func(t *testing.T) {
			if a := h.files(t, http.MethodPut, id, "path=../x", "application/octet-stream", strings.NewReader("x")); a.status != http.StatusBadRequest {
				t.Fatalf("upload = %d %s, want 400", a.status, a.body)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h.setLastActivity(t, id, before)
			row.do(t)
			if got := h.lastActivity(t, id); !got.Equal(before) {
				t.Errorf("last_activity_at = %v, want it left at %v", got, before)
			}
		})
	}

	t.Run("an exec over the sandbox's limit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		answered := h.execing(ctx, id, map[string]any{"command": []string{"block"}}, nil)
		defer func() {
			cancel()
			<-answered
		}()
		eventually(t, "the command to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 1 })
		h.setLastActivity(t, id, before)
		if a := h.exec(t, id, map[string]any{"command": []string{"true"}}, nil); a.status != http.StatusTooManyRequests {
			t.Fatalf("exec = %d %s, want 429", a.status, a.body)
		}
		if got := h.lastActivity(t, id); !got.Equal(before) {
			t.Errorf("last_activity_at = %v, want it left at %v", got, before)
		}
	})
}

// recordMembers are the members of every operation record, and those an
// exec's and a transfer's add.
var (
	recordMembers   = []string{"kind", "sandbox_id", "workspace", "session_id", "actor_type", "actor_id", "started_at", "duration_ms"}
	execMembers     = []string{"command", "exit_code", "timed_out", "result"}
	transferMembers = []string{"path", "bytes"}
)

// startObservedHub starts an exec hub that runs one command at a time in a
// sandbox, with the observers first and then rec registered, and metrics as
// its metrics recorder.
func startObservedHub(t *testing.T, first sandbox.Observer, rec *observed, metrics *recordingRecorder) *execHub {
	t.Helper()
	return newExecHub(t, "max_concurrent_execs = 1\n", func(cfg *hubtest.Config) {
		compose := cfg.Compose
		cfg.Compose = func(c *hub.Composition) {
			compose(c)
			c.SetSandboxRecorder(metrics)
			c.Sandbox().RegisterObserver(first)
			c.Sandbox().RegisterObserver(rec.observe)
		}
	})
}

func TestObservers_HearOnceOfEachOperationThatPassedTheChecks(t *testing.T) {
	t.Parallel()
	// failure is how the observer registered first fails: not at all,
	// with an error or with a panic.
	const (
		noFailure = iota
		returnsError
		panics
	)
	var failure atomic.Int32
	first := func(context.Context, sandbox.Operation) error {
		switch failure.Load() {
		case returnsError:
			return errors.New("the first observer broke")
		case panics:
			panic("the first observer panicked")
		}
		return nil
	}
	rec, metrics := &observed{}, &recordingRecorder{}
	h := startObservedHub(t, first, rec, metrics)
	h.cmds.set("exit2", func(context.Context, string, runtime.ExecRequest, io.Writer, io.Writer) int { return 2 })
	h.cmds.set("block", h.cmds.blocking("", ""))
	session := recordSession
	id := h.sandbox(t, func(r *sandbox.Record) { r.SessionID = &session })
	const envValue, stdinValue = "env-VALUE-7f3a", "stdin-BYTES-91c2"
	block := map[string]any{"command": []string{"block"}}

	t.Run("each operation gives one record", func(t *testing.T) {
		rows := []struct {
			name string
			do   func(t *testing.T)
			// extra are the members of its kind; want those checked.
			extra []string
			want  map[string]any
		}{
			{"an exec that exits 0", func(t *testing.T) {
				body := map[string]any{"command": []string{"sh", "-c", "exit 0"}, "env": map[string]string{"K": envValue}, "stdin": stdinValue}
				if a := h.exec(t, id, body, nil); a.status != http.StatusOK {
					t.Fatalf("exec = %d %s, want 200", a.status, a.body)
				}
			}, execMembers, map[string]any{"kind": "exec", "result": "ok", "exit_code": 0.0, "timed_out": false, "command": []any{"sh", "-c", "exit 0"}}},
			{"an exec that exits 2", func(t *testing.T) {
				if a := h.exec(t, id, map[string]any{"command": []string{"exit2"}}, nil); a.status != http.StatusOK {
					t.Fatalf("exec = %d %s, want 200", a.status, a.body)
				}
			}, execMembers, map[string]any{"kind": "exec", "result": "nonzero", "exit_code": 2.0, "timed_out": false}},
			{"an exec that outlasts its timeout", func(t *testing.T) {
				answered := h.execing(context.Background(), id, map[string]any{"command": []string{"block"}, "timeout": "2s"}, nil)
				eventually(t, "the command to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 1 })
				h.clock.Advance(2 * time.Second)
				if a := receive(t, answered); a.status != http.StatusOK {
					t.Fatalf("exec = %d %s, want 200", a.status, a.body)
				}
			}, execMembers, map[string]any{"kind": "exec", "result": "timeout", "exit_code": nil, "timed_out": true, "duration_ms": 2000.0}},
			{"an exec the runtime fails", func(t *testing.T) {
				h.rt.FailOperation(runtime.OpExecStream, runtime.ErrUnavailable, "broken")
				defer h.rt.FailOperation(runtime.OpExecStream, nil, "")
				if a := h.exec(t, id, map[string]any{"command": []string{"true"}}, nil); a.status != http.StatusBadGateway {
					t.Fatalf("exec = %d %s, want 502", a.status, a.body)
				}
			}, execMembers, map[string]any{"kind": "exec", "result": "error", "exit_code": nil, "timed_out": false}},
			{"a streamed exec whose caller leaves", func(t *testing.T) {
				_ = h.streaming(t, context.Background(), id, block).resp.Body.Close()
			}, execMembers, map[string]any{"kind": "exec", "result": "error", "exit_code": nil, "timed_out": false}},
			{"an attach", func(t *testing.T) {
				ws := h.attach(t, id, "")
				h.clock.Advance(90 * time.Second)
				_ = ws.Close()
			}, nil, map[string]any{"kind": "attach", "duration_ms": 90000.0}},
			{"an upload", func(t *testing.T) {
				if a := h.files(t, http.MethodPut, id, "path=a.txt", "application/octet-stream", strings.NewReader("hello")); a.status != http.StatusOK {
					t.Fatalf("upload = %d %s, want 200", a.status, a.body)
				}
			}, transferMembers, map[string]any{"kind": "upload", "path": "/workspace/a.txt", "bytes": 5.0}},
			{"a download", func(t *testing.T) {
				if a := h.mustDownload(t, id, "path=a.txt"); a.status != http.StatusOK {
					t.Fatalf("download = %d %s, want 200", a.status, a.body)
				}
			}, transferMembers, map[string]any{"kind": "download", "path": "/workspace/a.txt", "bytes": 5.0}},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				n := len(rec.records())
				start := h.clock.Now()
				row.do(t)
				eventually(t, "a record", func() bool { return len(rec.records()) > n })
				records := rec.records()
				if len(records) != n+1 {
					t.Fatalf("the observer got %d records, want 1", len(records)-n)
				}
				op := records[n]
				if !op.StartedAt.Equal(start) {
					t.Errorf("started_at = %v, want the clock's %v at the start", op.StartedAt, start)
				}
				raw, err := json.Marshal(op)
				if err != nil {
					t.Fatalf("marshal the record: %v", err)
				}
				for _, secret := range []string{envValue, stdinValue} {
					if bytes.Contains(raw, []byte(secret)) {
						t.Errorf("the record %s holds a value the request sent", raw)
					}
				}
				var o map[string]any
				if err := json.Unmarshal(raw, &o); err != nil {
					t.Fatalf("the record %s is not a JSON object: %v", raw, err)
				}
				if want := slices.Sorted(slices.Values(append(slices.Clone(recordMembers), row.extra...))); !reflect.DeepEqual(slices.Sorted(maps.Keys(o)), want) {
					t.Errorf("the record has the members %v, want %v", slices.Sorted(maps.Keys(o)), want)
				}
				wantMembers(t, o, map[string]any{
					"sandbox_id": id, "workspace": "ws", "session_id": session, "actor_type": "api_key", "actor_id": h.owner,
					"started_at": stamp(start),
				})
				wantMembers(t, o, row.want)
			})
		}
		want := map[string]float64{"result=ok": 1, "result=nonzero": 1, "result=timeout": 1, "result=error": 2}
		if got := metrics.values(sandbox.ExecMetric); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", sandbox.ExecMetric, got, want)
		}
	})

	t.Run("a refused request gives none", func(t *testing.T) {
		covered := h.sandbox(t, func(r *sandbox.Record) { r.HubAccess = &sandbox.HubAccess{Scopes: []string{"git:write"}} })
		stopped := h.sandbox(t, func(r *sandbox.Record) { r.State = sandbox.StateStopped })
		narrow := h.pat(t, h.owner, "sandboxes:write", "sandboxes:read")
		n := len(rec.records())
		cmd := map[string]any{"command": []string{"true"}}
		for _, row := range []struct {
			name, cred, id string
			body           map[string]any
			status         int
		}{
			{"an exec without a command", h.key, id, map[string]any{}, 400},
			{"an exec with a credential that does not cover the sandbox's", narrow, covered, cmd, 403},
			{"an exec in a stopped sandbox", h.key, stopped, cmd, 409},
		} {
			if a := h.execAs(t, row.cred, row.id, row.body, nil); a.status != row.status {
				t.Errorf("%s = %d %s, want %d", row.name, a.status, a.body, row.status)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		answered := h.execing(ctx, id, block, nil)
		eventually(t, "the command to wait", func() bool { _, _, waiting, _ := h.cmds.counts(); return waiting == 1 })
		if a := h.exec(t, id, cmd, nil); a.status != http.StatusTooManyRequests {
			t.Errorf("an exec over the limit = %d %s, want 429", a.status, a.body)
		}
		cancel()
		<-answered
		eventually(t, "the record of the command that waited", func() bool { return len(rec.records()) > n })
		if a := h.exec(t, id, cmd, nil); a.status != http.StatusOK {
			t.Fatalf("exec = %d %s, want 200", a.status, a.body)
		}
		eventually(t, "the record of the last exec", func() bool { return len(rec.records()) > n+1 })
		records := rec.records()[n:]
		if len(records) != 2 || !slices.Equal(records[0].Command, []string{"block"}) || records[1].Result != sandbox.ExecResultOK {
			t.Errorf("the observer got %+v, want the records of the command that waited and of the last exec alone", records)
		}
		want := map[string]float64{"result=ok": 2, "result=nonzero": 1, "result=timeout": 1, "result=error": 3}
		if got := metrics.values(sandbox.ExecMetric); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", sandbox.ExecMetric, got, want)
		}
	})

	t.Run("a failing observer changes neither the answer nor the others' records", func(t *testing.T) {
		for _, row := range []struct {
			name    string
			failure int32
		}{
			{"one that returns an error", returnsError},
			{"one that panics", panics},
		} {
			t.Run(row.name, func(t *testing.T) {
				failure.Store(row.failure)
				defer failure.Store(noFailure)
				failures := strings.Count(h.Log.String(), "observer failed")
				n := len(rec.records())
				o := decoded(t, h.exec(t, id, map[string]any{"command": []string{"true"}}, nil), http.StatusOK)
				wantMembers(t, o, map[string]any{"exit_code": 0.0, "stdout": "", "stderr": "", "timed_out": false})
				eventually(t, "the second observer's record", func() bool { return len(rec.records()) == n+1 })
				eventually(t, "a log line of the failure", func() bool { return strings.Count(h.Log.String(), "observer failed") == failures+1 })
			})
		}
	})
}
