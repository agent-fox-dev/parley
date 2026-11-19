package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// transcriptRoles are the roles of the items the trace types become. A
// session.result has none: it closes a session and says nothing of the
// conversation, so the transcript leaves it out.
var transcriptRoles = map[string]string{
	"session.init":      "system",
	"assistant.message": "assistant",
	"tool.use":          "tool_use",
	"tool.error":        "tool_error",
}

// transcriptPosition is a trace's place in the order of a transcript: its
// timestamp in the form of storedLayout, its sequence (0 when it has none),
// then its id, which make the order total. A cursor addresses a trace, not
// an item, so that a page continues after the trace its last item came
// from, past every session.result before the next item.
type transcriptPosition struct {
	Timestamp string `json:"t"`
	Sequence  int64  `json:"s"`
	ID        string `json:"id"`
}

// transcriptItem is one item of a transcript, built from one trace, whose
// position it keeps for the cursor.
type transcriptItem struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolName  *string    `json:"tool_name"`
	Timestamp clientTime `json:"timestamp"`

	position transcriptPosition
}

// serveTranscript answers the transcript of the node named by the
// parameter node_id in the path's workspace and run: the node's traces in
// transcript order, without session.result, each turned into an item, a
// page at a time. The run id, node_id, limit and cursor are checked before
// the store is read. A run or node without traces is an empty list.
func (c *Component) serveTranscript(w http.ResponseWriter, r *http.Request) error {
	if _, ok := access.DecisionOf(r); !ok {
		return errNoDecision
	}
	slug, runID := server.Param(r, "slug"), server.Param(r, "run_id")
	if !ValidRunID(runID) {
		return server.InvalidRequest(msgRunID)
	}
	q := ReadQuery(r)
	node := q.String("node_id")
	if err := q.Err(); err != nil {
		return err
	}
	if node == nil || *node == "" {
		return invalid("node_id", "is required and must not be empty")
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after transcriptPosition
	ok, err := page.After(&after)
	if err != nil {
		return err
	}
	var from *transcriptPosition
	if ok {
		from = &after
	}
	items, err := readTranscript(r.Context(), c.deps.DB, slug, runID, *node, from, page.Limit+1)
	if err != nil {
		return fmt.Errorf("read the transcript of node %s of run %s in workspace %s: %w", *node, runID, slug, err)
	}
	return server.WritePageWith(w, page, items, func(it transcriptItem) any { return it.position },
		server.Member{Name: "run_id", Value: runID}, server.Member{Name: "node_id", Value: *node})
}

// readTranscript reads from db at most limit items of the transcript of
// node in the workspace and run, after the trace at after, when given.
// session.result traces are left out by the query, so that a page holds
// limit items whenever that many remain.
func readTranscript(ctx context.Context, db DB, workspace, runID, node string, after *transcriptPosition, limit int) ([]transcriptItem, error) {
	query := selectRecords(traces.table, traces.columns) +
		` WHERE workspace = ? AND run_id = ? AND node_id = ? AND event_type <> 'session.result'`
	args := []any{workspace, runID, node}
	if after != nil {
		query += ` AND (timestamp, COALESCE(sequence, 0), id) > (?, ?, ?)`
		args = append(args, after.Timestamp, after.Sequence, after.ID)
	}
	query += ` ORDER BY timestamp, COALESCE(sequence, 0), id LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read traces: %w", err)
	}
	defer rows.Close()
	var items []transcriptItem
	for rows.Next() {
		var tr trace
		h, dest := traces.dest(&tr)
		if err := scanRecord(rows.Scan, traces.table, h, dest...); err != nil {
			return nil, fmt.Errorf("read traces: %w", err)
		}
		items = append(items, transcriptItemOf(tr))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read traces: %w", err)
	}
	return items, nil
}

// transcriptItemOf is the item the trace tr becomes: its content is the
// member content of its data when that is a string, else empty, and its
// tool name the member tool_name of its data when that is a string, else
// null.
func transcriptItemOf(tr trace) transcriptItem {
	var data map[string]json.RawMessage
	// Data that is not an object has neither member.
	_ = json.Unmarshal(tr.Data, &data)
	it := transcriptItem{
		Role:      transcriptRoles[tr.EventType],
		Timestamp: tr.Timestamp,
		position:  transcriptPosition{Timestamp: tr.Timestamp.stored(), ID: tr.ID},
	}
	if s, ok := jsonString(data["content"]); ok {
		it.Content = s
	}
	if s, ok := jsonString(data["tool_name"]); ok {
		it.ToolName = &s
	}
	if tr.Sequence != nil {
		it.position.Sequence = *tr.Sequence
	}
	return it
}

// jsonString reports whether raw is a JSON string and returns its value.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
