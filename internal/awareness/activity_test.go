package awareness_test

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

func TestActivity_ListsInFlightChangesMatchingTheGlobs(t *testing.T) {
	s := newServed(t)
	for _, c := range []struct {
		number int64
		state  string
		target string
		paths  []string
	}{
		{1, awareness.StateOpen, "main", []string{"api/a.go", "api/b.go"}},
		{2, awareness.StateDraft, "main", []string{"api/a.go"}},
		{3, awareness.StateOpen, "main", []string{"docs/x.md"}},
		{4, awareness.StateOpen, "release", []string{"api/a.go"}},
		{5, awareness.StateAbandoned, "main", []string{"api/a.go"}},
	} {
		ch := s.touching(c.number, 1, "", c.paths...)
		ch.TargetBranch = c.target
		if c.state == awareness.StateAbandoned {
			s.version(ch)
			ch.State = c.state
			s.emit(eventAbandon, ch)
			continue
		}
		ch.State = c.state
		s.version(ch)
	}
	s.pass()
	mainHead := gitrepotest.Refs(t, s.repo, "refs/heads/")["main"]

	for _, tc := range []struct {
		query  string
		target string
		// want are the changes, each as "number:count", in order.
		want []string
		// sha is the target_sha, "" for null.
		sha string
	}{
		{"paths=api/**", "main", []string{"1:2", "2:1"}, mainHead},
		{"paths=docs/**,api/a.go", "main", []string{"1:1", "2:1", "3:1"}, mainHead},
		{"paths=docs/**&paths=api/b.go", "main", []string{"1:1", "3:1"}, mainHead},
		{"paths=api/**&target=release", "release", []string{"4:1"}, ""},
		{"paths=api/**&target=nosuch", "nosuch", []string{}, ""},
		{"paths=vendor/**", "main", []string{}, mainHead},
	} {
		t.Run(tc.query, func(t *testing.T) {
			a := s.activity(t, tc.query)
			if a.TargetBranch != tc.target {
				t.Errorf("target_branch = %q, want %q", a.TargetBranch, tc.target)
			}
			if got := deref(a.TargetSHA); got != tc.sha {
				t.Errorf("target_sha = %q, want %q (\"\" for null)", got, tc.sha)
			}
			if got := a.changeCounts(); !slices.Equal(got, tc.want) {
				t.Errorf("changes (number:count) = %v, want %v", got, tc.want)
			}
			if len(a.Claims) != 0 || len(a.Landed) != 0 {
				t.Errorf("claims = %v, landed = %v; want none", a.Claims, a.Landed)
			}
		})
	}
	a := s.activity(t, "paths=api/**")
	if got := a.Changes[0].Paths; !slices.Equal(got, []string{"api/a.go", "api/b.go"}) {
		t.Errorf("paths of change 1 = %v, want [api/a.go api/b.go]", got)
	}
}

func TestActivity_ListsTheLiveClaimsWhoseGlobsCanMatch(t *testing.T) {
	s := newServed(t)
	now := time.Now().UTC().Truncate(time.Second)
	claim := func(id, target, state string, created, expires time.Time, paths ...string) awareness.Claim {
		return awareness.Claim{
			ID: id, Workspace: "demo", TargetBranch: target, Paths: paths, Intent: "work on " + id,
			By:    awareness.Actor{ActorType: "api_key", ActorID: new("u1"), Username: new("dev")},
			State: state, CreatedAt: created, ExpiresAt: expires,
		}
	}
	later := now.Add(time.Hour)
	s.seedClaims(
		claim(claimA, "main", awareness.ClaimActive, now.Add(-10*time.Second), later, "api/v1/*.go"),
		claim(claimB, "main", awareness.ClaimActive, now.Add(-9*time.Second), later, "api/**"),
		claim(claimC, "main", awareness.ClaimActive, now.Add(-8*time.Second), later, "docs/**"),
		claim(claimD, "main", awareness.ClaimReleased, now.Add(-7*time.Second), later, "api/**"),
		claim(claimE, "main", awareness.ClaimActive, now.Add(-2*time.Hour), now.Add(-time.Second), "api/**"),
		claim(claimF, "release", awareness.ClaimActive, now.Add(-6*time.Second), later, "api/**"),
	)

	a := s.activity(t, "paths=api/**")
	if got, want := a.claimIDs(), []string{claimB, claimA}; !slices.Equal(got, want) {
		t.Errorf("claims on main = %v, want %v", got, want)
	}
	if got := s.activity(t, "paths=api/**&target=release").claimIDs(); !slices.Equal(got, []string{claimF}) {
		t.Errorf("claims on release = %v, want [%s]", got, claimF)
	}

	_, raw := s.get("activity?paths=api/**")
	first := firstOf(t, raw, "claims")
	if !jsonHasKeys(t, first, "id", "workspace_slug", "paths", "target_branch", "intent", "by", "session_id", "change",
		"expires_at", "state", "created_at") {
		t.Errorf("claim = %s, want exactly its eleven members", first)
	}
	want := fmt.Sprintf(`{"id":%q,"workspace_slug":"demo","paths":["api/**"],"target_branch":"main","intent":"work on %s",`+
		`"by":{"actor_type":"api_key","actor_id":"u1","username":"dev"},"session_id":null,"change":null,"expires_at":%q,`+
		`"state":"active","created_at":%q}`, claimB, claimB, timefmt.Format(later), timefmt.Format(now.Add(-9*time.Second)))
	if string(first) != want {
		t.Errorf("claim = %s, want %s", first, want)
	}
}

