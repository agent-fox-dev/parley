package awareness_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/awareness"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestCreateClaim_StoresTheClaimAndAnswersItsActivity(t *testing.T) {
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := newServedWith(t, servedConfig{Now: (&clock{now: at}).Now})
	s.version(s.touching(1, 1, "", "api/a.go"))
	s.pass()
	stamp := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339) }

	status, raw := s.postClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "rework handlers"})
	if status != http.StatusCreated {
		t.Fatalf("POST claims = %d %s, want 201", status, raw)
	}
	if !jsonHasKeys(t, firstMember(t, raw, "claim"), "id", "workspace_slug", "paths", "target_branch", "intent", "by",
		"session_id", "change", "expires_at", "state", "created_at") {
		t.Errorf("claim = %s, want exactly the eleven members of a claim", firstMember(t, raw, "claim"))
	}
	var a claimAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	cl := a.Claim
	if !uuidPattern.MatchString(cl.ID) {
		t.Errorf("id = %q, want a UUID", cl.ID)
	}
	if cl.Workspace != "demo" || cl.TargetBranch != "main" || cl.State != awareness.ClaimActive || cl.Change != nil ||
		cl.SessionID != nil || cl.Intent != "rework handlers" || !slices.Equal(cl.Paths, []string{"api/**"}) {
		t.Errorf("claim = %+v, want an active claim of demo on main, without change or session, with the given paths and intent", cl)
	}
	if cl.CreatedAt != stamp(0) || cl.ExpiresAt != stamp(time.Hour) {
		t.Errorf("created_at, expires_at = %s, %s; want %s, %s", cl.CreatedAt, cl.ExpiresAt, stamp(0), stamp(time.Hour))
	}
	if cl.By.ActorType != "api_key" || deref(cl.By.ActorID) != s.owner || deref(cl.By.Username) != "owner" {
		t.Errorf("by = %s %s %s, want api_key %s owner", cl.By.ActorType, deref(cl.By.ActorID), deref(cl.By.Username), s.owner)
	}
	if got := a.Activity.changeCounts(); !slices.Equal(got, []string{"1:1"}) || a.Activity.TargetBranch != "main" {
		t.Errorf("activity on %s changes = %v, want main and [1:1]", a.Activity.TargetBranch, got)
	}
	if slices.Contains(a.Activity.claimIDs(), cl.ID) {
		t.Errorf("activity claims = %v, want the new claim left out", a.Activity.claimIDs())
	}
	if n := s.count(t, "awareness_claims"); n != 1 {
		t.Errorf("%d claims stored, want 1", n)
	}
	ev := s.lastEvent(t)
	wantMeta := map[string]any{"paths": []any{"api/**"}, "intent": "rework handlers", "target_branch": "main", "expires_at": cl.ExpiresAt}
	if ev.Type != "parley.claim.create" || ev.ResourceType != "claim" || ev.ResourceID != cl.ID || deref(ev.Workspace) != "demo" ||
		ev.ActorType != "api_key" || deref(ev.ActorID) != s.owner || ev.SessionID != nil || !reflect.DeepEqual(ev.Metadata, wantMeta) {
		t.Errorf("last event = %+v %v, want parley.claim.create of claim %s on demo by api_key %s with %v",
			ev, ev.Metadata, cl.ID, s.owner, wantMeta)
	}

	status, raw = s.postClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "x", "ttl": "5m",
		"target_branch": "release"})
	if status != http.StatusCreated {
		t.Fatalf("POST claims with ttl and target_branch = %d %s, want 201", status, raw)
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if a.Claim.ExpiresAt != stamp(5*time.Minute) || a.Claim.TargetBranch != "release" || a.Activity.TargetBranch != "release" {
		t.Errorf("claim expires_at, target_branch = %s, %s (activity on %s); want %s, release",
			a.Claim.ExpiresAt, a.Claim.TargetBranch, a.Activity.TargetBranch, stamp(5*time.Minute))
	}

	status, raw = s.hub.Call(t, http.MethodPost, "/api/v1/workspaces/demo/claims", s.hub.Token,
		map[string]any{"paths": []string{"docs/**"}, "intent": "y"})
	if status != http.StatusCreated {
		t.Fatalf("POST claims with the admin token = %d %s, want 201", status, raw)
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if ev := s.lastEvent(t); a.Claim.By.ActorType != "admin_token" || a.Claim.By.ActorID != nil || ev.ActorType != "admin_token" ||
		ev.ActorID != nil {
		t.Errorf("by = %+v, event actor %s %v; want admin_token without an id", a.Claim.By, ev.ActorType, deref(ev.ActorID))
	}
}

func TestCreateClaim_AcceptsClaimsOnTheSamePathsAndShowsEachInTheOther(t *testing.T) {
	s := newServed(t)
	s.comp.RegisterSessions(&sessions{accepted: []string{session1, session2}})
	first := s.createClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "a", "session_id": session1})

	status, raw := s.postClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "b", "session_id": session2})
	if status != http.StatusCreated {
		t.Fatalf("POST the second claim on the same paths = %d %s, want 201", status, raw)
	}
	var b claimAnswer
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	if got := b.Activity.claimIDs(); !slices.Equal(got, []string{first}) {
		t.Errorf("the second claim's activity claims = %v, want [%s]", got, first)
	}
	if got := s.activity(t, "paths=api/**&exclude_session="+session1).claimIDs(); !slices.Equal(got, []string{b.Claim.ID}) {
		t.Errorf("activity without the first session's claims = %v, want [%s]", got, b.Claim.ID)
	}
}

