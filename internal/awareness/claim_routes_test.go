package awareness_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/awareness"
)

func TestReadClaim_ReportsExpiredFromExpiresAtOn(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	clk := &clock{now: at}
	s := newServedWith(t, servedConfig{Now: clk.Now})
	id := s.createClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "rework", "ttl": "5m"})

	clk.advance(4*time.Minute + 59*time.Second)
	cl, raw := s.claim(t, id)
	if cl.State != awareness.ClaimActive || cl.ID != id || cl.Workspace != "demo" {
		t.Errorf("claim at T+4m59s = %s, want the active claim %s of demo", raw, id)
	}
	if !jsonHasKeys(t, raw, "id", "workspace_slug", "paths", "target_branch", "intent", "by", "session_id", "change",
		"expires_at", "state", "created_at") {
		t.Errorf("claim = %s, want exactly the eleven members of a claim", raw)
	}

	clk.advance(time.Second)
	if cl, raw := s.claim(t, id); cl.State != awareness.ClaimExpired {
		t.Errorf("claim at T+5m, before any expiry run = %s, want state expired", raw)
	}
}

func TestListClaims_FiltersByStateSessionAndPathNewestFirst(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := newServedWith(t, servedConfig{Now: (&clock{now: at}).Now})
	s.hub.AddWorkspace(t, access.Record{Slug: "other", OwnerID: s.owner, OrgID: s.org, Kind: access.KindRepository,
		Status: access.StatusReady})
	withSession := func(id string) func(*awareness.Claim) {
		return func(cl *awareness.Claim) { cl.SessionID = &id }
	}
	s.seedClaims(
		newClaim(claimA, at.Add(-40*time.Minute), withSession(session1)),
		newClaim(claimB, at.Add(-30*time.Minute), func(cl *awareness.Claim) {
			withSession(session2)(cl)
			cl.Paths = []string{"docs/**"}
		}),
		newClaim(claimC, at.Add(-20*time.Minute), func(cl *awareness.Claim) {
			withSession(session1)(cl)
			ended(awareness.ClaimReleased, at.Add(-10*time.Minute))(cl)
		}),
		// Stored as active, but past its expires_at.
		newClaim(claimD, at.Add(-10*time.Minute), func(cl *awareness.Claim) { cl.ExpiresAt = at.Add(-time.Second) }),
		// A live claim of another workspace.
		newClaim(claimE, at.Add(-5*time.Minute), func(cl *awareness.Claim) { cl.Workspace = "other" }),
	)

	list := func(t *testing.T, query string) (int, []byte, claimPage) {
		t.Helper()
		status, raw := s.get("claims?" + query)
		var page claimPage
		if status == http.StatusOK {
			if !jsonHasKeys(t, raw, "items", "next_cursor") {
				t.Errorf("GET claims?%s = %s, want exactly items and next_cursor", query, raw)
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatal(err)
			}
		}
		return status, raw, page
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{claimB, claimA}},
		{"state=active", []string{claimB, claimA}},
		{"state=released", []string{claimC}},
		{"state=expired", []string{claimD}},
		{"session_id=" + session1, []string{claimA}},
		{"state=released&session_id=" + session1, []string{claimC}},
		{"path=api/v1/*.go", []string{claimA}},
		{"path=web/**", []string{}},
		{"state=fulfilled", []string{}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			status, raw, page := list(t, tc.query)
			if status != http.StatusOK {
				t.Fatalf("GET claims?%s = %d %s, want 200", tc.query, status, raw)
			}
			if got := page.ids(); !slices.Equal(got, tc.want) || page.NextCursor != nil {
				t.Errorf("GET claims?%s = %v (next_cursor %v), want %v and no next page", tc.query, got, page.NextCursor, tc.want)
			}
		})
	}

	t.Run("state=expired reports the claims it lists as expired", func(t *testing.T) {
		_, _, page := list(t, "state=expired")
		if len(page.Items) != 1 || page.Items[0].State != awareness.ClaimExpired {
			t.Errorf("GET claims?state=expired = %+v, want claim D with state expired", page.Items)
		}
	})

	t.Run("limit=1", func(t *testing.T) {
		_, raw, first := list(t, "limit=1")
		if got := first.ids(); !slices.Equal(got, []string{claimB}) || first.NextCursor == nil {
			t.Fatalf("GET claims?limit=1 = %s, want [B] and a next_cursor", raw)
		}
		_, raw, second := list(t, "limit=1&cursor="+url.QueryEscape(*first.NextCursor))
		if got := second.ids(); !slices.Equal(got, []string{claimA}) || second.NextCursor != nil {
			t.Errorf("the page after B = %s, want [A] and no next page", raw)
		}
	})

	t.Run("state=bogus", func(t *testing.T) {
		if status, raw, _ := list(t, "state=bogus"); status != http.StatusBadRequest || errorType(t, raw) != "invalid_request" {
			t.Errorf("GET claims?state=bogus = %d %s, want 400 invalid_request", status, raw)
		}
	})
}

