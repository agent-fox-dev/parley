package sandbox

import (
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// Object is the sandbox object every response shows: the 32
// members, each always present. Lists are [] and maps {} when empty;
// timestamps are RFC 3339 in UTC with whole seconds. It holds no token value
// and no secret value.
type Object struct {
	ID                 string            `json:"id"`
	Workspace          string            `json:"workspace"`
	SessionID          *string           `json:"session_id"`
	Profile            string            `json:"profile"`
	Purpose            string            `json:"purpose"`
	State              string            `json:"state"`
	StateReason        *string           `json:"state_reason"`
	Command            []string          `json:"command"`
	Env                map[string]string `json:"env"`
	ExitCode           *int              `json:"exit_code"`
	Checkout           *Checkout         `json:"checkout"`
	Image              string            `json:"image"`
	CPU                string            `json:"cpu"`
	Memory             string            `json:"memory"`
	GPU                int               `json:"gpu"`
	Credentials        []string          `json:"credentials"`
	ExposedSecrets     []string          `json:"exposed_secrets"`
	WorkspaceRules     []string          `json:"workspace_rules"`
	HubAccess          *HubAccessObject  `json:"hub_access"`
	PolicyRevision     int               `json:"policy_revision"`
	ExpiresAt          timefmt.Time      `json:"expires_at"`
	IdleTimeoutSeconds int64             `json:"idle_timeout_seconds"`
	LastActivityAt     timefmt.Time      `json:"last_activity_at"`
	Checkpoint         json.RawMessage   `json:"checkpoint"`
	RestoredFrom       *string           `json:"restored_from"`
	RuntimeReachable   bool              `json:"runtime_reachable"`
	Labels             map[string]string `json:"labels"`
	CreatedBy          Creator           `json:"created_by"`
	CreatedAt          timefmt.Time      `json:"created_at"`
	UpdatedAt          timefmt.Time      `json:"updated_at"`
	EndedAt            *timefmt.Time     `json:"ended_at"`
	// Tools reports whether the sandbox has a tool server, as its profile
	// said at creation; the path is not shown.
	Tools bool `json:"tools"`
}

// HubAccessObject is the hub_access member of an Object: the hub
// credential's scopes, its paths limit (null for none) and its expiry, never
// its value.
type HubAccessObject struct {
	Scopes    []string     `json:"scopes"`
	Paths     []string     `json:"paths"`
	ExpiresAt timefmt.Time `json:"expires_at"`
}

// ObjectOf returns the sandbox object of rec, with runtimeReachable as
// runtime_reachable: what the runtime client reports, not what is stored.
func ObjectOf(rec Record, runtimeReachable bool) Object {
	o := Object{
		ID: rec.ID, Workspace: rec.Workspace, SessionID: rec.SessionID, Profile: rec.Profile,
		Purpose: rec.Purpose, State: rec.State, Command: slices.Clone(rec.Command),
		Env: cloneMap(rec.Env), ExitCode: rec.ExitCode, Checkout: rec.Checkout,
		Image: rec.Image, CPU: rec.CPU, Memory: rec.Memory, GPU: rec.GPU,
		Credentials: orEmpty(slices.Clone(rec.Credentials)), ExposedSecrets: orEmpty(slices.Clone(rec.ExposedSecrets)),
		WorkspaceRules: make([]string, 0, len(rec.WorkspaceRules)), PolicyRevision: rec.PolicyRevision,
		ExpiresAt: timefmt.Time(rec.ExpiresAt), IdleTimeoutSeconds: int64(rec.IdleTimeout / time.Second),
		LastActivityAt: timefmt.Time(rec.LastActivityAt), Checkpoint: rec.Checkpoint,
		RestoredFrom: rec.RestoredFrom, RuntimeReachable: runtimeReachable, Labels: cloneMap(rec.Labels),
		CreatedBy: rec.CreatedBy, CreatedAt: timefmt.Time(rec.CreatedAt), UpdatedAt: timefmt.Time(rec.UpdatedAt),
		Tools: rec.Snapshot.ToolServer != "",
	}
	if rec.StateReason != "" {
		reason := rec.StateReason
		o.StateReason = &reason
	}
	for _, r := range rec.WorkspaceRules {
		o.WorkspaceRules = append(o.WorkspaceRules, r.ID)
	}
	if ha := rec.HubAccess; ha != nil {
		o.HubAccess = &HubAccessObject{Scopes: orEmpty(slices.Clone(ha.Scopes)), Paths: slices.Clone(ha.Paths), ExpiresAt: timefmt.Time(ha.ExpiresAt)}
	}
	if rec.EndedAt != nil {
		ended := timefmt.Time(*rec.EndedAt)
		o.EndedAt = &ended
	}
	return o
}

// cloneMap returns a copy of m, empty and not nil when m is nil.
func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return maps.Clone(m)
}