func TestActivity_ListsWhatLandedOnTheTargetSince(t *testing.T) {
	s := newServed(t)
	now := time.Now().UTC()
	// The versions' files come from one head; main holds the landed
	// commits, one on the other, oldest first.
	ch := s.touching(1, 1, "", "api/a.go")
	docs := s.touching(1, 1, "", "docs/x.md")
	shas := map[int64]string{}
	prev := s.base
	for _, l := range []struct {
		number int64
		ago    time.Duration
		file   string
	}{
		{10, 30 * time.Hour, "api/a.go"},
		{11, 2 * time.Hour, "api/a.go"},
		{12, time.Hour, "api/a.go"},
		{13, 30 * time.Minute, "docs/x.md"},
	} {
		prev = commit(t, s.repo, "main", prev, map[string]string{l.file: fmt.Sprintf("landed %d\n", l.number)})
		shas[l.number] = prev
		v := ch
		if l.file == "docs/x.md" {
			v = docs
		}
		v.Number, v.Title = l.number, fmt.Sprintf("Change %d", l.number)
		s.landed(v, prev, now.Add(-l.ago))
	}
	s.pass()

	for _, tc := range []struct {
		name  string
		since string
		want  []int64
	}{
		{"absent", "", []int64{12, 11}},
		{"48 hours ago", timefmt.Format(now.Add(-48 * time.Hour)), []int64{12, 11, 10}},
		{"the landed commit of 11", shas[11], []int64{12}},
		{"the landed commit of 12", shas[12], []int64{}},
		{"now", timefmt.Format(now), []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := "paths=api/**"
			if tc.since != "" {
				query += "&since=" + url.QueryEscape(tc.since)
			}
			if got := s.activity(t, query).landedNumbers(); !slices.Equal(got, tc.want) {
				t.Errorf("landed = %v, want %v", got, tc.want)
			}
		})
	}

	_, raw := s.get("activity?paths=api/**")
	want := fmt.Sprintf(`{"number":12,"ref":"demo#12","title":"Change 12","landed_sha":%q,"landed_at":%q,`+
		`"session_id":null,"paths":["api/a.go"]}`, shas[12], timefmt.Format(now.Add(-time.Hour)))
	if got := firstOf(t, raw, "landed"); string(got) != want {
		t.Errorf("first landed item = %s, want %s", got, want)
	}

	t.Run("at most 100", func(t *testing.T) {
		s := newServed(t)
		ch := s.touching(1, 1, "", "api/a.go")
		for n := int64(1); n <= 101; n++ {
			v := ch
			v.Number = n
			s.landed(v, fmt.Sprintf("%040x", n), now.Add(-time.Duration(102-n)*time.Minute))
		}
		s.pass()
		got := s.activity(t, "paths=api/**").landedNumbers()
		if len(got) != 100 || got[0] != 101 || slices.Contains(got, 1) {
			t.Errorf("landed = %d items from %v, want the 100 newest, 101 first, without 1", len(got), got[:min(len(got), 3)])
		}
	})
}

