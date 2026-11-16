package awareness_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/change"
)

// linked is the change the claim cl is linked to, as text: "-" for none.
func linked(cl awareness.Claim) string {
	if cl.Change == nil {
		return "-"
	}
	return fmt.Sprint(*cl.Change)
}

func TestClaimLink_LinksTheSessionsActiveClaimsToItsFirstVersion(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	clk := &clock{now: now}
	f := newFixtureAt(t, clk.Now)
	created := now.Add(-time.Minute)
	in := func(session string, edit func(*awareness.Claim)) func(*awareness.Claim) {
		return func(cl *awareness.Claim) {
			cl.SessionID = new(session)
			if edit != nil {
				edit(cl)
			}
		}
	}
	f.seedClaims(
		newClaim(claimA, created, in(session1, nil)),
		newClaim(claimB, created, in(session1, func(cl *awareness.Claim) { cl.TargetBranch = "release" })),
		newClaim(claimC, created, in(session2, nil)),
		newClaim(claimD, created, in(session1, ended(awareness.ClaimReleased, created))),
		newClaim(claimE, created, in(session1, func(cl *awareness.Claim) { cl.Change = new(int64(3)) })),
		// Stored as active, but expired from its expires_at on.
		newClaim(claimF, now.Add(-2*time.Hour), in(session1, nil)),
		newClaim("aaaaaaaa-0000-4000-8000-000000000010", created, in(session1, func(cl *awareness.Claim) { cl.Workspace = "other" })),
	)

	f.version(f.touching(7, 1, session1, "api/a.go"))
	f.pass()

	want := map[string]string{claimA: "7", claimB: "-", claimC: "-", claimD: "-", claimE: "3", claimF: "-"}
	for id, change := range want {
		if got := linked(f.stored(id)); got != change {
			t.Errorf("claim %s is linked to %s after version 1 of change 7, want %s", id, got, change)
		}
	}
	if got := linked(storedOn(t, f.store, "other", "aaaaaaaa-0000-4000-8000-000000000010")); got != "-" {
		t.Errorf("the session's claim on another workspace is linked to %s, want none", got)
	}

	late := "aaaaaaaa-0000-4000-8000-000000000011"
	f.seedClaims(newClaim(late, now, in(session1, nil)))
	f.version(f.touching(8, 2, session1, "api/b.go"))
	f.pass()
	rows, err := f.store.Main().QueryContext(context.Background(), `SELECT id FROM awareness_claims WHERE change_number = 8`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Errorf("claim %s is linked to change 8, of which only version 2 was read", id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// A retarget repeats the current version's number and records no
	// version: it is not a first version.
	nine := f.touching(9, 1, session1, "api/c.go")
	nine.Workspace, nine.TargetBranch, nine.State = "demo", "main", awareness.StateOpen
	f.changes.set(nine)
	f.events.add(change.EventVersion, "demo", 9, `{"version":1,"kind":"retarget"}`)
	f.pass()
	if got := linked(f.stored(late)); got != "-" {
		t.Errorf("the session's unlinked claim is linked to %s after a retarget of change 9 at version 1, want none", got)
	}
}

func TestClaimLink_FulfilsTheActiveClaimsOfALandedOrAbandonedChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(f *fixture, ch awareness.Change)
	}{
		{"land", func(f *fixture, ch awareness.Change) {
			ch.State, ch.Landed = awareness.StateLanded, &awareness.Landed{SHA: ch.HeadSHA, At: time.Now().UTC()}
			f.emit(eventLand, ch)
		}},
		{"abandon", func(f *fixture, ch awareness.Change) {
			ch.State = awareness.StateAbandoned
			f.emit(eventAbandon, ch)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
			clk := &clock{now: now}
			f := newFixtureAt(t, clk.Now)
			created := now.Add(-time.Minute)
			released := now.Add(-30 * time.Second)
			f.seedClaims(
				newClaim(claimA, created, func(cl *awareness.Claim) { cl.Change = new(int64(7)) }),
				newClaim(claimB, created, func(cl *awareness.Claim) {
					cl.Change, cl.State, cl.EndedAt = new(int64(7)), awareness.ClaimReleased, &released
				}),
				newClaim(claimC, created, func(cl *awareness.Claim) { cl.Change = new(int64(9)) }),
			)
			seven := f.touching(7, 1, "", "api/a.go")
			f.version(seven)
			f.pass()
			clk.advance(time.Minute)

			tc.end(f, seven)
			f.pass()

			if a := f.stored(claimA); a.State != awareness.ClaimFulfilled || a.EndedAt == nil || !a.EndedAt.Equal(clk.Now()) {
				t.Errorf("claim A of change 7: state %q, ended at %v; want fulfilled, ended at %v", a.State, a.EndedAt, clk.Now())
			}
			if b := f.stored(claimB); b.State != awareness.ClaimReleased || !b.EndedAt.Equal(released) {
				t.Errorf("claim B of change 7, released: state %q, ended at %v; want released at %v", b.State, b.EndedAt, released)
			}
			if c := f.stored(claimC); c.State != awareness.ClaimActive || c.EndedAt != nil {
				t.Errorf("claim C of change 9: state %q, ended at %v; want active", c.State, c.EndedAt)
			}
			if got := f.logged("parley.claim."); len(got) != 0 {
				t.Errorf("%d parley.claim.* events (first %s), want none", len(got), got[0].Type)
			}
			act, err := f.comp.Activity(context.Background(), "demo", awareness.ActivityQuery{Paths: []string{"api/**"}, TargetBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, cl := range act.Claims {
				ids = append(ids, cl.ID)
			}
			if len(ids) != 1 || ids[0] != claimC {
				t.Errorf("activity claims = %v, want only %s", ids, claimC)
			}

			seven.State = awareness.StateOpen
			f.emit(eventRestore, seven)
			f.pass()
			if a := f.stored(claimA); a.State != awareness.ClaimFulfilled {
				t.Errorf("claim A after change 7 was restored: state %q, want still fulfilled", a.State)
			}
		})
	}
}
