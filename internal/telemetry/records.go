package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// traceTypes are the event types of a trace record: the closed set a run's
// transcript is rebuilt from.
var traceTypes = []string{"session.init", "assistant.message", "tool.use", "tool.error", "session.result"}

// head holds the members every record carries, first in a response.
type head struct {
	ID         string       `json:"id"`
	Workspace  string       `json:"workspace"`
	RunID      string       `json:"run_id"`
	Timestamp  clientTime   `json:"timestamp"`
	IngestedAt timefmt.Time `json:"ingested_at"`
}

// common is the head of the record, which every kind embeds.
func (h head) common() head { return h }

// record is a kind of record that has a head.
type record interface{ common() head }

// outcome is a stored session outcome, as a response holds it.
type outcome struct {
	head
	SessionID  string          `json:"session_id"`
	Status     string          `json:"status"`
	NodeID     *string         `json:"node_id"`
	DurationMS *int64          `json:"duration_ms"`
	TokenUsage json.RawMessage `json:"token_usage"`
}

// toolCall is a stored tool call, as a response holds it.
type toolCall struct {
	head
	ToolName   string          `json:"tool_name"`
	NodeID     *string         `json:"node_id"`
	SessionID  *string         `json:"session_id"`
	DurationMS *int64          `json:"duration_ms"`
	Input      json.RawMessage `json:"input"`
	Output     json.RawMessage `json:"output"`
}

// toolError is a stored tool error, as a response holds it.
type toolError struct {
	head
	ToolName  string  `json:"tool_name"`
	ErrorMsg  string  `json:"error_msg"`
	ErrorCode *string `json:"error_code"`
	NodeID    *string `json:"node_id"`
	SessionID *string `json:"session_id"`
}

// trace is a stored trace record, as a response holds it.
type trace struct {
	head
	EventType string          `json:"event_type"`
	NodeID    *string         `json:"node_id"`
	SessionID *string         `json:"session_id"`
	Sequence  *int64          `json:"sequence"`
	Data      json.RawMessage `json:"data"`
}

// readHead reads the common members the body f sends to t, with their
// defaults: a new id and the hub's time as the timestamp. ingested_at is
// the hub's time too.
func (c *Component) readHead(f fields, t target) (head, error) {
	now := c.deps.Now().UTC().Truncate(time.Second)
	h := head{ID: newID(), Workspace: t.workspace, RunID: t.runID, Timestamp: clientTime(now), IngestedAt: timefmt.Time(now)}
	id, err := f.uuid("id")
	if err != nil {
		return head{}, err
	}
	if id != nil {
		h.ID = *id
	}
	ts, err := f.timestamp("timestamp")
	if err != nil {
		return head{}, err
	}
	if ts != nil {
		h.Timestamp = clientTime(*ts)
	}
	return h, nil
}

// readOutcome reads the session outcome the body f sends to t. Its
// session_id need not name a stored session.
func (c *Component) readOutcome(f fields, t target) (o outcome, err error) {
	if o.head, err = c.readHead(f, t); err != nil {
		return outcome{}, err
	}
	session, err := f.uuid("session_id")
	if err != nil {
		return outcome{}, err
	}
	if session == nil {
		return outcome{}, invalid("session_id", "is required")
	}
	o.SessionID = *session
	if o.Status, err = f.required("status"); err != nil {
		return outcome{}, err
	}
	if o.NodeID, err = f.label("node_id"); err != nil {
		return outcome{}, err
	}
	if o.DurationMS, err = f.count("duration_ms"); err != nil {
		return outcome{}, err
	}
	if o.TokenUsage, err = f.anyJSON("token_usage"); err != nil {
		return outcome{}, err
	}
	return o, nil
}

// readToolCall reads the tool call the body f sends to t.
func (c *Component) readToolCall(f fields, t target) (tc toolCall, err error) {
	if tc.head, err = c.readHead(f, t); err != nil {
		return toolCall{}, err
	}
	if tc.ToolName, err = f.requiredLabel("tool_name"); err != nil {
		return toolCall{}, err
	}
	if tc.NodeID, err = f.label("node_id"); err != nil {
		return toolCall{}, err
	}
	if tc.SessionID, err = f.uuid("session_id"); err != nil {
		return toolCall{}, err
	}
	if tc.DurationMS, err = f.count("duration_ms"); err != nil {
		return toolCall{}, err
	}
	if tc.Input, err = f.anyJSON("input"); err != nil {
		return toolCall{}, err
	}
	if tc.Output, err = f.anyJSON("output"); err != nil {
		return toolCall{}, err
	}
	return tc, nil
}

