package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The severities of a run event.
const (
	severityInfo     = "info"
	severityWarning  = "warning"
	severityError    = "error"
	severityCritical = "critical"
)

// defaultSeverity is the severity of the event types that do not get
// severityInfo when a run event is sent without one.
var defaultSeverity = map[string]string{
	"session.fail":         severityError,
	"run.limit_reached":    severityWarning,
	"git.conflict":         severityWarning,
	"harvest.empty":        severityWarning,
	"review.parse_failure": severityWarning,
}

// severityOf is the severity of a run event of eventType sent without one.
func severityOf(eventType string) string {
	if s, ok := defaultSeverity[eventType]; ok {
		return s
	}
	return severityInfo
}

// runEvent is a stored run event, as a response holds it: every member is
// present, an optional one that was not sent is null.
type runEvent struct {
	ID         string          `json:"id"`
	Workspace  string          `json:"workspace"`
	RunID      string          `json:"run_id"`
	Timestamp  clientTime      `json:"timestamp"`
	IngestedAt timefmt.Time    `json:"ingested_at"`
	EventType  string          `json:"event_type"`
	Severity   string          `json:"severity"`
	NodeID     *string         `json:"node_id"`
	SessionID  *string         `json:"session_id"`
	Payload    json.RawMessage `json:"payload"`
}

// readRunEvent reads the run event the body f sends to t, with its
// defaults: a new id, the hub's time as its timestamp and the severity of
// its event type. ingested_at is the hub's time too.
func (c *Component) readRunEvent(f fields, t target) (runEvent, error) {
	now := c.deps.Now().UTC().Truncate(time.Second)
	e := runEvent{Workspace: t.workspace, RunID: t.runID, IngestedAt: timefmt.Time(now)}
	var err error
	if e.EventType, err = f.required("event_type"); err != nil {
		return runEvent{}, err
	}
	severity, err := f.oneOf("severity", severityInfo, severityWarning, severityError, severityCritical)
	if err != nil {
		return runEvent{}, err
	}
	e.Severity = severityOf(e.EventType)
	if severity != nil {
		e.Severity = *severity
	}
	id, err := f.uuid("id")
	if err != nil {
		return runEvent{}, err
	}
	e.ID = newID()
	if id != nil {
		e.ID = *id
	}
	ts, err := f.timestamp("timestamp")
	if err != nil {
		return runEvent{}, err
	}
	e.Timestamp = clientTime(now)
	if ts != nil {
		e.Timestamp = clientTime(*ts)
	}
	if e.NodeID, err = f.label("node_id"); err != nil {
		return runEvent{}, err
	}
	if e.SessionID, err = f.uuid("session_id"); err != nil {
		return runEvent{}, err
	}
	if e.Payload, err = f.object("payload"); err != nil {
		return runEvent{}, err
	}
	return e, nil
}

// postRunEvent serves POST …/events: it stores one run event and answers
// it 201.
func (c *Component) postRunEvent(f fields, t target) (act, error) {
	e, err := c.readRunEvent(f, t)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, w http.ResponseWriter) error {
		err := c.write(ctx, func(tx Tx) error { return insertRunEvent(ctx, tx, e) })
		if err != nil {
			return fmt.Errorf("store run event %s of run %s in workspace %s: %w", e.ID, e.RunID, e.Workspace, err)
		}
		server.WriteJSON(w, http.StatusCreated, e)
		return nil
	}, nil
}