func TestRenewClaim_SetsExpiresAtFromNowAndReplacesTheIntent(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := newServedWith(t, servedConfig{Now: (&clock{now: at.Add(30 * time.Minute)}).Now})
	stamp := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339) }
	for i, tc := range []struct {
		name        string
		body        map[string]any
		wantExpires string
		wantIntent  string
	}{
		{"ttl", map[string]any{"ttl": "2h"}, stamp(2*time.Hour + 30*time.Minute), "rework"},
		{"intent", map[string]any{"intent": "new"}, stamp(time.Hour), "new"},
		{"ttl and intent", map[string]any{"ttl": "24h", "intent": "x"}, stamp(24*time.Hour + 30*time.Minute), "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := []string{claimA, claimB, claimC}[i]
			s.seedClaims(newClaim(id, at, func(cl *awareness.Claim) { cl.SessionID = new(session1) }))
			before, _ := s.claim(t, id)
			events := s.count(t, "hub_events")

			status, raw := s.call(t, http.MethodPatch, "demo/claims/"+id, s.key, tc.body)
			if status != http.StatusOK {
				t.Fatalf("PATCH claims/%s %v = %d %s, want 200", id, tc.body, status, raw)
			}
			var got claimBody
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.ExpiresAt != tc.wantExpires || got.Intent != tc.wantIntent {
				t.Errorf("expires_at, intent = %s, %q; want %s, %q", got.ExpiresAt, got.Intent, tc.wantExpires, tc.wantIntent)
			}
			want := before
			want.ExpiresAt, want.Intent = tc.wantExpires, tc.wantIntent
			if !reflect.DeepEqual(got, want) {
				t.Errorf("PATCH answered %+v, want %+v: every other member unchanged", got, want)
			}
			if read, _ := s.claim(t, id); !reflect.DeepEqual(read, want) {
				t.Errorf("the claim reads %+v after the PATCH, want %+v", read, want)
			}
			if n := s.count(t, "hub_events"); n != events {
				t.Errorf("%d events in the log, want the %d before the renewal", n, events)
			}
		})
	}
}

func TestRenewClaim_RefusesAnInvalidBody(t *testing.T) {
	t.Parallel()
	s := newServedWith(t, servedConfig{Now: (&clock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}).Now})
	id := s.createClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "rework"})
	for _, tc := range []struct {
		name string
		body any
	}{
		{"no member", map[string]any{}},
		{"null members", map[string]any{"ttl": nil, "intent": nil}},
		{"ttl x", map[string]any{"ttl": "x"}},
		{"ttl 0s", map[string]any{"ttl": "0s"}},
		{"ttl 25h", map[string]any{"ttl": "25h"}},
		{"ttl a number", map[string]any{"ttl": 3600}},
		{"an empty intent", map[string]any{"intent": ""}},
		{"an intent of 2001 characters", map[string]any{"intent": strings.Repeat("i", 2001)}},
		{"an intent that is not a string", map[string]any{"intent": 4}},
		{"a valid ttl with an invalid intent", map[string]any{"ttl": "2h", "intent": ""}},
		{"not an object", []string{"ttl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, before := s.claim(t, id)
			status, raw := s.call(t, http.MethodPatch, "demo/claims/"+id, s.key, tc.body)
			if status != http.StatusBadRequest || errorType(t, raw) != "invalid_request" {
				t.Errorf("PATCH claims/%s %v = %d %s, want 400 invalid_request", id, tc.body, status, raw)
			}
			if _, after := s.claim(t, id); string(after) != string(before) {
				t.Errorf("the claim reads %s after the refusal, want %s", after, before)
			}
		})
	}
}

