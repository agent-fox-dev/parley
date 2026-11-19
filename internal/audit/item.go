package audit

import (
	"cmp"
	"encoding/json"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// The sources of an item.
const (
	sourceHub   = "hub"
	sourceAgent = "agent"
)

// item is one entry of the audit list. Both sources have the same
// members, in this order; those of the other source are null.
type item struct {
	Source    string  `json:"source"`
	ID        string  `json:"id"`
	Type      string  `json:"type"`
	Time      string  `json:"time"`
	Workspace *string `json:"workspace"`
	SessionID *string `json:"session_id"`

	Seq          *int64          `json:"seq"`
	ActorType    *string         `json:"actor_type"`
	ActorID      *string         `json:"actor_id"`
	ResourceType *string         `json:"resource_type"`
	ResourceID   *string         `json:"resource_id"`
	Action       *string         `json:"action"`
	Metadata     json.RawMessage `json:"metadata"`

	RunID    *string         `json:"run_id"`
	NodeID   *string         `json:"node_id"`
	Severity *string         `json:"severity"`
	Payload  json.RawMessage `json:"payload"`

	// at is the instant the list orders the item by.
	at time.Time
}

// hubItem is the item of the hub event rec.
func hubItem(rec event.Record) item {
	meta := rec.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	return item{
		Source: sourceHub, ID: rec.ID, Type: rec.Type, Time: timefmt.Format(rec.Time), Workspace: rec.Workspace,
		SessionID: rec.SessionID, Seq: &rec.Seq, ActorType: &rec.ActorType, ActorID: rec.ActorID,
		ResourceType: &rec.ResourceType, ResourceID: &rec.ResourceID, Action: &rec.Action, Metadata: meta,
		at: rec.Time,
	}
}

// agentItem is the item of the run event e. Its time keeps the fraction of
// a second the client sent.
func agentItem(e telemetry.RunEventRecord) item {
	return item{
		Source: sourceAgent, ID: e.ID, Type: e.EventType, Time: e.Timestamp.UTC().Format(time.RFC3339Nano),
		Workspace: &e.Workspace, SessionID: e.SessionID, RunID: &e.RunID, NodeID: e.NodeID, Severity: &e.Severity,
		Payload: e.Payload, at: e.Timestamp,
	}
}

// compareItems orders the audit list: time descending, then id descending,
// then a hub event before a run event, so the order is total even were an
// id in both sources.
func compareItems(a, b item) int {
	return cmp.Or(b.at.Compare(a.at), cmp.Compare(b.ID, a.ID), cmp.Compare(sourceRank(a.Source), sourceRank(b.Source)))
}

// sourceRank is the place of source among items of the same time and id.
func sourceRank(source string) int {
	if source == sourceHub {
		return 0
	}
	return 1
}

// position is an item's place in the audit list, which a cursor carries:
// its time with every digit it has, its id and its source.
type position struct {
	Time   string `json:"t"`
	ID     string `json:"id"`
	Source string `json:"s"`
}

// positionOf is the position of it.
func positionOf(it item) any {
	return position{Time: it.at.UTC().Format(time.RFC3339Nano), ID: it.ID, Source: it.Source}
}
