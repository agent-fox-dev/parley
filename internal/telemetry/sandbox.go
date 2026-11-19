package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// SessionRuns is the narrow lookup of the sessions the recorder of sandbox
// use needs: the run id of the session id. found is false for an id that
// names no stored session, and runID is "" for a session without one;
// neither is an error.
type SessionRuns interface {
	RunOf(ctx context.Context, sessionID string) (runID string, found bool, err error)
}

// SandboxUse is what every reported sandbox operation holds: the workspace
// slug and the sandbox it ran in, the sandbox's session ("" for none), the
// caller's actor type and user id ("" for the admin token) and when it
// started.
type SandboxUse struct {
	Workspace string
	SandboxID string
	SessionID string
	ActorType string
	ActorID   string
	StartedAt time.Time
}

// SandboxExec is an exec that ended: the command as sent, its exit code
// (nil when it gave none), whether it was killed at its timeout and how
// long it ran.
type SandboxExec struct {
	SandboxUse
	Command    []string
	ExitCode   *int
	TimedOut   bool
	DurationMS int64
}

// SandboxAttach is an attach that ended, and how long it was open.
type SandboxAttach struct {
	SandboxUse
	DurationMS int64
}

// SandboxUpload is an upload: the path in the sandbox and the bytes
// written.
type SandboxUpload struct {
	SandboxUse
	Path  string
	Bytes int64
}

// SandboxDownload is a download: the path in the sandbox and the bytes
// sent.
type SandboxDownload struct {
	SandboxUse
	Path  string
	Bytes int64
}

// SandboxOperation is one of SandboxExec, SandboxAttach, SandboxUpload and
// SandboxDownload. The four hold what an operation was and who did it, and
// have no place for what it carried: no standard input, output, terminal
// content, file content or environment value can reach the store.
type SandboxOperation interface {
	use() SandboxUse
	// eventType is the event type of the operation's run event, and
	// payload its payload, which holds the operation's members and the
	// common ones of usePayload.
	eventType() string
	payload() any
}

func (u SandboxUse) use() SandboxUse { return u }

// usePayload are the members of every sandbox use payload: actor_id is null
// for the admin token, which is no user.
type usePayload struct {
	SandboxID string  `json:"sandbox_id"`
	ActorType string  `json:"actor_type"`
	ActorID   *string `json:"actor_id"`
}

func (u SandboxUse) common() usePayload {
	p := usePayload{SandboxID: u.SandboxID, ActorType: u.ActorType}
	if u.ActorID != "" {
		p.ActorID = &u.ActorID
	}
	return p
}

func (SandboxExec) eventType() string { return "sandbox.exec" }

func (e SandboxExec) payload() any {
	command := e.Command
	if command == nil {
		command = []string{}
	}
	return struct {
		usePayload
		Command    []string `json:"command"`
		ExitCode   *int     `json:"exit_code"`
		TimedOut   bool     `json:"timed_out"`
		DurationMS int64    `json:"duration_ms"`
	}{e.common(), command, e.ExitCode, e.TimedOut, e.DurationMS}
}

func (SandboxAttach) eventType() string { return "sandbox.attach" }

func (a SandboxAttach) payload() any {
	return struct {
		usePayload
		DurationMS int64 `json:"duration_ms"`
	}{a.common(), a.DurationMS}
}

// transferPayload is the payload of an upload and of a download.
type transferPayload struct {
	usePayload
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

func (SandboxUpload) eventType() string { return "sandbox.upload" }

func (u SandboxUpload) payload() any { return transferPayload{u.common(), u.Path, u.Bytes} }

func (SandboxDownload) eventType() string { return "sandbox.download" }

func (d SandboxDownload) payload() any { return transferPayload{d.common(), d.Path, d.Bytes} }

// RecordSandboxUse stores op, an operation the sandbox component reports
// once it ended, as one run event of its workspace under the run and the
// id of the sandbox's session: severity info, a new id, the time the
// operation started as its timestamp. An operation of a sandbox without a
// session, of a session that is not stored or of one that belongs to no
// run is not recorded.
//
// It is the hub's own write, so it checks neither access nor the
// workspace's status. It runs once the operation's answer was written, so
// it outlives the request's context and is bounded by the write limit
// alone, lookup and write together. It reports nothing: the operation's
// outcome is the operation's. The run event observers are told of each
// run event it stores. A failure is logged in one line naming the
// workspace, the sandbox and the event type, and nothing else of the
// payload, which may say more than an operator's log should.
func (c *Component) RecordSandboxUse(ctx context.Context, op SandboxOperation) {
	u := op.use()
	if u.SessionID == "" || c.deps.Sessions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.deps.WriteTimeout)
	defer cancel()
	if err := c.recordSandboxUse(ctx, u, op); err != nil {
		c.deps.Logger.Error("cannot record a sandbox use as a run event",
			"workspace", u.Workspace, "sandbox_id", u.SandboxID, "event_type", op.eventType(), "error", err)
	}
}

// recordSandboxUse is RecordSandboxUse, which reports its failure.
func (c *Component) recordSandboxUse(ctx context.Context, u SandboxUse, op SandboxOperation) error {
	runID, found, err := c.deps.Sessions.RunOf(ctx, u.SessionID)
	if err != nil {
		if ctx.Err() != nil {
			return errors.Join(errWriteTimeout, err)
		}
		return err
	}
	if !found || runID == "" {
		return nil
	}
	payload, err := json.Marshal(op.payload())
	if err != nil {
		return err
	}
	session := u.SessionID
	now := c.deps.Now().UTC().Truncate(time.Second)
	e := runEvent{
		head: head{
			ID: newID(), Workspace: u.Workspace, RunID: runID,
			Timestamp: clientTime(u.StartedAt.UTC().Truncate(time.Microsecond)), IngestedAt: timefmt.Time(now),
		},
		EventType: op.eventType(),
		Severity:  severityInfo,
		SessionID: &session,
		Payload:   payload,
	}
	if err := c.write(ctx, func(ctx context.Context, tx Tx) error { return runEvents.insert(ctx, tx, e) }); err != nil {
		return err
	}
	c.stored(e)
	return nil
}
