package hub_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

func TestStartHook_MayEmitBeforeListening(t *testing.T) {
	t.Parallel()
	api := "api"
	userID := "u_1"
	in := event.Input{Type: "parley.git.fetch", Workspace: &api, ActorType: "pat", ActorID: &userID,
		ResourceType: "workspace", ResourceID: "api"}
	var events *event.Component
	var emitted event.Record
	hubtest.Start(t, func(c *hub.Composition) {
		events = c.Events()
		c.AddStartHook("emit-at-start", func(ctx context.Context, _ *hub.Runtime) error {
			var err error
			emitted, err = c.Events().EmitNow(ctx, in)
			return err
		})
	})

	recs, _, err := events.ReadAfter(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(recs) != 1 || recs[0].Seq != 1 || recs[0].ID != emitted.ID || recs[0].Type != "parley.git.fetch" {
		t.Errorf("ReadAfter(0) = %+v, want the event the start hook emitted, with seq 1", recs)
	}
}

func TestEvents_NotifiesCommitsWhileTheHubServes(t *testing.T) {
	t.Parallel()
	notified := make(chan event.Record, 1)
	hubtest.Start(t, func(c *hub.Composition) {
		c.Events().OnCommit(func(rec event.Record) { notified <- rec })
		c.AddStartHook("emit-at-start", func(ctx context.Context, _ *hub.Runtime) error {
			_, err := c.Events().EmitNow(ctx, event.Input{Type: "parley.workspace.create", ActorType: "system",
				ResourceType: "workspace", ResourceID: "api", Metadata: map[string]any{"scope": "user", "scope_id": "u_1"}})
			return err
		})
	})

	select {
	case rec := <-notified:
		if rec.Seq != 1 || rec.Type != "parley.workspace.create" {
			t.Errorf("notified %+v, want the event of the start hook with seq 1", rec)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no commit notified within 5 s of the start")
	}
}

func TestEvents_DecidesVisibilityWithTheHubsAccessRule(t *testing.T) {
	t.Parallel()
	var events *event.Component
	h := hubtest.Start(t, func(c *hub.Composition) { events = c.Events() })
	dev, mem, out := h.CreateUser(t, "dev"), h.CreateUser(t, "mem"), h.CreateUser(t, "out")
	acme := h.CreateOrg(t, "acme")
	h.AddMember(t, acme, mem)
	h.AddWorkspace(t, access.Record{Slug: "api", OwnerID: dev, OrgID: acme, Kind: access.KindRepository, Status: access.StatusReady})

	ctx := context.Background()
	api := "api"
	onAPI, err := events.EmitNow(ctx, event.Input{Type: "parley.change.open", Workspace: &api, ActorType: "api_key",
		ActorID: &dev, ResourceType: "change", ResourceID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	ofAcme, err := events.EmitNow(ctx, event.Input{Type: "parley.variable.update", ActorType: "admin_token",
		ResourceType: "variable", ResourceID: "region", Metadata: map[string]any{"scope": "org", "scope_id": acme}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		rec  event.Record
		user string
		want bool
	}{
		{"owner, workspace event", onAPI, dev, true},
		{"org member, workspace event", onAPI, mem, true},
		{"no relation, workspace event", onAPI, out, false},
		{"org member, org event", ofAcme, mem, true},
		{"no relation, org event", ofAcme, out, false},
	} {
		got, err := events.Visible(ctx, event.Caller{UserID: tc.user}, tc.rec)
		if err != nil || got != tc.want {
			t.Errorf("%s: Visible = %v, %v, want %v, nil", tc.name, got, err, tc.want)
		}
	}
}