func TestCreateClaim_RefusesAnInvalidBody(t *testing.T) {
	s := newServed(t)
	globs := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "dir" + strings.Repeat("x", i) + "/**"
		}
		return out
	}
	valid := func() map[string]any { return map[string]any{"paths": []string{"api/**"}, "intent": "rework"} }
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"no paths", func(b map[string]any) { delete(b, "paths") }},
		{"paths a string", func(b map[string]any) { b["paths"] = "api/**" }},
		{"paths empty", func(b map[string]any) { b["paths"] = []string{} }},
		{"51 globs", func(b map[string]any) { b["paths"] = globs(51) }},
		{"a glob that is not a string", func(b map[string]any) { b["paths"] = []any{1} }},
		{"an empty glob", func(b map[string]any) { b["paths"] = []string{""} }},
		{"a glob of 256 characters", func(b map[string]any) { b["paths"] = []string{strings.Repeat("a", 256)} }},
		{"an absolute glob", func(b map[string]any) { b["paths"] = []string{"/abs"} }},
		{"no intent", func(b map[string]any) { delete(b, "intent") }},
		{"an empty intent", func(b map[string]any) { b["intent"] = "" }},
		{"an intent of 2001 characters", func(b map[string]any) { b["intent"] = strings.Repeat("i", 2001) }},
		{"an intent that is not a string", func(b map[string]any) { b["intent"] = 5 }},
		{"ttl soon", func(b map[string]any) { b["ttl"] = "soon" }},
		{"ttl 0s", func(b map[string]any) { b["ttl"] = "0s" }},
		{"ttl -5m", func(b map[string]any) { b["ttl"] = "-5m" }},
		{"ttl 24h1s", func(b map[string]any) { b["ttl"] = "24h1s" }},
		{"ttl 25h", func(b map[string]any) { b["ttl"] = "25h" }},
		{"ttl a number", func(b map[string]any) { b["ttl"] = 3600 }},
		{"an empty target_branch", func(b map[string]any) { b["target_branch"] = "" }},
		{"a target_branch of 256 characters", func(b map[string]any) { b["target_branch"] = strings.Repeat("b", 256) }},
		{"a target_branch that is not a string", func(b map[string]any) { b["target_branch"] = 1 }},
		{"a session_id that is not a string", func(b map[string]any) { b["session_id"] = 7 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := s.count(t, "hub_events")
			body := valid()
			tc.change(body)
			status, raw := s.postClaim(t, "demo", body)
			if status != http.StatusBadRequest || errorType(t, raw) != "invalid_request" {
				t.Errorf("POST claims %v = %d %s, want 400 invalid_request", body, status, raw)
			}
			if n := s.count(t, "awareness_claims"); n != 0 {
				t.Errorf("%d claims stored, want 0", n)
			}
			if n := s.count(t, "hub_events"); n != events {
				t.Errorf("%d events in the log, want the %d before the request", n, events)
			}
		})
	}

	// The largest body of each member is accepted; an intent is counted in
	// characters, not bytes.
	status, raw := s.postClaim(t, "demo", map[string]any{"paths": globs(50), "intent": strings.Repeat("é", 2000), "ttl": "24h",
		"target_branch": strings.Repeat("b", 255)})
	if status != http.StatusCreated {
		t.Errorf("POST claims with 50 globs, an intent of 2000 characters and ttl 24h = %d %s, want 201", status, raw)
	}
}