func TestRenewClaim_RefusesAClaimThatIsNotActive(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := newServedWith(t, servedConfig{Now: (&clock{now: at}).Now})
	for _, tc := range []struct {
		name string
		cl   awareness.Claim
	}{
		{"released", newClaim(claimA, at.Add(-time.Hour), ended(awareness.ClaimReleased, at.Add(-time.Minute)))},
		{"fulfilled", newClaim(claimB, at.Add(-time.Hour), ended(awareness.ClaimFulfilled, at.Add(-time.Minute)))},
		{"expired by the expiry run", newClaim(claimC, at.Add(-2*time.Hour), ended(awareness.ClaimExpired, at.Add(-time.Hour)))},
		{"active, past expires_at", newClaim(claimD, at.Add(-time.Hour), func(cl *awareness.Claim) {
			cl.ExpiresAt = at.Add(-time.Second)
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.seedClaims(tc.cl)
			rows := s.claimRows(t)
			status, raw := s.call(t, http.MethodPatch, "demo/claims/"+tc.cl.ID, s.key, map[string]any{"ttl": "1h"})
			if status != http.StatusConflict || errorType(t, raw) != "claim_not_active" {
				t.Errorf("PATCH a %s claim = %d %s, want 409 claim_not_active", tc.name, status, raw)
			}
			if got := s.claimRows(t); !slices.Equal(got, rows) {
				t.Errorf("stored claims after the refusal = %v, want %v", got, rows)
			}
		})
	}
}

func TestReleaseClaim_ReleasesAnActiveClaimAndAnnouncesIt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	clk := &clock{now: at}
	s := newServedWith(t, servedConfig{Now: clk.Now})
	id := s.createClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "rework"})
	clk.advance(10 * time.Minute)

	status, raw := s.call(t, http.MethodDelete, "demo/claims/"+id, s.key, nil)
	if status != http.StatusNoContent || len(raw) != 0 {
		t.Fatalf("DELETE claims/%s = %d %q, want 204 with an empty body", id, status, raw)
	}
	if cl, raw := s.claim(t, id); cl.State != awareness.ClaimReleased {
		t.Errorf("the claim reads %s, want state released", raw)
	}
	ev := s.lastEvent(t)
	if ev.Type != "parley.claim.release" || ev.ResourceType != "claim" || ev.ResourceID != id || deref(ev.Workspace) != "demo" ||
		ev.ActorType != "api_key" || deref(ev.ActorID) != s.owner ||
		!reflect.DeepEqual(ev.Metadata, map[string]any{"paths": []any{"api/**"}}) {
		t.Errorf("last event = %+v %v, want parley.claim.release of claim %s on demo by api_key %s with {paths: [api/**]}",
			ev, ev.Metadata, id, s.owner)
	}
	var ended int64
	if err := s.hub.Store.Main().QueryRowContext(context.Background(), `SELECT ended_at FROM awareness_claims WHERE id = ?`,
		id).Scan(&ended); err != nil || ended != clk.Now().UnixNano() {
		t.Errorf("ended_at = %d (%v), want the time of the release %d", ended, err, clk.Now().UnixNano())
	}
	if got := s.activity(t, "paths=api/**").claimIDs(); len(got) != 0 {
		t.Errorf("activity claims = %v, want none", got)
	}
}

func TestReleaseClaim_ChangesNothingForAClaimThatIsNotActive(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := newServedWith(t, servedConfig{Now: (&clock{now: at}).Now})
	for _, tc := range []struct {
		name  string
		cl    awareness.Claim
		state string
	}{
		{"released", newClaim(claimA, at.Add(-time.Hour), ended(awareness.ClaimReleased, at.Add(-time.Minute))), awareness.ClaimReleased},
		{"expired", newClaim(claimB, at.Add(-2*time.Hour), ended(awareness.ClaimExpired, at.Add(-time.Hour))), awareness.ClaimExpired},
		{"fulfilled", newClaim(claimC, at.Add(-time.Hour), ended(awareness.ClaimFulfilled, at.Add(-time.Minute))), awareness.ClaimFulfilled},
		{"active, past expires_at", newClaim(claimD, at.Add(-time.Hour), func(cl *awareness.Claim) {
			cl.ExpiresAt = at.Add(-time.Second)
		}), awareness.ClaimExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.seedClaims(tc.cl)
			rows, events := s.claimRows(t), s.count(t, "hub_events")
			status, raw := s.call(t, http.MethodDelete, "demo/claims/"+tc.cl.ID, s.key, nil)
			if status != http.StatusNoContent || len(raw) != 0 {
				t.Errorf("DELETE a %s claim = %d %q, want 204 with an empty body", tc.name, status, raw)
			}
			if cl, _ := s.claim(t, tc.cl.ID); cl.State != tc.state {
				t.Errorf("state = %s, want %s", cl.State, tc.state)
			}
			if got := s.claimRows(t); !slices.Equal(got, rows) {
				t.Errorf("stored claims after the DELETE = %v, want %v", got, rows)
			}
			if n := s.count(t, "hub_events"); n != events {
				t.Errorf("%d events in the log, want the %d before the DELETE", n, events)
			}
		})
	}
}

