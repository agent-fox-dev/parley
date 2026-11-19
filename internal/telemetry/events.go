package telemetry

import "encoding/json"

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
	head
	EventType string          `json:"event_type"`
	Severity  string          `json:"severity"`
	NodeID    *string         `json:"node_id"`
	SessionID *string         `json:"session_id"`
	Payload   json.RawMessage `json:"payload"`
}

// readRunEvent reads the run event the body f sends to t, with the
// defaults of readHead and the severity of its event type.
func (c *Component) readRunEvent(f fields, t target) (e runEvent, err error) {
	if e.head, err = c.readHead(f, t); err != nil {
		return runEvent{}, err
	}
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
