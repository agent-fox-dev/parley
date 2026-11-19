package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// auditRun is the run of the audit flows.
const auditRun = "20260601_120000_c0ffee"

// TestAudit runs the flows of the audit record on one hub, where the
// developer registered the workspace demo as every developer does: an
// operator audits it from the shell, then the developer reads what one node
// of a run did.
func TestAudit(t *testing.T) {
	parallel(t)
	h := startWorkspaceHub(t, newOrigin(t), "demo")
	h.must(t, "workspace", "create", "--slug", "demo", "--git-url", h.origin.URL())
	h.waitStatus(t, "demo", workspace.StatusReady)
	agent := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"audit:write"}, Workspace: "demo"})

	t.Run("an operator audits a workspace from the shell", func(t *testing.T) { operatorAuditsAWorkspace(t, h, agent) })
	t.Run("a developer inspects what one node of a run did", func(t *testing.T) { developerInspectsANode(t, h, agent) })
}

// operatorAuditsAWorkspace is an agent that ingests one run event of demo
// and the operator who then runs parley audit query --workspace demo with
// the admin token, and finds the run event beside the hub's own events of
// demo, newest first.
func operatorAuditsAWorkspace(t *testing.T, h *workspaceHub, agent string) {
	if status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/runs/"+auditRun+"/events", agent,
		map[string]any{"event_type": "tool.started"}); status != http.StatusCreated {
		t.Fatalf("ingest the run event = %d %s, want 201", status, body)
	}

	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"--endpoint-url", h.URL(""), "--api-key", h.Token,
		"audit", "query", "--workspace", "demo"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("parley audit query exited %d; stderr = %q", code, stderr.String())
	}
	var page struct {
		Items []struct {
			Source    string  `json:"source"`
			Type      string  `json:"type"`
			Time      string  `json:"time"`
			Workspace *string `json:"workspace"`
			Seq       *int64  `json:"seq"`
			RunID     *string `json:"run_id"`
		} `json:"items"`
	}
	dec := json.NewDecoder(&stdout)
	if err := dec.Decode(&page); err != nil || dec.More() {
		t.Fatalf("stdout is not one JSON document (%v): %q", err, stdout.String())
	}

	var agentItem, hubItem bool
	var times []time.Time
	for _, it := range page.Items {
		if it.Workspace == nil || *it.Workspace != "demo" {
			t.Errorf("item %+v is not of demo", it)
		}
		if it.Source == "agent" && it.Type == "tool.started" && it.RunID != nil && *it.RunID == auditRun {
			agentItem = true
		}
		if it.Source == "hub" && it.Seq != nil && *it.Seq > 0 {
			hubItem = true
		}
		at, err := time.Parse(time.RFC3339Nano, it.Time)
		if err != nil {
			t.Fatalf("item time %q: %v", it.Time, err)
		}
		times = append(times, at)
	}
	if !agentItem || !hubItem {
		t.Errorf("items %s: want the ingested run event and a hub event of demo", stdout.String())
	}
	if !slices.IsSortedFunc(times, func(a, b time.Time) int { return b.Compare(a) }) {
		t.Errorf("item times %v are not newest first", times)
	}
}

// developerInspectsANode is an agent that ingests four traces of the node
// coder and the developer who reads the run's traces and the node's
// transcript, which the two reads leave as they found them.
func developerInspectsANode(t *testing.T, h *workspaceHub, agent string) {
	run := "/api/v1/workspaces/demo/runs/" + auditRun
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	traces := []map[string]any{
		{"event_type": "session.init"},
		{"event_type": "assistant.message", "data": map[string]any{"content": "hello"}},
		{"event_type": "tool.use", "data": map[string]any{"tool_name": "bash"}},
		{"event_type": "session.result"},
	}
	for i, tr := range traces {
		tr["node_id"] = "coder"
		tr["timestamp"] = base.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		if status, body := h.Call(t, http.MethodPost, run+"/traces", agent, tr); status != http.StatusCreated {
			t.Fatalf("ingest trace %d = %d %s, want 201", i, status, body)
		}
	}

	status, first := h.Call(t, http.MethodGet, run+"/traces", h.key, nil)
	var list struct {
		Items []struct {
			Timestamp string `json:"timestamp"`
		} `json:"items"`
	}
	decodeInto(t, first, &list)
	if status != http.StatusOK || len(list.Items) != 4 {
		t.Fatalf("GET traces = %d %s, want 200 with the four traces", status, first)
	}
	var stamps []string
	for _, it := range list.Items {
		stamps = append(stamps, it.Timestamp)
	}
	if !slices.IsSorted(stamps) {
		t.Errorf("trace timestamps of %s do not ascend", first)
	}

	status, raw := h.Call(t, http.MethodGet, run+"/transcript?node_id=coder", h.key, nil)
	var transcript struct {
		Items []struct {
			Role     string  `json:"role"`
			Content  string  `json:"content"`
			ToolName *string `json:"tool_name"`
		} `json:"items"`
	}
	decodeInto(t, raw, &transcript)
	var roles []string
	for _, it := range transcript.Items {
		roles = append(roles, it.Role)
	}
	if status != http.StatusOK || !slices.Equal(roles, []string{"system", "assistant", "tool_use"}) {
		t.Fatalf("GET transcript = %d %s, want 200 with the roles system, assistant, tool_use", status, raw)
	}
	if transcript.Items[1].Content != "hello" || transcript.Items[2].ToolName == nil || *transcript.Items[2].ToolName != "bash" {
		t.Errorf("transcript %s: want the assistant's content hello and the tool bash", raw)
	}

	if _, again := h.Call(t, http.MethodGet, run+"/traces", h.key, nil); !bytes.Equal(again, first) {
		t.Errorf("a second read of the traces = %s, want %s", again, first)
	}
}
