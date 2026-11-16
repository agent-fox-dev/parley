package awareness_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/event"
)

func TestExpireClaims_ExpiresEachClaimWhoseTimeHasComeOnce(t *testing.T) {
	start := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	clk := &clock{now: start}
	f := newFixtureAt(t, clk.Now)
	f.seedClaims(
		newClaim(claimA, start, func(cl *awareness.Claim) {
			cl.ExpiresAt, cl.SessionID, cl.Paths = start.Add(5*time.Minute), new(session1), []string{"api/**", "web/*.ts"}
		}),
		newClaim(claimB, start, func(cl *awareness.Claim) { cl.ExpiresAt = start.Add(2 * time.Hour) }),
		newClaim(claimC, start, ended(awareness.ClaimReleased, start.Add(time.Minute))),
	)
	clk.advance(6 * time.Minute)

	if err := f.comp.ExpireClaims(context.Background()); err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}
	a := f.stored(claimA)
	if a.State != awareness.ClaimExpired || a.EndedAt == nil || !a.EndedAt.Equal(start.Add(5*time.Minute)) {
		t.Errorf("claim A: state %q, ended at %v; want expired, ended at its expires_at %v", a.State, a.EndedAt, start.Add(5*time.Minute))
	}
	if b := f.stored(claimB); b.State != awareness.ClaimActive || b.EndedAt != nil {
		t.Errorf("claim B, expiring in the future: state %q, ended at %v; want active, not ended", b.State, b.EndedAt)
	}
	if c := f.stored(claimC); c.State != awareness.ClaimReleased || !c.EndedAt.Equal(start.Add(time.Minute)) {
		t.Errorf("claim C, released: state %q, ended at %v; want released, ended as before", c.State, c.EndedAt)
	}

	expired := f.logged(awareness.EventClaimExpire)
	if len(expired) != 1 {
		t.Fatalf("%d parley.claim.expire events, want 1", len(expired))
	}
	ev := expired[0]
	if ev.ResourceType != "claim" || ev.ResourceID != claimA || deref(ev.Workspace) != "demo" ||
		ev.ActorType != event.ActorSystem || ev.ActorID != nil || deref(ev.SessionID) != session1 {
		t.Errorf("event = %s %s/%s on %q by %s/%v in session %q; want claim/%s on demo by system in session %s",
			ev.Type, ev.ResourceType, ev.ResourceID, deref(ev.Workspace), ev.ActorType, ev.ActorID, deref(ev.SessionID), claimA, session1)
	}
	var meta map[string]any
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(meta); string(got) != `{"paths":["api/**","web/*.ts"]}` {
		t.Errorf("metadata = %s, want {\"paths\":[\"api/**\",\"web/*.ts\"]}", got)
	}

	if err := f.comp.ExpireClaims(context.Background()); err != nil {
		t.Fatalf("second ExpireClaims: %v", err)
	}
	if got := len(f.logged(awareness.EventClaimExpire)); got != 1 {
		t.Errorf("%d parley.claim.expire events after a second run, want still 1", got)
	}
}