func TestActivity_LeavesOutTheExcludedSessionAndChange(t *testing.T) {
	s := newServed(t)
	now := time.Now().UTC()
	s.version(s.touching(1, 1, session1, "api/a.go"))
	s.version(s.touching(2, 1, session2, "api/a.go"))
	s.landed(s.touching(12, 1, session1, "api/a.go"), strings.Repeat("c", 40), now.Add(-time.Hour))
	s.pass()
	s.seedClaims(
		awareness.Claim{ID: claimA, Workspace: "demo", TargetBranch: "main", Paths: []string{"api/**"}, Intent: "a",
			By: awareness.Actor{ActorType: "api_key"}, SessionID: new(session1), Change: new(int64(1)),
			State: awareness.ClaimActive, CreatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(time.Hour)},
		awareness.Claim{ID: claimB, Workspace: "demo", TargetBranch: "main", Paths: []string{"api/**"}, Intent: "b",
			By: awareness.Actor{ActorType: "api_key"}, SessionID: new(session2),
			State: awareness.ClaimActive, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
	)

	for _, tc := range []struct {
		query   string
		changes []string
		claims  []string
		landed  []int64
	}{
		{"", []string{"1:1", "2:1"}, []string{claimB, claimA}, []int64{12}},
		{"exclude_session=" + session1, []string{"2:1"}, []string{claimB}, []int64{}},
		{"exclude_change=1", []string{"2:1"}, []string{claimB}, []int64{12}},
		{"exclude_change=12", []string{"1:1", "2:1"}, []string{claimB, claimA}, []int64{}},
		{"exclude_session=" + session2 + "&exclude_change=1", []string{}, []string{}, []int64{12}},
	} {
		name := tc.query
		if name == "" {
			name = "no exclusion"
		}
		t.Run(name, func(t *testing.T) {
			a := s.activity(t, "paths=api/**&"+tc.query)
			if got := a.changeCounts(); !slices.Equal(got, tc.changes) {
				t.Errorf("changes = %v, want %v", got, tc.changes)
			}
			if got := a.claimIDs(); !slices.Equal(got, tc.claims) {
				t.Errorf("claims = %v, want %v", got, tc.claims)
			}
			if got := a.landedNumbers(); !slices.Equal(got, tc.landed) {
				t.Errorf("landed = %v, want %v", got, tc.landed)
			}
		})
	}
}

func TestActivity_RefusesAnInvalidQuery(t *testing.T) {
	s := newServed(t)
	s.version(s.touching(1, 1, "", "api/a.go"))
	s.pass()
	globs := func(n, length int) string {
		var out []string
		for i := range n {
			g := fmt.Sprintf("g%02d/", i)
			out = append(out, g+strings.Repeat("x", length-len(g)))
		}
		return "paths=" + strings.Join(out, ",")
	}
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"no paths", "target=main"},
		{"an empty paths", "paths="},
		{"an empty glob in a list", "paths=a,,b"},
		{"51 globs", globs(51, 10)},
		{"a glob of 256 characters", "paths=" + strings.Repeat("a", 256)},
		{"an absolute glob", "paths=/abs/**"},
		{"a since of another form", "paths=api/**&since=yesterday"},
		{"an abbreviated commit", "paths=api/**&since=abc123"},
		{"a commit the repository does not hold", "paths=api/**&since=" + strings.Repeat("0", 40)},
		{"an exclude_change of 0", "paths=api/**&exclude_change=0"},
		{"an exclude_change that is no number", "paths=api/**&exclude_change=x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := s.get("activity?" + tc.query)
			if status != http.StatusBadRequest || errorType(t, raw) != "invalid_request" {
				t.Errorf("GET activity?%s = %d %s, want 400 invalid_request", tc.query, status, raw)
			}
		})
	}
	t.Run("50 globs of 255 characters", func(t *testing.T) {
		if status, raw := s.get("activity?" + globs(50, 255)); status != http.StatusOK {
			t.Errorf("GET activity with 50 globs of 255 characters = %d %s, want 200", status, raw)
		}
	})
}
