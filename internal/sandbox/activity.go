package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The kinds of operation an observer is told of.
const (
	OperationExec     = "exec"
	OperationAttach   = "attach"
	OperationUpload   = "upload"
	OperationDownload = "download"
)

// The results of an exec: its command exited 0, exited with another code,
// was killed at its timeout, or did not end with an exit code, because the
// runtime failed or the command was killed when its caller left or the hub
// began to stop.
const (
	ExecResultOK      = "ok"
	ExecResultNonzero = "nonzero"
	ExecResultTimeout = "timeout"
	ExecResultError   = "error"
)

// ExecMetric counts the execs that passed every check, under the label
// result (an ExecResult).
const ExecMetric = "parley_sandbox_exec_total"

// attachTouchInterval is the least time between two activity writes the
// input of one attach connection causes: an open terminal in use is not
// idle, and its keystrokes need not each reach the store.
const attachTouchInterval = time.Minute

// touchTimeout bounds an activity write, which runs after the caller may
// have left.
const touchTimeout = 5 * time.Second

// Operation is the record of an exec, an attach, an upload or a download
// that passed every check of its route, given to every observer once it
// ended. It holds what the operation was and who did it, never what it
// carried: no standard input, output, terminal bytes, file content or
// environment value.
type Operation struct {
	// Kind is OperationExec, OperationAttach, OperationUpload or
	// OperationDownload.
	Kind string
	// SandboxID and Workspace name the sandbox and the workspace slug of the
	// request; SessionID is the sandbox's session, nil for none.
	SandboxID string
	Workspace string
	SessionID *string
	// ActorType is the caller's event actor type, and ActorID its user id
	// ("" for the admin token).
	ActorType string
	ActorID   string
	// StartedAt is when the operation started, and DurationMS how long it
	// ran, on the hub's clock.
	StartedAt  time.Time
	DurationMS int64
	// Command, ExitCode, TimedOut and Result are an exec's: the command as
	// sent, its exit code (nil when it gave none), whether it was killed at
	// its timeout, and an ExecResult.
	Command  []string
	ExitCode *int
	TimedOut bool
	Result   string
	// Path and Bytes are a transfer's: the path in the sandbox, absolute and
	// clean, and the bytes moved: the file content an upload wrote, or the
	// body a download sent.
	Path  string
	Bytes int64
}

// MarshalJSON writes the record as an object with kind, sandbox_id,
// workspace, session_id, actor_type, actor_id, started_at and duration_ms;
// an exec adds command, exit_code, timed_out and result, and a transfer
// path and bytes.
func (o Operation) MarshalJSON() ([]byte, error) {
	type common struct {
		Kind       string       `json:"kind"`
		SandboxID  string       `json:"sandbox_id"`
		Workspace  string       `json:"workspace"`
		SessionID  *string      `json:"session_id"`
		ActorType  string       `json:"actor_type"`
		ActorID    string       `json:"actor_id"`
		StartedAt  timefmt.Time `json:"started_at"`
		DurationMS int64        `json:"duration_ms"`
	}
	c := common{o.Kind, o.SandboxID, o.Workspace, o.SessionID, o.ActorType, o.ActorID, timefmt.Time(o.StartedAt), o.DurationMS}
	switch o.Kind {
	case OperationExec:
		return json.Marshal(struct {
			common
			Command  []string `json:"command"`
			ExitCode *int     `json:"exit_code"`
			TimedOut bool     `json:"timed_out"`
			Result   string   `json:"result"`
		}{c, o.Command, o.ExitCode, o.TimedOut, o.Result})
	case OperationUpload, OperationDownload:
		return json.Marshal(struct {
			common
			Path  string `json:"path"`
			Bytes int64  `json:"bytes"`
		}{c, o.Path, o.Bytes})
	}
	return json.Marshal(c)
}

// Observer is told of each operation that passed every check of its route
// once it ended and its answer was written. Its error, or its panic, is
// logged and changes nothing else.
type Observer func(ctx context.Context, op Operation) error

// observers is the list of observers, safe for concurrent registration.
type observers struct {
	mu   sync.Mutex
	list []Observer
}

// RegisterObserver adds fn to the observers of the operations on sandboxes,
// which are called in the order they were registered. A package registers
// while the hub composes or in a start hook; the telemetry recorder of
// sandbox use is one.
func (s *Service) RegisterObserver(fn Observer) {
	s.observers.mu.Lock()
	defer s.observers.mu.Unlock()
	s.observers.list = append(s.observers.list, fn)
}

// notify calls every observer once with op. An observer that fails or
// panics is logged, and the next is called all the same.
func (s *Service) notify(ctx context.Context, op Operation) {
	s.observers.mu.Lock()
	list := slices.Clone(s.observers.list)
	s.observers.mu.Unlock()
	for _, fn := range list {
		s.observe(ctx, fn, op)
	}
}

