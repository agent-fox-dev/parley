package session_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/session"
)

// stored is a session a list test writes straight into the store, so that
// it can have any creator, status, run id and start: user "" is the admin
// token; parent is the stored parent slug of a session on a task fork.
type stored struct {
	workspace, parent, user, credential, status, runID string
	// at is when the session started, after the clock's start.
	at time.Duration
}

// store writes the session s and returns its id.
func (w *world) store(t *testing.T, s stored) string {
	t.Helper()
	id := freshID()
	if s.status == "" {
		s.status = session.StatusActive
	}
	if s.credential == "" {
		s.credential = session.CredentialAPIKey
		if s.user == "" {
			s.credential = session.CredentialAdminToken
		}
	}
	started := timefmt.Format(newClock().Now().Add(s.at))
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO sessions (id, workspace_slug, parent_slug, run_id, status, user_id, credential_type, started_at)
VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?)`,
			id, s.workspace, s.parent, s.runID, s.status, s.user, s.credential, started)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// list sends GET /sessions?query as credential.
func (w *world) list(t *testing.T, credential, query string) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodGet, "/api/v1/sessions?"+query, credential, nil)
}

// listed reads the session list with query as credential, following
// next_cursor to its end, checks that every item is a full session object
// and returns the ids of the items in their order and the number of pages.
func (w *world) listed(t *testing.T, credential, query string) ([]string, int) {
	t.Helper()
	var pages []usageList
	q := query
	for {
		status, raw := w.list(t, credential, q)
		if status != http.StatusOK {
			t.Fatalf("GET /sessions?%s = %d %s, want 200", q, status, raw)
		}
		if !strings.Contains(string(raw), `"items":[`) {
			t.Fatalf("GET /sessions?%s = %s, want an items array", q, raw)
		}
		var p usageList
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		for _, item := range p.Items {
			for _, m := range members {
				if _, ok := item[m]; !ok {
					t.Errorf("member %s is missing from item %v", m, item)
				}
			}
		}
		pages = append(pages, p)
		if p.NextCursor == nil {
			return idsOf(pages), len(pages)
		}
		if len(pages) > 10 {
			t.Fatalf("GET /sessions?%s: more than 10 pages", query)
		}
		q = query + "&cursor=" + url.QueryEscape(*p.NextCursor)
	}
}

// wantIDs fails the test unless got are the ids want, in their order.
func wantIDs(t *testing.T, got, want []string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got == nil {
		got = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ids = %v, want %v", got, want)
	}
}

// newestFirst orders the ids of sessions as the list does by default: by
// started_at, then id, descending. started gives each id's start.
func newestFirst(started map[string]time.Duration, ids ...string) []string {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b string) int {
		if started[a] != started[b] {
			return -int(started[a] - started[b])
		}
		return -strings.Compare(a, b)
	})
	return out
}

func TestList_AnswersTheSessionsTheCallerMayRead(t *testing.T) {
	w := newWorld(t)
	seeds := []stored{
		{workspace: demo, user: w.alice, at: time.Minute},
		{workspace: demo, user: w.alice, at: 2 * time.Minute},
		{workspace: demo, user: w.alice, at: 2 * time.Minute},
		{workspace: fork1, parent: demo, user: w.erin, at: 3 * time.Minute},
		{workspace: other, user: w.dave, at: 4 * time.Minute},
		{workspace: other, at: 5 * time.Minute},
	}
	started := map[string]time.Duration{}
	ids := make([]string, len(seeds))
	for i, s := range seeds {
		ids[i] = w.store(t, s)
		started[ids[i]] = s.at
	}
	a1, a2, a3, e, d, x := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5]
	alices := newestFirst(started, a1, a2, a3, e)
	cases := []struct {
		name, credential string
		want             []string
	}{
		{"the admin token reads every session", w.Token, newestFirst(started, ids...)},
		{"alice reads her own and those of demo's fork", w.aliceKey, alices},
		{"bob, a member of demo's org, reads the same", w.bobKey, alices},
		{"carol reads none", w.carolKey, nil},
		{"dave reads his own and the admin token's on his workspace", w.daveKey, []string{x, d}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := w.listed(t, c.credential, "")
			wantIDs(t, got, c.want)
			for _, id := range got {
				if status, raw := w.get(t, c.credential, id); status != http.StatusOK {
					t.Errorf("GET /sessions/%s = %d %s, want 200", id, status, raw)
				}
			}
		})
	}
	t.Run("an empty list holds an empty items array", func(t *testing.T) {
		status, raw := w.list(t, w.carolKey, "")
		if status != http.StatusOK || !strings.Contains(string(raw), `"items":[]`) || !strings.Contains(string(raw), `"next_cursor":null`) {
			t.Errorf("GET /sessions as carol = %d %s, want 200 with items [] and next_cursor null", status, raw)
		}
	})
	t.Run("pages of two hold every session once, newest first", func(t *testing.T) {
		got, pages := w.listed(t, w.aliceKey, "limit=2")
		wantIDs(t, got, alices)
		if pages != 2 {
			t.Errorf("pages = %d, want 2", pages)
		}
	})
	t.Run("order=asc reverses the order", func(t *testing.T) {
		got, _ := w.listed(t, w.aliceKey, "order=asc&limit=2")
		want := slices.Clone(alices)
		slices.Reverse(want)
		wantIDs(t, got, want)
	})
	t.Run("order=desc is the default order", func(t *testing.T) {
		got, _ := w.listed(t, w.aliceKey, "order=desc&limit=3")
		wantIDs(t, got, alices)
	})
}

func TestList_AnswersOnlyTheSessionsThatMatchEveryFilter(t *testing.T) {
	w := newWorld(t)
	const run = "20260102_030405_abcdef"
	t1 := time.Hour
	ids := map[string]string{}
	for name, s := range map[string]stored{
		"s1": {workspace: demo, user: w.alice, status: session.StatusCompleted, runID: run, at: t1 - time.Minute},
		"s2": {workspace: demo, user: w.alice, credential: session.CredentialPAT, at: t1},
		"s3": {workspace: fork1, parent: demo, user: w.alice, status: session.StatusTerminated, runID: run, at: t1 + time.Minute},
		"s4": {workspace: demo, at: t1 + 2*time.Minute},
		"s5": {workspace: other, user: w.dave, runID: run, at: t1},
		"s6": {workspace: demo, user: w.alice, status: session.StatusCompleted, runID: "20260102_030405_000000", at: t1 + 3*time.Minute},
	} {
		ids[name] = w.store(t, s)
	}
	at := func(d time.Duration, zone *time.Location) string {
		return url.QueryEscape(newClock().Now().Add(d).In(zone).Format(time.RFC3339Nano))
	}
	plus2 := time.FixedZone("+02:00", 2*60*60)
	cases := []struct {
		query string
		want  []string
	}{
		{"workspace=demo", []string{"s6", "s4", "s2", "s1"}},
		{"run_id=" + run, []string{"s3", "s1"}},
		{"status=active", []string{"s4", "s2"}},
		{"status=terminated", []string{"s3"}},
		{"credential_type=pat", []string{"s2"}},
		{"credential_type=admin_token", []string{"s4"}},
		{"since=" + at(t1, time.UTC), []string{"s6", "s4", "s3", "s2"}},
		{"until=" + at(t1, time.UTC), []string{"s1"}},
		{"since=" + at(t1, plus2), []string{"s6", "s4", "s3", "s2"}},
		{"since=" + at(t1+500*time.Millisecond, time.UTC), []string{"s6", "s4", "s3"}},
		{"until=" + at(t1+500*time.Millisecond, time.UTC), []string{"s2", "s1"}},
		{"since=" + at(t1, time.UTC) + "&until=" + at(t1+2*time.Minute, time.UTC), []string{"s3", "s2"}},
		{"since=" + at(t1+time.Hour, time.UTC) + "&until=" + at(t1, time.UTC), nil},
		{"workspace=demo&status=completed&credential_type=api_key", []string{"s6", "s1"}},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			got, _ := w.listed(t, w.aliceKey, c.query)
			var want []string
			for _, name := range c.want {
				want = append(want, ids[name])
			}
			wantIDs(t, got, want)
		})
	}
}

func TestList_IncludesTheSessionsOfTheWorkspacesForksOnRequest(t *testing.T) {
	w := newWorld(t)
	x := w.store(t, stored{workspace: demo, user: w.alice, at: time.Minute})
	f1 := w.store(t, stored{workspace: fork1, parent: demo, user: w.alice, at: 2 * time.Minute})
	f2 := w.store(t, stored{workspace: fork2, parent: demo, user: w.alice, at: 3 * time.Minute})
	w.store(t, stored{workspace: other, user: w.dave, at: 4 * time.Minute})
	w.removeWorkspace(t, fork2)
	for _, caller := range []struct{ name, credential string }{{"alice", w.aliceKey}, {"bob", w.bobKey}} {
		cases := []struct {
			query string
			want  []string
		}{
			{"workspace=demo", []string{x}},
			{"workspace=demo&include_forks=false", []string{x}},
			{"workspace=demo&include_forks=true", []string{f2, f1, x}},
		}
		for _, c := range cases {
			t.Run(caller.name+"/"+c.query, func(t *testing.T) {
				got, _ := w.listed(t, caller.credential, c.query)
				wantIDs(t, got, c.want)
			})
		}
	}
}

func TestList_RefusesAnInvalidParameter(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		query, parameter string
	}{
		{"status=done", "status"},
		{"status=", "status"},
		{"credential_type=key", "credential_type"},
		{"order=up", "order"},
		{"include_forks=yes", "include_forks"},
		{"include_forks=true", "include_forks"},
		{"include_forks=false", "include_forks"},
		{"run_id=abc", "run_id"},
		{"since=yesterday", "since"},
		{"until=2026-13-01T00:00:00Z", "until"},
		{"limit=0", "limit"},
		{"cursor=abc", "cursor"},
		{"workspace=nope&status=done", "status"},
		{"workspace=demo&order=up", "order"},
	}
	for _, c := range cases {
		for _, caller := range []struct{ name, credential string }{{"alice", w.aliceKey}, {"carol", w.carolKey}} {
			t.Run(caller.name+"/"+c.query, func(t *testing.T) {
				status, raw := w.list(t, caller.credential, c.query)
				errorType, message := errorOf(t, raw)
				if status != http.StatusBadRequest || errorType != "invalid_request" || !strings.Contains(message, c.parameter) {
					t.Errorf("GET /sessions?%s = %d %s, want 400 invalid_request naming %s", c.query, status, raw, c.parameter)
				}
			})
		}
	}
}

func TestList_HidesWhetherTheWorkspaceFilterNamesAWorkspace(t *testing.T) {
	w := newWorld(t, func(d *session.Deps) { d.Coverage = boundCoverage{} })
	onFork := w.store(t, stored{workspace: fork1, parent: demo, user: w.alice, at: time.Minute})
	boundToFork := w.pat(t, w.alice, fork1, "sessions:read")
	cases := []struct {
		name, credential, query string
	}{
		{"carol on demo", w.carolKey, "workspace=demo"},
		{"alice on a slug that names no workspace", w.aliceKey, "workspace=nope"},
		{"alice's token bound to demo--t1 on demo", boundToFork, "workspace=demo"},
		{"alice's token bound to demo--t1 on demo with its forks", boundToFork, "workspace=demo&include_forks=true"},
	}
	var bodies [][]byte
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := w.list(t, c.credential, c.query)
			errorType, message := errorOf(t, raw)
			if status != http.StatusNotFound || errorType != "not_found" || message != "workspace not found" {
				t.Fatalf("GET /sessions?%s = %d %s, want 404 not_found with message %q", c.query, status, raw, "workspace not found")
			}
			bodies = append(bodies, raw)
		})
	}
	for i := 1; i < len(bodies); i++ {
		if string(bodies[i]) != string(bodies[0]) {
			t.Errorf("404 bodies differ: %s and %s", bodies[0], bodies[i])
		}
	}
	t.Run("alice's token bound to demo--t1 on demo--t1", func(t *testing.T) {
		got, _ := w.listed(t, boundToFork, "workspace="+fork1)
		wantIDs(t, got, []string{onFork})
	})
}

func TestList_ConfinesABoundTokenToTheWorkspacesItsBindingCovers(t *testing.T) {
	// want are the workspaces whose sessions a token bound to demo--t1
	// lists, and elsewhere those a token bound to a workspace with no
	// session lists.
	cases := []struct {
		name            string
		coverage        session.Coverage
		want, elsewhere []string
	}{
		{"with a coverage that covers demo--t1 alone", boundCoverage{}, []string{fork1}, nil},
		{"with no coverage installed", nil, []string{fork2, fork1, demo}, []string{fork2, fork1, demo}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, func(d *session.Deps) { d.Coverage = c.coverage })
			bySlug := map[string]string{}
			for i, ws := range []string{demo, fork1, fork2} {
				parent := ""
				if ws != demo {
					parent = demo
				}
				bySlug[ws] = w.store(t, stored{workspace: ws, parent: parent, user: w.alice, credential: session.CredentialPAT, at: time.Duration(i) * time.Minute})
			}
			bound := w.pat(t, w.alice, fork1, "sessions:read")
			got, _ := w.listed(t, bound, "")
			var want []string
			for _, ws := range c.want {
				want = append(want, bySlug[ws])
			}
			wantIDs(t, got, want)
			for _, id := range got {
				if status, raw := w.get(t, bound, id); status != http.StatusOK {
					t.Errorf("GET /sessions/%s = %d %s, want 200", id, status, raw)
				}
			}
			got, _ = w.listed(t, w.pat(t, w.alice, "demo--t3", "sessions:read"), "")
			want = nil
			for _, ws := range c.elsewhere {
				want = append(want, bySlug[ws])
			}
			wantIDs(t, got, want)
		})
	}
}
