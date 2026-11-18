package sandbox_test

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

// objectMembers are the members of every sandbox object, sorted.
var objectMembers = []string{
	"checkout", "checkpoint", "command", "cpu", "created_at", "created_by", "credentials", "ended_at", "env",
	"exit_code", "expires_at", "exposed_secrets", "gpu", "hub_access", "id", "idle_timeout_seconds", "image",
	"labels", "last_activity_at", "memory", "policy_revision", "profile", "purpose", "restored_from",
	"runtime_reachable", "session_id", "state", "state_reason", "updated_at", "workspace", "workspace_rules",
}

var wholeSecondUTC = regexp.MustCompile(`^"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z"$`)

func TestObject_HasEveryMember(t *testing.T) {
	t.Parallel()
	// A time off UTC and between two seconds, which the object writes in
	// UTC and whole seconds.
	at := time.Date(2026, 11, 18, 12, 0, 0, 750_000_000, time.FixedZone("+02:00", 2*60*60))
	ended := at.Add(time.Hour)
	exit, session, source, branch := 0, recordSession, "sbx_0123456789abcdef0123456789abcdef", "t1"
	fresh := sandbox.Record{
		ID: sandbox.NewID(), Workspace: "ws", Profile: "agent", State: sandbox.StateProvisioning,
		ExpiresAt: at, LastActivityAt: at, CreatedAt: at, UpdatedAt: at,
		CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: "u1", Username: "owner"},
	}
	full := fresh
	full.SessionID, full.StateReason, full.Command, full.ExitCode = &session, "provision_failed", []string{"agent", "run"}, &exit
	full.Env, full.Labels = map[string]string{"A": "1"}, map[string]string{"team": "core"}
	full.Checkout = &sandbox.Checkout{Path: sandbox.WorkDir, Ref: "main", SHA: strings.Repeat("a", 40), Branch: &branch}
	full.Image, full.CPU, full.Memory, full.GPU = "agent:1", "2", "4Gi", 1
	full.Credentials, full.ExposedSecrets = []string{"model"}, []string{"Z_TOKEN"}
	full.WorkspaceRules = []sandbox.WorkspaceRule{{ID: "rule-1", Body: json.RawMessage(`{"host":"rule-SECRET.test"}`)}}
	full.HubAccess = &sandbox.HubAccess{Scopes: []string{"git:read"}, ExpiresAt: at}
	full.PolicyRevision, full.IdleTimeout = 2, 30*time.Minute
	full.Checkpoint = json.RawMessage(`{"ok":true}`)
	full.RestoredFrom, full.EndedAt = &source, &ended
	full.Snapshot = sandbox.Snapshot{Env: map[string]string{"K": "env-SECRET"}, BasePolicy: []byte("policy-SECRET")}
	full.HubTokenID = "tok-SECRET"

	cases := []struct {
		name string
		rec  sandbox.Record
	}{{"freshly created", fresh}, {"fully populated", full}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(sandbox.ObjectOf(tc.rec, true))
			if err != nil {
				t.Fatal(err)
			}
			var o map[string]json.RawMessage
			if err := json.Unmarshal(body, &o); err != nil {
				t.Fatalf("object %s: %v", body, err)
			}
			if got := slices.Sorted(maps.Keys(o)); !slices.Equal(got, objectMembers) {
				t.Fatalf("members %v; want %v", got, objectMembers)
			}
			for _, name := range []string{"credentials", "exposed_secrets", "workspace_rules"} {
				if !strings.HasPrefix(string(o[name]), "[") {
					t.Errorf("%s = %s; want an array", name, o[name])
				}
			}
			for _, name := range []string{"labels", "env"} {
				if !strings.HasPrefix(string(o[name]), "{") {
					t.Errorf("%s = %s; want an object", name, o[name])
				}
			}
			times := []string{"created_at", "updated_at", "expires_at", "last_activity_at"}
			if tc.rec.EndedAt != nil {
				times = append(times, "ended_at")
			}
			for _, name := range times {
				if !wholeSecondUTC.Match(o[name]) {
					t.Errorf("%s = %s; want RFC 3339 in UTC with whole seconds", name, o[name])
				}
			}
			var creator map[string]json.RawMessage
			if err := json.Unmarshal(o["created_by"], &creator); err != nil {
				t.Fatalf("created_by %s: %v", o["created_by"], err)
			}
			if got := slices.Sorted(maps.Keys(creator)); !slices.Equal(got, []string{"actor_id", "actor_type", "username"}) {
				t.Errorf("created_by members %v; want actor_id, actor_type, username", got)
			}
			if strings.Contains(string(body), "SECRET") {
				t.Errorf("object %s holds a value the record keeps hidden", body)
			}
			if tc.rec.HubAccess != nil {
				var ha map[string]json.RawMessage
				if err := json.Unmarshal(o["hub_access"], &ha); err != nil || !wholeSecondUTC.Match(ha["expires_at"]) {
					t.Errorf("hub_access = %s; want expires_at in RFC 3339 in UTC with whole seconds", o["hub_access"])
				}
				if string(o["workspace_rules"]) != `["rule-1"]` {
					t.Errorf("workspace_rules = %s; want the ids only", o["workspace_rules"])
				}
			}
		})
	}
}