func TestCreateClaim_RefusesASessionTheLinkingRuleRefuses(t *testing.T) {
	const (
		sessionOK    = "0000000a-0000-4000-8000-00000000000a"
		sessionOther = "0000000b-0000-4000-8000-00000000000b"
	)
	s := newServed(t)
	for _, tc := range []struct {
		name    string
		wired   bool
		session string
		status  int
		errType string
	}{
		{"an accepted session", true, sessionOK, http.StatusCreated, ""},
		{"a refused session", true, sessionOther, http.StatusBadRequest, "unknown_session"},
		{"a session without a session interface", false, sessionOK, http.StatusBadRequest, "unknown_session"},
		{"no session without a session interface", false, "", http.StatusCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			linker := &sessions{accepted: []string{sessionOK}}
			if tc.wired {
				s.comp.RegisterSessions(linker)
			} else {
				s.comp.RegisterSessions(nil)
			}
			claims, events := s.count(t, "awareness_claims"), s.count(t, "hub_events")
			body := map[string]any{"paths": []string{"api/**"}, "intent": "rework"}
			if tc.session != "" {
				body["session_id"] = tc.session
			}
			status, raw := s.postClaim(t, "demo", body)
			if status != tc.status {
				t.Fatalf("POST claims with session %q = %d %s, want %d", tc.session, status, raw, tc.status)
			}
			grown := 0
			if status == http.StatusCreated {
				grown = 1
				var a claimAnswer
				if err := json.Unmarshal(raw, &a); err != nil {
					t.Fatal(err)
				}
				if deref(a.Claim.SessionID) != tc.session {
					t.Errorf("session_id = %q, want %q", deref(a.Claim.SessionID), tc.session)
				}
				if ev := s.lastEvent(t); deref(ev.SessionID) != tc.session {
					t.Errorf("the event's session_id = %q, want %q", deref(ev.SessionID), tc.session)
				}
				var want []string
				if tc.session != "" {
					want = []string{tc.session + " demo " + a.Claim.ID}
				}
				if !slices.Equal(linker.records, want) {
					t.Errorf("claim links recorded = %v, want %v", linker.records, want)
				}
			} else if got := errorType(t, raw); got != tc.errType {
				t.Errorf("error_type = %q, want %q", got, tc.errType)
			}
			if n := s.count(t, "awareness_claims"); n != claims+grown {
				t.Errorf("%d claims stored, want %d", n, claims+grown)
			}
			if n := s.count(t, "hub_events"); n != events+grown {
				t.Errorf("%d events in the log, want %d", n, events+grown)
			}
		})
	}
}

func TestCreateClaim_RefusesAClaimOverTheLimits(t *testing.T) {
	t.Run("workspace", func(t *testing.T) {
		clk := &clock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
		s := newServedWith(t, servedConfig{TOML: "[awareness]\nmax_claims_per_workspace = 3\n", Now: clk.Now})
		s.hub.AddWorkspace(t, access.Record{Slug: "demo-fork", OwnerID: s.owner, OrgID: s.org, Kind: access.KindFork,
			ParentSlug: "demo", Status: access.StatusReady})
		body := map[string]any{"paths": []string{"api/**"}, "intent": "rework"}
		refused := func(slug string) {
			t.Helper()
			claims := s.count(t, "awareness_claims")
			status, raw := s.postClaim(t, slug, body)
			if status != http.StatusTooManyRequests || errorType(t, raw) != "claim_limit_reached" {
				t.Fatalf("POST %s/claims = %d %s, want 429 claim_limit_reached", slug, status, raw)
			}
			if n := s.count(t, "awareness_claims"); n != claims {
				t.Errorf("%d claims stored, want %d", n, claims)
			}
		}

		s.createClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "short", "ttl": "5m"})
		second := s.createClaim(t, "demo", body)
		s.createClaim(t, "demo", body)
		refused("demo")
		refused("demo-fork")

		txOf(t, s.hub.Store, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(context.Background(), `UPDATE awareness_claims SET state = 'released', ended_at = ? WHERE id = ?`,
				clk.Now().UnixNano(), second)
			return err
		})
		status, raw := s.postClaim(t, "demo-fork", body)
		if status != http.StatusCreated {
			t.Fatalf("POST demo-fork/claims after a release = %d %s, want 201", status, raw)
		}
		var a claimAnswer
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		if a.Claim.Workspace != "demo" {
			t.Errorf("a claim through the fork is stored on %q, want demo", a.Claim.Workspace)
		}
		refused("demo")

		// The short claim is past its expires_at, which no expiry run has
		// stored yet.
		clk.advance(6 * time.Minute)
		s.createClaim(t, "demo", body)
		refused("demo")
	})

	t.Run("session", func(t *testing.T) {
		s := newServed(t)
		s.comp.RegisterSessions(&sessions{accepted: []string{session1, session2}})
		withSession := map[string]any{"paths": []string{"api/**"}, "intent": "rework", "session_id": session1}
		for range 50 {
			s.createClaim(t, "demo", withSession)
		}
		claims := s.count(t, "awareness_claims")
		status, raw := s.postClaim(t, "demo", withSession)
		if status != http.StatusTooManyRequests || errorType(t, raw) != "claim_limit_reached" {
			t.Fatalf("the 51st claim of a session = %d %s, want 429 claim_limit_reached", status, raw)
		}
		if n := s.count(t, "awareness_claims"); n != claims {
			t.Errorf("%d claims stored, want %d", n, claims)
		}
		s.createClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "rework", "session_id": session2})
		s.createClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "rework"})
	})
}
