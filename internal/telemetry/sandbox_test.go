package telemetry_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// The sessions of the recorder's tests: s1 belongs to a run, s2 to none,
// and s3 is not stored.
const (
	s1        = "0a1b2c3d-0000-4000-8000-0000000000a1"
	s2        = "0a1b2c3d-0000-4000-8000-0000000000a2"
	s3        = "0a1b2c3d-0000-4000-8000-0000000000a3"
	sandboxID = "sbx1"
)

// knownSessions is the session lookup of s1, s2 and s3.
var knownSessions = sessionRuns{runs: map[string]string{s1: someRunID, s2: ""}}

// startedAt is when the recorder's operations start.
var startedAt = time.Date(2026, 1, 2, 3, 0, 0, 250_000_000, time.UTC)

// useOf is the common part of an operation of the session session by the
// user u1 in the sandbox sbx1 of demo--task1.
func useOf(session string) telemetry.SandboxUse {
	return telemetry.SandboxUse{Workspace: demoFork, SandboxID: sandboxID, SessionID: session, ActorType: "user", ActorID: "u1", StartedAt: startedAt}
}

// operationsOf are the four kinds of operation of the session session.
func operationsOf(session string) map[string]telemetry.SandboxOperation {
	exit := 1
	u := useOf(session)
	return map[string]telemetry.SandboxOperation{
		"exec":     telemetry.SandboxExec{SandboxUse: u, Command: []string{"go", "test"}, ExitCode: &exit, DurationMS: 1500},
		"attach":   telemetry.SandboxAttach{SandboxUse: u, DurationMS: 60000},
		"upload":   telemetry.SandboxUpload{SandboxUse: u, Path: "/work/a.txt", Bytes: 12},
		"download": telemetry.SandboxDownload{SandboxUse: u, Path: "/work/a.txt", Bytes: 12},
	}
}

