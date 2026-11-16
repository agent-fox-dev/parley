package awareness_test

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/hub"
)

func TestClaimCleanup_RemovesTheClaimsThatEndedSevenDaysAgo(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	clk := &clock{now: now}
	// passed hears of each maintenance pass once the tasks of known names,
	// claim_cleanup among them, have run; a start runs one pass.
	passed := make(chan struct{}, 4)
	s := newServedWith(t, servedConfig{Now: clk.Now, Compose: func(c *hub.Composition) {
		c.AddMaintenanceTask("claim_cleanup_test_pass", func(context.Context, *hub.Runtime) error {
			select {
			case passed <- struct{}{}:
			default:
			}
			return nil
		})
	}})
	waitPass := func() {
		t.Helper()
		select {
		case <-passed:
		case <-time.After(10 * time.Second):
			t.Fatal("no maintenance pass within 10 s")
		}
	}
	waitPass()

	week := 7 * 24 * time.Hour
	created := now.Add(-30 * 24 * time.Hour)
	s.seedClaims(
		s.storedClaim(claimA, created, ended(awareness.ClaimReleased, now.Add(-week))),
		s.storedClaim(claimB, created, ended(awareness.ClaimExpired, now.Add(-week-time.Second))),
		s.storedClaim(claimC, created, ended(awareness.ClaimFulfilled, now.Add(-8*24*time.Hour))),
		s.storedClaim(claimD, created, ended(awareness.ClaimReleased, now.Add(-week+time.Second))),
		s.storedClaim(claimE, created, func(cl *awareness.Claim) { cl.ExpiresAt = now.Add(time.Hour) }),
	)

	s.hub.Restart(t)
	waitPass()

	if got, want := claimIDsIn(t, s.hub.Store), []string{claimD, claimE}; !slices.Equal(got, want) {
		t.Errorf("claims after claim_cleanup = %v, want %v", got, want)
	}
	if status, raw := s.get("claims/" + claimA); status != http.StatusNotFound || errorType(t, raw) != "claim_not_found" {
		t.Errorf("GET claims/A after claim_cleanup = %d %s, want 404 claim_not_found", status, raw)
	}
	for _, id := range []string{claimD, claimE} {
		s.claim(t, id)
	}
}

func TestRemoveWorkspace_RemovesTheClaimsAndTheIndexOfOneWorkspace(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	f.seedClaims(
		newClaim(claimA, now, nil),
		newClaim(claimB, now, func(cl *awareness.Claim) { cl.Workspace = "other" }),
	)
	f.version(f.touching(1, 1, "", "api/a.go"))
	f.version(f.touching(2, 1, "", "api/a.go"))
	one := f.touching(1, 1, "", "api/a.go")
	one.Workspace = "other"
	f.version(one)
	f.pass()
	if got := f.pairs(); len(got) != 1 {
		t.Fatalf("announced pairs before the removal = %v, want demo's one", got)
	}

	txOf(t, f.store, func(tx *sql.Tx) error { return awareness.RemoveWorkspace(context.Background(), tx, "demo") })

	if got, want := claimIDsIn(t, f.store), []string{claimB}; !slices.Equal(got, want) {
		t.Errorf("claims after removing demo = %v, want only other's %v", got, want)
	}
	for _, row := range f.dump() {
		if len(row) >= 4 && row[:4] == "demo" {
			t.Errorf("index row of demo after its removal: %s", row)
		}
	}
	if got := f.dump(); len(got) != 2 {
		t.Errorf("index rows after removing demo = %v, want other's change and its file", got)
	}
}