// readToolError reads the tool error the body f sends to t.
func (c *Component) readToolError(f fields, t target) (te toolError, err error) {
	if te.head, err = c.readHead(f, t); err != nil {
		return toolError{}, err
	}
	if te.ToolName, err = f.requiredLabel("tool_name"); err != nil {
		return toolError{}, err
	}
	if te.ErrorMsg, err = f.required("error_msg"); err != nil {
		return toolError{}, err
	}
	if te.ErrorCode, err = f.str("error_code"); err != nil {
		return toolError{}, err
	}
	if te.NodeID, err = f.label("node_id"); err != nil {
		return toolError{}, err
	}
	if te.SessionID, err = f.uuid("session_id"); err != nil {
		return toolError{}, err
	}
	return te, nil
}

// readTrace reads the trace record the body f sends to t.
func (c *Component) readTrace(f fields, t target) (tr trace, err error) {
	if tr.head, err = c.readHead(f, t); err != nil {
		return trace{}, err
	}
	if _, err = f.required("event_type"); err != nil {
		return trace{}, err
	}
	eventType, err := f.oneOf("event_type", traceTypes...)
	if err != nil {
		return trace{}, err
	}
	tr.EventType = *eventType
	if tr.NodeID, err = f.label("node_id"); err != nil {
		return trace{}, err
	}
	if tr.SessionID, err = f.uuid("session_id"); err != nil {
		return trace{}, err
	}
	if tr.Sequence, err = f.count("sequence"); err != nil {
		return trace{}, err
	}
	if tr.Data, err = f.anyJSON("data"); err != nil {
		return trace{}, err
	}
	return tr, nil
}

// postRecord is the reader of a single-record route of the kind k: it
// reads the record with read and stores it once under its id. A new record
// is answered 201. A record whose id names a stored record of the same
// workspace and run is answered 200 with the stored record, whatever its
// own members hold, and one whose id names a record of another workspace or
// run is refused 409; neither writes anything. Validation comes before the
// lookup, so that an invalid body is refused whatever its id names. The
// observers are told of a new run event once the write has committed.
func postRecord[R record](c *Component, k kind[R], read func(fields, target) (R, error)) func(fields, target) (act, error) {
	return func(f fields, t target) (act, error) {
		rec, err := read(f, t)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, w http.ResponseWriter) error {
			var stored R
			var created bool
			err := c.write(ctx, func(ctx context.Context, tx Tx) (err error) {
				stored, created, err = storeOnce(ctx, tx, k, rec)
				return err
			})
			h := rec.common()
			if err != nil {
				return fmt.Errorf("store %s %s of run %s in workspace %s: %w", k.name, h.ID, h.RunID, h.Workspace, err)
			}
			switch s := stored.common(); {
			case created:
				c.stored(stored)
				server.WriteJSON(w, http.StatusCreated, stored)
			case s.Workspace != h.Workspace || s.RunID != h.RunID:
				return server.Conflict(fmt.Sprintf("id %s names a %s of another workspace or run", h.ID, k.name))
			default:
				server.WriteJSON(w, http.StatusOK, stored)
			}
			return nil
		}, nil
	}
}

// requiredLabel reads the member name as a non-empty string of at most
// maxLabel characters.
func (f fields) requiredLabel(name string) (string, error) {
	if _, err := f.required(name); err != nil {
		return "", err
	}
	s, err := f.label(name)
	if err != nil {
		return "", err
	}
	return *s, nil
}

// anyJSON reads the member name as any JSON value, compacted, nil when
// absent, so that it is stored and answered as the value that was sent.
func (f fields) anyJSON(name string) (json.RawMessage, error) {
	if f.absent(name) {
		return nil, nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, f[name]); err != nil {
		return nil, invalid(name, "must be a JSON value")
	}
	return b.Bytes(), nil
}