func TestSandboxUse_StoresEachOperationAsOneRunEventOfTheSessionsRun(t *testing.T) {
	adminExec := useOf(s1)
	adminExec.ActorType, adminExec.ActorID = "admin_token", ""
	ops := operationsOf(s1)
	cases := []struct {
		name, eventType string
		op              telemetry.SandboxOperation
		payload         string
	}{
		{"exec", "sandbox.exec", ops["exec"],
			`{"sandbox_id": "sbx1", "actor_type": "user", "actor_id": "u1", "command": ["go", "test"], "exit_code": 1, "timed_out": false, "duration_ms": 1500}`},
		{"attach", "sandbox.attach", ops["attach"],
			`{"sandbox_id": "sbx1", "actor_type": "user", "actor_id": "u1", "duration_ms": 60000}`},
		{"upload", "sandbox.upload", ops["upload"],
			`{"sandbox_id": "sbx1", "actor_type": "user", "actor_id": "u1", "path": "/work/a.txt", "bytes": 12}`},
		{"download", "sandbox.download", ops["download"],
			`{"sandbox_id": "sbx1", "actor_type": "user", "actor_id": "u1", "path": "/work/a.txt", "bytes": 12}`},
		{"exec of the admin token killed at its timeout", "sandbox.exec",
			telemetry.SandboxExec{SandboxUse: adminExec, Command: []string{"sleep", "600"}, TimedOut: true, DurationMS: 30000},
			`{"sandbox_id": "sbx1", "actor_type": "admin_token", "actor_id": null, "command": ["sleep", "600"], "exit_code": null, "timed_out": true, "duration_ms": 30000}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRecorder(t, knownSessions, 0)
			// The sandbox component reports an operation once its answer
			// was written, when the request's context may have ended.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			r.comp.RecordSandboxUse(ctx, c.op)

			if n := r.runEvents(t); n != 1 {
				t.Fatalf("run events = %d, want 1", n)
			}
			var (
				id, workspace, run, ts, ingested, eventType, severity, session, payload string
				node                                                                    *string
			)
			err := r.db.QueryRowContext(context.Background(), `
SELECT id, workspace, run_id, timestamp, ingested_at, event_type, severity, node_id, session_id, payload FROM run_events`).
				Scan(&id, &workspace, &run, &ts, &ingested, &eventType, &severity, &node, &session, &payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := telemetry.NormaliseUUID(id); !ok {
				t.Errorf("id = %q, want a generated UUID", id)
			}
			stamp, err := time.Parse("2006-01-02T15:04:05.000000Z", ts)
			if err != nil {
				t.Fatalf("timestamp %q: %v", ts, err)
			}
			got := map[string]any{
				"workspace": workspace, "run_id": run, "timestamp": stamp.Format(time.RFC3339Nano), "ingested_at": ingested,
				"event_type": eventType, "severity": severity, "node_id": node, "session_id": session,
			}
			want := map[string]any{
				"workspace": demoFork, "run_id": someRunID, "timestamp": "2026-01-02T03:00:00.25Z", "ingested_at": "2026-01-02T03:04:05Z",
				"event_type": c.eventType, "severity": "info", "node_id": (*string)(nil), "session_id": s1,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("run event = %v, want %v", got, want)
			}
			var gotPayload, wantPayload map[string]any
			if err := json.Unmarshal([]byte(payload), &gotPayload); err != nil {
				t.Fatalf("payload %s: %v", payload, err)
			}
			if err := json.Unmarshal([]byte(c.payload), &wantPayload); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotPayload, wantPayload) {
				t.Errorf("payload = %s, want %s", payload, c.payload)
			}
		})
	}
}

func TestSandboxUse_StoresNothingWithoutARunToRecordItUnder(t *testing.T) {
	r := newRecorder(t, knownSessions, 0)
	for _, session := range []struct{ name, id string }{
		{"no session", ""}, {"a session without a run id", s2}, {"a session that is not stored", s3},
	} {
		for kind, op := range operationsOf(session.id) {
			t.Run(session.name+"/"+kind, func(t *testing.T) {
				r.comp.RecordSandboxUse(context.Background(), op)
				if n := r.runEvents(t); n != 0 {
					t.Errorf("run events = %d, want 0", n)
				}
				if strings.Contains(r.log.String(), "level=ERROR") {
					t.Errorf("log = %q, want nothing at error level", r.log.String())
				}
			})
		}
	}
}

func TestSandboxUse_LogsAFailureInOneLineAndReportsNone(t *testing.T) {
	cases := []struct {
		name     string
		sessions telemetry.SessionRuns
		limit    time.Duration
		fault    func(r *recorder)
	}{
		{"the store fails", knownSessions, 0, func(r *recorder) { r.fail.Store(true) }},
		{"the store does not finish within the limit", knownSessions, 100 * time.Millisecond, func(r *recorder) { r.block.Store(true) }},
		{"the session lookup fails", sessionRuns{err: errors.New("the session store is closed")}, 0, func(*recorder) {}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRecorder(t, c.sessions, c.limit)
			c.fault(r)
			exit := 0
			op := telemetry.SandboxExec{SandboxUse: useOf(s1), Command: []string{"echo", "hunter2"}, ExitCode: &exit, DurationMS: 5}

			began := time.Now()
			r.comp.RecordSandboxUse(context.Background(), op)
			if took := time.Since(began); took > 2*time.Second {
				t.Errorf("the record took %s, want it back within 2s", took)
			}

			r.fail.Store(false)
			r.block.Store(false)
			if n := r.runEvents(t); n != 0 {
				t.Errorf("run events = %d, want 0", n)
			}
			lines := r.logLines()
			if len(lines) != 1 {
				t.Fatalf("log = %q, want one line", lines)
			}
			for _, want := range []string{demoFork, sandboxID, "sandbox.exec"} {
				if !strings.Contains(lines[0], want) {
					t.Errorf("log line %q does not name %q", lines[0], want)
				}
			}
			for _, member := range []string{"hunter2", "echo", "actor_type", "actor_id", "command", "exit_code", "timed_out", "duration_ms"} {
				if strings.Contains(lines[0], member) {
					t.Errorf("log line %q holds %q, a member of the payload", lines[0], member)
				}
			}
		})
	}
}