// observe calls fn with op and logs its error or its panic.
func (s *Service) observe(ctx context.Context, fn Observer, op Operation) {
	defer func() {
		if p := recover(); p != nil {
			s.logObserver(op, fmt.Sprint("panic: ", p))
		}
	}()
	if err := fn(ctx, op); err != nil {
		s.logObserver(op, err.Error())
	}
}

func (s *Service) logObserver(op Operation, text string) {
	s.logger().Error("a sandbox operation observer failed", "kind", op.Kind, "sandbox_id", op.SandboxID, "error", s.scrub(text))
}

// touch raises the last_activity_at of the sandbox id to at, in whole
// seconds, and never lowers it: two operations whose writes reach the store
// in either order leave the later time. A failure is logged; it changes
// nothing about the operation.
func (s *Service) touch(ctx context.Context, id string, at time.Time) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), touchTimeout)
	defer cancel()
	sec := at.Unix()
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sandboxes SET last_activity_at = ? WHERE id = ? AND last_activity_at < ?`, sec, id, sec)
		return err
	})
	if err != nil {
		s.logger().Warn("cannot record a sandbox's activity", "sandbox_id", id, "error", s.scrub(err.Error()))
	}
}

// activity is one use of a sandbox through a route that passed the gate's
// checks: the operation records its activity when it starts and again,
// through the gate, when it ends, and the observers are told of it then.
// Nothing is recorded for a request that never starts, as one refused by
// a check of the operation itself, such as the exec limit.
type activity struct {
	s   *Service
	ctx context.Context
	w   *answerWriter
	op  Operation
	// started is set by start; when it is not, end does nothing.
	started bool
}

// newActivity is the activity of the request whose caller, workspace slug
// and sandbox u holds, answered through w.
func (s *Service) newActivity(ctx context.Context, w *answerWriter, u useRequest) *activity {
	op := Operation{SandboxID: u.rec.ID, Workspace: u.slug, SessionID: u.rec.SessionID, ActorType: actorOf(u.caller)}
	if u.caller.User != nil {
		op.ActorID = u.caller.User.ID
	}
	return &activity{s: s, ctx: ctx, w: w, op: op}
}

// actorOf is the event actor type of the caller p.
func actorOf(p identity.Principal) string {
	if p.Type == identity.TypeAdmin {
		return event.ActorAdminToken
	}
	return actorType(p)
}

// start records that the operation of kind started now, as activity of
// its sandbox, and returns the time. An exec starts as a failure until it
// reports how its command ended.
func (a *activity) start(kind string) time.Time {
	now := a.s.deps.Now()
	a.op.Kind, a.op.StartedAt, a.started = kind, now, true
	if kind == OperationExec {
		a.op.Result = ExecResultError
	}
	a.s.touch(a.ctx, a.op.SandboxID, now)
	return now
}

// exited records how an exec's command ended: with the exit code code, or
// killed at its timeout when code is nil.
func (a *activity) exited(code *int) {
	a.op.ExitCode, a.op.TimedOut = code, code == nil
	switch {
	case code == nil:
		a.op.Result = ExecResultTimeout
	case *code == 0:
		a.op.Result = ExecResultOK
	default:
		a.op.Result = ExecResultNonzero
	}
}

// answer writes err, the error that ends a started operation, when it is
// an answer and the operation has not begun its own, so that the observers
// are told only once the caller has it; it returns what the pipeline still
// has to deal with.
func (a *activity) answer(err error) error {
	var e *server.Error
	if err == nil || !a.started || a.w.sent || !errors.As(err, &e) {
		return err
	}
	server.WriteError(a.w, e)
	return nil
}

// end records the end of a started operation as activity of its sandbox,
// counts an exec in ExecMetric, sends what of the answer is still held
// and then tells the observers. It runs from a defer, so that an
// operation that drops its connection, by panicking with
// http.ErrAbortHandler, is recorded too.
func (a *activity) end() {
	if !a.started {
		return
	}
	now := a.s.deps.Now()
	a.op.DurationMS = now.Sub(a.op.StartedAt).Milliseconds()
	a.s.touch(a.ctx, a.op.SandboxID, now)
	if a.op.Kind == OperationExec {
		_ = a.s.deps.Recorder.Add(ExecMetric, map[string]string{"result": a.op.Result}, 1)
	}
	if a.w.sent && a.op.Kind != OperationAttach {
		// An attach's connection is no longer the server's to flush.
		_ = http.NewResponseController(a.w).Flush()
	}
	a.s.notify(context.WithoutCancel(a.ctx), a.op)
}

// answerWriter is the writer of a route that uses a sandbox: it notes
// whether the answer has begun.
type answerWriter struct {
	http.ResponseWriter
	sent bool
}

func (w *answerWriter) WriteHeader(code int) {
	w.sent = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *answerWriter) Write(b []byte) (int, error) {
	w.sent = true
	return w.ResponseWriter.Write(b)
}

// Unwrap gives http.ResponseController the server's writer, through which
// an answer is flushed and an attach takes the connection.
func (w *answerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
