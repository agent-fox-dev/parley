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

// runStatuses are the ways a run that explains itself in a postmortem can
// have stopped.
var runStatuses = []string{"stalled", "block_limit", "cost_limit", "session_limit"}

// postmortemVersion is the one schema version of a postmortem the hub reads.
const postmortemVersion = 1

// postmortem is a stored postmortem, as a response holds it. Its id,
// timestamp and ingested_at are the hub's; the summaries and lists are
// returned as they were sent.
type postmortem struct {
	head
	SchemaVersion  int             `json:"schema_version"`
	RunStatus      string          `json:"run_status"`
	StartedAt      clientTime      `json:"started_at"`
	CompletedAt    clientTime      `json:"completed_at"`
	TaskSummary    json.RawMessage `json:"task_summary"`
	CostSummary    json.RawMessage `json:"cost_summary"`
	BlockedTasks   json.RawMessage `json:"blocked_tasks"`
	SessionHistory json.RawMessage `json:"session_history"`
}

// readPostmortem reads the postmortem the body f sends to t. Its
// schema_version is checked first: a body of another version may follow
// other rules, which those of version 1 would misname. The body names
// neither the id nor the timestamp: an id or timestamp member is ignored
// like any unknown member, and the hub gives both. The order of started_at
// and completed_at is not checked.
func (c *Component) readPostmortem(f fields, t target) (p postmortem, err error) {
	if err := f.schemaVersion(); err != nil {
		return postmortem{}, err
	}
	now := c.deps.Now().UTC().Truncate(time.Second)
	p.head = head{ID: newID(), Workspace: t.workspace, RunID: t.runID, Timestamp: clientTime(now), IngestedAt: timefmt.Time(now)}
	p.SchemaVersion = postmortemVersion
	if _, err := f.required("run_status"); err != nil {
		return postmortem{}, err
	}
	status, err := f.oneOf("run_status", runStatuses...)
	if err != nil {
		return postmortem{}, err
	}
	p.RunStatus = *status
	if p.StartedAt, err = f.requiredTimestamp("started_at"); err != nil {
		return postmortem{}, err
	}
	if p.CompletedAt, err = f.requiredTimestamp("completed_at"); err != nil {
		return postmortem{}, err
	}
	if p.TaskSummary, err = f.requiredObject("task_summary"); err != nil {
		return postmortem{}, err
	}
	var summary map[string]json.RawMessage
	if err := json.Unmarshal(p.TaskSummary, &summary); err != nil {
		return postmortem{}, invalid("task_summary", "must be a JSON object")
	}
	if total, ok := summary["total"]; !ok || string(total) == "null" {
		return postmortem{}, invalid("task_summary", "must hold the key total with a value other than null")
	}
	if p.CostSummary, err = f.requiredObject("cost_summary"); err != nil {
		return postmortem{}, err
	}
	if p.BlockedTasks, err = f.list("blocked_tasks"); err != nil {
		return postmortem{}, err
	}
	if p.SessionHistory, err = f.list("session_history"); err != nil {
		return postmortem{}, err
	}
	return p, nil
}

// postPostmortem is the reader of the postmortem route: it reads the
// postmortem and stores it unless the workspace and run have one. The first
// postmortem of a workspace and run is answered 201; every later valid one
// is answered 200 with the stored postmortem and changes nothing, whatever
// it holds. Validation comes before the lookup, so that an invalid body is
// refused also when the run has a postmortem.
func (c *Component) postPostmortem(f fields, t target) (act, error) {
	p, err := c.readPostmortem(f, t)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, w http.ResponseWriter) error {
		var stored postmortem
		var created bool
		err := c.write(ctx, func(ctx context.Context, tx Tx) (err error) {
			stored, created, err = storePostmortemOnce(ctx, tx, p)
			return err
		})
		if err != nil {
			return fmt.Errorf("store the postmortem of run %s in workspace %s: %w", p.RunID, p.Workspace, err)
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		server.WriteJSON(w, status, stored)
		return nil
	}, nil
}

// schemaVersion checks the member schema_version, which, when present, must
// be the JSON number 1; absent or null it is 1.
func (f fields) schemaVersion() error {
	if f.absent("schema_version") {
		return nil
	}
	var v any
	if err := json.Unmarshal(f["schema_version"], &v); err != nil || v != any(float64(postmortemVersion)) {
		return server.NewError("unknown_schema_version", http.StatusBadRequest,
			fmt.Sprintf("schema_version must be the number %d, the one version of a postmortem the hub reads", postmortemVersion))
	}
	return nil
}

// requiredTimestamp reads the member name as an RFC 3339 timestamp
// (parseTimestamp), which must be present.
func (f fields) requiredTimestamp(name string) (clientTime, error) {
	t, err := f.timestamp(name)
	if err != nil {
		return clientTime{}, err
	}
	if t == nil {
		return clientTime{}, invalid(name, "is required and must be an RFC 3339 timestamp")
	}
	return clientTime(*t), nil
}

// requiredObject reads the member name as a JSON object, which must be
// present.
func (f fields) requiredObject(name string) (json.RawMessage, error) {
	o, err := f.object(name)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, invalid(name, "is required and must be a JSON object")
	}
	return o, nil
}

// list reads the member name as a JSON array, compacted, the empty array
// when absent.
func (f fields) list(name string) (json.RawMessage, error) {
	if f.absent(name) {
		return json.RawMessage("[]"), nil
	}
	raw := f[name]
	var b bytes.Buffer
	if raw[0] != '[' || json.Compact(&b, raw) != nil {
		return nil, invalid(name, "must be a JSON array")
	}
	return b.Bytes(), nil
}
