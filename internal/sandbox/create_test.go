package sandbox_test

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestCreate_StoresProvisioningSandbox(t *testing.T) {
	t.Parallel()
	const toolServer = "/opt/tools/agentkit-tools"
	h := startCreateHub(t, agentProfile+"[[sandbox.profiles]]\nname = \"tooled\"\nttl = \"4h\"\nhub_access = \"none\"\ntool_server = \""+toolServer+"\"\n")
	h.world(t)
	s := sandboxObject(t, h.post(t, "demo--t1", map[string]any{
		"profile": "agent", "command": []string{"make", "test"}, "env": map[string]string{"CI": "1"},
		"purpose": "run tests", "labels": map[string]string{"team": "core"}, "ttl": "1h",
	}))
	id, _ := s["id"].(string)
	if !regexp.MustCompile(`^sbx_[0-9a-f]{32}$`).MatchString(id) {
		t.Errorf("id = %q; want sbx_ and 32 lowercase hexadecimal characters", id)
	}
	want := map[string]any{
		"workspace": "demo--t1", "state": "provisioning", "state_reason": nil, "exit_code": nil,
		"checkpoint": nil, "restored_from": nil, "ended_at": nil, "policy_revision": 0.0,
		"runtime_reachable": true, "profile": "agent", "command": []any{"make", "test"},
		"env": map[string]any{"CI": "1"}, "purpose": "run tests", "labels": map[string]any{"team": "core"},
		"session_id": t1Session, "image": "img:1", "cpu": "2", "memory": "4Gi", "gpu": 0.0,
		"idle_timeout_seconds": 1800.0,
		"created_by":           map[string]any{"actor_type": "api_key", "actor_id": h.dana, "username": "dana"},
		"created_at":           stamp(recordStart), "updated_at": stamp(recordStart),
		"last_activity_at": stamp(recordStart), "expires_at": stamp(recordStart.Add(time.Hour)),
		"checkout": map[string]any{"path": "/workspace", "ref": "main", "sha": h.base, "branch": "t1"},
		"tools":    false,
	}
	for k, v := range want {
		if !reflect.DeepEqual(s[k], v) {
			t.Errorf("%s = %#v; want %#v", k, s[k], v)
		}
	}
	if got := decoded(t, h.call(t, http.MethodGet, "/workspaces/demo--t1/sandboxes/"+id, h.key, nil), http.StatusOK); !reflect.DeepEqual(got, s) {
		t.Errorf("get of the new sandbox = %v; want the created object %v", got, s)
	}
	rec, err := sandbox.GetIn(context.Background(), h.Store.Main(), "demo--t1", id)
	if err != nil {
		t.Fatalf("read the stored sandbox: %v", err)
	}
	if rec.ID != id || rec.State != sandbox.StateProvisioning || rec.Marker != sandbox.MarkerCreate ||
		rec.Snapshot.TTL != 4*time.Hour || rec.Snapshot.HubAccess != "fork" || rec.UserID != h.dana ||
		rec.OrgID != h.org || rec.FamilyRoot != "demo" {
		t.Errorf("stored %+v; want sandbox %s in provisioning, marked create, with agent's snapshot, of dana in acme, in demo's family", rec, id)
	}
	if rec.Snapshot.ToolServer != "" {
		t.Errorf("the snapshot of a sandbox of agent names the tool server %q; want none", rec.Snapshot.ToolServer)
	}

	a := h.post(t, "demo--t1", map[string]any{"profile": "tooled"})
	tooled := sandboxObject(t, a)
	if tooled["tools"] != true {
		t.Errorf("tools of a sandbox of a profile with a tool server = %v; want true", tooled["tools"])
	}
	if bytes.Contains(a.body, []byte("agentkit-tools")) {
		t.Errorf("the created object %s holds the tool server's path", a.body)
	}
	tooledID, _ := tooled["id"].(string)
	if rec, err := sandbox.GetIn(context.Background(), h.Store.Main(), "demo--t1", tooledID); err != nil || rec.Snapshot.ToolServer != toolServer {
		t.Errorf("the stored snapshot names the tool server %q (%v); want %q", rec.Snapshot.ToolServer, err, toolServer)
	}
}

func TestCreate_Defaults(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	allNull := map[string]any{}
	for _, m := range []string{"profile", "command", "env", "labels", "purpose", "ttl", "session_id", "checkout", "restore_from"} {
		allNull[m] = nil
	}
	bodies := []struct {
		name string
		body any
	}{
		{"absent", nil},
		{"empty object", "{}"},
		{"undefined member", `{"colour":"red"}`},
		{"every member null", allNull},
	}
	workspaces := []struct {
		slug    string
		expires time.Time
		session any
	}{
		{"demo", recordStart.Add(4 * time.Hour), nil},
		{"demo--t1", recordStart.Add(4 * time.Hour), t1Session},
		{"demo--t2", recordStart.Add(time.Hour), nil},
	}
	for _, w := range workspaces {
		for _, b := range bodies {
			t.Run(w.slug+"/"+b.name, func(t *testing.T) {
				s := sandboxObject(t, h.post(t, w.slug, b.body))
				want := map[string]any{
					"profile": "agent", "command": nil, "env": map[string]any{}, "labels": map[string]any{},
					"purpose": "", "expires_at": stamp(w.expires), "session_id": w.session,
				}
				for k, v := range want {
					if !reflect.DeepEqual(s[k], v) {
						t.Errorf("%s = %#v; want %#v", k, s[k], v)
					}
				}
			})
		}
	}
}

func TestCreate_WritesCreateEvent(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	s := sandboxObject(t, h.post(t, "demo--t1", map[string]any{"command": []string{"make", "test", "--secret=abc"}, "purpose": "p"}))
	id, _ := s["id"].(string)
	events := h.createEvents(t, id)
	if len(events) != 1 {
		t.Fatalf("create events of %s = %+v; want one", id, events)
	}
	e := events[0]
	if e.resourceType != "sandbox" || e.workspace != "demo--t1" || e.actorType != "api_key" || e.actorID != h.dana ||
		e.session == nil || *e.session != t1Session {
		t.Errorf("event of %s %s in %s by %s %s, session %v; want of the sandbox in demo--t1 by dana's api_key, session %s",
			e.resourceType, id, e.workspace, e.actorType, e.actorID, e.session, t1Session)
	}
	keys := slices.Sorted(maps.Keys(e.metadata))
	wantKeys := []string{"command", "credentials", "expires_at", "exposed_secrets", "hub_access", "profile", "purpose", "restored_from", "workspace_rules"}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("metadata members %v; want %v", keys, wantKeys)
	}
	if e.metadata["command"] != "make" || e.metadata["profile"] != "agent" || e.metadata["purpose"] != "p" ||
		!reflect.DeepEqual(e.metadata["hub_access"], s["hub_access"]) || e.metadata["expires_at"] != s["expires_at"] ||
		e.metadata["restored_from"] != nil {
		t.Errorf("metadata %v; want command make, profile agent, purpose p, and hub_access, expires_at and restored_from of %v", e.metadata, s)
	}

	// An event log that refuses the write refuses the create, which stores
	// nothing.
	if _, err := h.Store.Main().Exec(`CREATE TRIGGER refuse_events BEFORE INSERT ON hub_events BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	before := h.footprint(t)
	if a := h.post(t, "demo--t1", nil); a.status != http.StatusInternalServerError {
		t.Errorf("create with a failing event log = %d %s; want 500", a.status, a.body)
	}
	if after := h.footprint(t); after != before {
		t.Errorf("a create whose event failed left %+v; want %+v", after, before)
	}
}
