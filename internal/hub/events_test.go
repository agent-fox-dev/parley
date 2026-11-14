package hub_test

import (
	"context"
	"testing"
	"time"

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