func TestModifyClaim_AllowsOnlyTheCreatorTheOwnerAndTheAdminToken(t *testing.T) {
	t.Parallel()
	s := newServed(t)
	creator, creatorKey := s.member(t, "creator")
	_, otherKey := s.member(t, "other")
	creatorPAT := s.pat(t, creator, "changes:write")
	for _, tc := range []struct {
		name       string
		credential string
		allowed    bool
	}{
		{"the creator with a second credential", creatorPAT, true},
		{"the workspace owner", s.key, true},
		{"the admin token", s.hub.Token, true},
		{"another member of the organization", otherKey, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := s.call(t, http.MethodPost, "demo/claims", creatorKey, map[string]any{"paths": []string{"api/**"}, "intent": "rework"})
			if status != http.StatusCreated {
				t.Fatalf("POST claims as the creator = %d %s, want 201", status, raw)
			}
			var created claimAnswer
			if err := json.Unmarshal(raw, &created); err != nil {
				t.Fatal(err)
			}
			id := created.Claim.ID
			for _, req := range []struct {
				method string
				body   any
				ok     int
			}{
				{http.MethodPatch, map[string]any{"ttl": "2h"}, http.StatusOK},
				{http.MethodDelete, nil, http.StatusNoContent},
			} {
				_, before := s.claim(t, id)
				releases := s.eventsOf(t, "parley.claim.release")
				status, raw := s.call(t, req.method, "demo/claims/"+id, tc.credential, req.body)
				if tc.allowed {
					if status != req.ok {
						t.Errorf("%s claims/%s = %d %s, want %d", req.method, id, status, raw, req.ok)
					}
					continue
				}
				if status != http.StatusForbidden || errorType(t, raw) != "forbidden" {
					t.Errorf("%s claims/%s = %d %s, want 403 forbidden", req.method, id, status, raw)
				}
				if _, after := s.claim(t, id); string(after) != string(before) {
					t.Errorf("the claim reads %s after the refusal, want %s", after, before)
				}
				if n := s.eventsOf(t, "parley.claim.release"); n != releases {
					t.Errorf("%d parley.claim.release events, want the %d before the refusal", n, releases)
				}
			}
		})
	}
}

func TestClaimRoutes_RefuseAnIDThatNamesNoClaimOfTheWorkspace(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := newServedWith(t, servedConfig{Now: (&clock{now: at}).Now})
	s.hub.AddWorkspace(t, access.Record{Slug: "other", OwnerID: s.owner, OrgID: s.org, Kind: access.KindRepository,
		Status: access.StatusReady})
	s.seedClaims(newClaim(claimF, at, func(cl *awareness.Claim) { cl.Workspace = "other" }))
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		for _, id := range []string{"7d1c3f0e-9b5a-4c2e-8f61-0a4b2d9e8c17", "not-a-uuid", claimF} {
			var body any
			if method == http.MethodPatch {
				body = map[string]any{"ttl": "1h"}
			}
			status, raw := s.call(t, method, "demo/claims/"+id, s.hub.Token, body)
			if status != http.StatusNotFound || errorType(t, raw) != "claim_not_found" {
				t.Errorf("%s demo/claims/%s = %d %s, want 404 claim_not_found", method, id, status, raw)
			}
		}
	}
	if cl, raw := s.claimOn(t, "other", claimF); cl.State != awareness.ClaimActive || cl.ExpiresAt != at.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("the claim of other reads %s, want it active and unchanged", raw)
	}
}
