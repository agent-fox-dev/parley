package policy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// eventCheck is the type of the event a check report records.
const eventCheck = "parley.change.check"

// report puts body as the result of the check name of the change number
// of api, as dana, and fails the test unless it is stored; it returns the
// check as answered.
func (w *world) report(t *testing.T, number int64, name string, body map[string]any) any {
	t.Helper()
	r := w.putCheck(t, number, name, w.danaKey, body)
	if r.status != http.StatusOK {
		t.Fatalf("report %s %v on %d = %d %s, want 200", name, body, number, r.status, r.body)
	}
	return r.json
}

// detailsOf is a details object whose JSON text is size bytes long.
func detailsOf(size int) json.RawMessage {
	return json.RawMessage(`{"pad":"` + strings.Repeat("x", size-len(`{"pad":""}`)) + `"}`)
}

func TestPutCheck_StoresTheResultForTheVersionAndName(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	changes := []struct {
		state  string
		number int64
	}{
		{"open", w.openPushed(t, "open").number},
		{"draft", w.openPushed(t, "draft", func(in *change.OpenInput) { in.Draft = true }).number},
	}
	ci := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"checks:write"}})
	callers := []struct {
		name, credential string
		by               map[string]any
	}{
		{"dana's API key", w.danaKey, map[string]any{"actor_type": "api_key", "actor_id": w.dana, "username": "dana"}},
		{"dana's PAT with checks:write only", ci, map[string]any{"actor_type": "pat", "actor_id": w.dana, "username": "dana"}},
		{"the admin token", w.Token, map[string]any{"actor_type": "admin_token", "actor_id": nil, "username": nil}},
	}
	names := []string{"test", "a", "billing-e2e", "lint.go_1", strings.Repeat("n", 63)}
	bodies := []struct {
		name string
		body map[string]any
	}{
		{"pending", map[string]any{"status": "pending"}},
		{"passed with every member", map[string]any{"status": "passed", "summary": "42 tests", "url": "https://ci.example/run/7",
			"details": map[string]any{"duration_ms": 812}}},
		{"failed on version 1", map[string]any{"status": "failed", "version": 1}},
		{"a summary of 2000 characters", map[string]any{"status": "passed", "summary": strings.Repeat("é", 2000)}},
		{"details of 16384 bytes", map[string]any{"status": "passed", "details": detailsOf(16384)}},
	}
	for _, ch := range changes {
		for _, c := range callers {
			for _, name := range names {
				for _, b := range bodies {
					t.Run(ch.state+", "+c.name+", "+name[:min(len(name), 12)]+", "+b.name, func(t *testing.T) {
						r := w.putCheck(t, ch.number, name, c.credential, b.body)
						if r.status != http.StatusOK {
							t.Fatalf("= %d %s, want 200", r.status, r.body)
						}
						in := asJSON(t, b.body).(map[string]any)
						summary, _ := in["summary"].(string)
						want := map[string]any{
							"name": name, "change": float64(ch.number), "version": 1.0, "status": in["status"],
							"summary": summary, "url": in["url"], "details": in["details"], "reported_by": c.by,
						}
						for k, v := range want {
							if !reflect.DeepEqual(r.json[k], v) {
								t.Errorf("%s = %v, want %v", k, r.json[k], v)
							}
						}
						if at, _ := r.json["updated_at"].(string); !wholeSecondsUTC.MatchString(at) {
							t.Errorf("updated_at = %v, want RFC 3339 in UTC with whole seconds", r.json["updated_at"])
						}
						if len(r.json) != 9 {
							t.Errorf("the check has %d members, want 9: %s", len(r.json), r.body)
						}
						var n int
						if err := w.Store.Main().QueryRow(`SELECT count(*) FROM change_checks
WHERE workspace_slug = 'api' AND number = ? AND version = 1 AND name = ?`, ch.number, name).Scan(&n); err != nil {
							t.Fatal(err)
						}
						if n != 1 {
							t.Errorf("%d results stored for version 1 and %s, want 1", n, name)
						}
					})
				}
			}
		}
	}
	if r := w.get(t, "/api/v1/workspaces/api/changes/1", ci); r.status != http.StatusForbidden || r.errorType() != "insufficient_scope" {
		t.Errorf("read the change with the checks:write PAT = %d %s, want 403 insufficient_scope", r.status, r.body)
	}
	if r := w.postVerdict(t, 1, ci, map[string]any{"verdict": "approve"}); r.status != http.StatusForbidden || r.errorType() != "insufficient_scope" {
		t.Errorf("give a verdict with the checks:write PAT = %d %s, want 403 insufficient_scope", r.status, r.body)
	}
}

func TestPutCheck_ReplacesTheStoredResult(t *testing.T) {
	t.Parallel()
	clock := &stepClock{at: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	w := newWorld(t, func(d *policy.Deps) { d.Now = clock.now })
	n := w.openPushed(t, "work").number
	ci := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"checks:write"}})
	first := w.putCheck(t, n, "test", ci, map[string]any{"status": "pending", "summary": "queued",
		"url": "https://ci.example/1", "details": map[string]any{"a": 1}})
	if first.status != http.StatusOK {
		t.Fatalf("first report = %d %s, want 200", first.status, first.body)
	}
	for range 5 {
		clock.tick()
	}
	r := w.putCheck(t, n, "test", w.Token, map[string]any{"status": "passed"})
	if r.status != http.StatusOK {
		t.Fatalf("replacement = %d %s, want 200", r.status, r.body)
	}
	if r.json["status"] != "passed" || r.json["summary"] != "" || r.json["url"] != nil || r.json["details"] != nil {
		t.Errorf("replacement = %s, want passed with summary '', url and details null", r.body)
	}
	if by := r.json["reported_by"]; !reflect.DeepEqual(by, map[string]any{"actor_type": "admin_token", "actor_id": nil, "username": nil}) {
		t.Errorf("reported_by = %v, want the admin token", by)
	}
	before, err1 := time.Parse(time.RFC3339, first.json["updated_at"].(string))
	after, err2 := time.Parse(time.RFC3339, r.json["updated_at"].(string))
	if err1 != nil || err2 != nil || after.Sub(before) != 5*time.Second {
		t.Errorf("updated_at went from %v to %v, want 5 s later", first.json["updated_at"], r.json["updated_at"])
	}
	want := map[string]any{"status": "passed", "summary": "", "url": nil, "details": nil, "actor_type": "admin_token",
		"actor_id": nil, "username": nil, "updated_at": after.Unix()}
	if got := w.storedResult(t, n, 1, "test"); !reflect.DeepEqual(got, want) {
		t.Errorf("stored result = %v, want %v", got, want)
	}
	if l := w.get(t, checksPath(n), w.danaKey); !reflect.DeepEqual(l.json["items"], []any{r.json}) {
		t.Errorf("listed results = %s, want the replacement alone", l.body)
	}
}

func TestPutCheck_RefusesAndStoresNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	n := w.openPushed(t, "work").number
	forked := w.openTouching(t, "forked", "f.txt", func(in *change.OpenInput) { in.SourceWorkspace = "api--f1" })
	danaNoScope := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"changes:write", "changes:approve"}})
	melCI := w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"checks:write"}})
	boundToFork := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"checks:write"}, Workspace: "api--f1"})
	path := func(slug, number, name string) string {
		return "/api/v1/workspaces/" + slug + "/changes/" + number + "/checks/" + name
	}
	test := path("api", "1", "test")
	passed := map[string]any{"status": "passed"}
	with := func(member string, value any) map[string]any {
		return map[string]any{"status": "passed", member: value}
	}
	cases := []struct {
		name, path, credential string
		body                   any
		status                 int
		errorType              string
	}{
		{"PAT without checks:write", test, danaNoScope, passed, http.StatusForbidden, "insufficient_scope"},
		{"PAT without checks:write, unknown workspace", path("nosuch", "1", "test"), danaNoScope, passed, http.StatusForbidden, "insufficient_scope"},
		{"read-only member's API key", test, w.melKey, passed, http.StatusForbidden, "forbidden"},
		{"read-only member's PAT with checks:write", test, melCI, passed, http.StatusForbidden, "forbidden"},
		{"outsider", test, w.ozKey, passed, http.StatusNotFound, "not_found"},
		{"outsider, unknown workspace", path("nosuch", "1", "test"), w.ozKey, passed, http.StatusNotFound, "not_found"},
		{"token bound to a fork, on its parent, for the fork's change", checkPath(forked, "test"), boundToFork, passed, http.StatusForbidden, "token_bound"},

		{"name with a capital", path("api", "1", "Test"), w.danaKey, passed, http.StatusBadRequest, "invalid_request"},
		{"name starting with a dash", path("api", "1", "-test"), w.danaKey, passed, http.StatusBadRequest, "invalid_request"},
		{"name with a space", path("api", "1", "a%20b"), w.danaKey, passed, http.StatusBadRequest, "invalid_request"},
		{"name of 64 characters", path("api", "1", strings.Repeat("n", 64)), w.danaKey, passed, http.StatusBadRequest, "invalid_request"},
		{"name with a !", path("api", "1", "x!"), w.danaKey, passed, http.StatusBadRequest, "invalid_request"},
		{"no status", test, w.danaKey, map[string]any{}, http.StatusBadRequest, "invalid_request"},
		{"unknown status", test, w.danaKey, map[string]any{"status": "ok"}, http.StatusBadRequest, "invalid_request"},
		{"summary of 2001 characters", test, w.danaKey, with("summary", strings.Repeat("é", 2001)), http.StatusBadRequest, "invalid_request"},
		{"ftp url", test, w.danaKey, with("url", "ftp://ci.example/1"), http.StatusBadRequest, "invalid_request"},
		{"relative url", test, w.danaKey, with("url", "/run/7"), http.StatusBadRequest, "invalid_request"},
		{"url of 2049 characters", test, w.danaKey, with("url", "https://ci.example/"+strings.Repeat("a", 2049-19)), http.StatusBadRequest, "invalid_request"},
		{"details an array", test, w.danaKey, with("details", []any{1, 2}), http.StatusBadRequest, "invalid_request"},
		{"details a string", test, w.danaKey, with("details", "text"), http.StatusBadRequest, "invalid_request"},
		{"details of 16385 bytes", test, w.danaKey, with("details", detailsOf(16385)), http.StatusBadRequest, "invalid_request"},
		{"version 0", test, w.danaKey, with("version", 0), http.StatusBadRequest, "invalid_request"},
		{"version not a number", test, w.danaKey, with("version", "one"), http.StatusBadRequest, "invalid_request"},
		{"unknown member", test, w.danaKey, with("extra", true), http.StatusBadRequest, "invalid_request"},
		{"not an object", test, w.danaKey, `"passed"`, http.StatusBadRequest, "invalid_request"},

		{"unknown change", path("api", "99", "test"), w.danaKey, passed, http.StatusNotFound, "change_not_found"},
		{"number not a number", path("api", "x", "test"), w.danaKey, passed, http.StatusNotFound, "change_not_found"},
		{"unknown change with an invalid name and body", path("api", "99", "Test"), w.danaKey, map[string]any{}, http.StatusNotFound, "change_not_found"},
		{"version the change does not have", test, w.danaKey, with("version", 5), http.StatusNotFound, "not_found"},
	}
	if n != 1 {
		t.Fatalf("the change is number %d, want 1", n)
	}
	seq := w.seq(t)
	bodies := map[string][]byte{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := w.send(t, http.MethodPut, tc.path, tc.credential, tc.body, nil)
			if r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
			}
			bodies[tc.name] = r.body
			if got := w.count(t, "change_checks"); got != 0 {
				t.Errorf("%d results stored, want none", got)
			}
			if evs := w.eventsAfter(t, seq, eventCheck); len(evs) != 0 {
				t.Errorf("%d check events written, want none", len(evs))
			}
		})
	}
	if a, b := bodies["outsider"], bodies["outsider, unknown workspace"]; string(a) != string(b) {
		t.Errorf("the outsider's refusal %s differs from that of an unknown workspace %s", a, b)
	}
}

func TestPutCheck_RefusesAClosedChangeOrAnArchivedWorkspace(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	landed, abandoned, open := w.openPushed(t, "landed").number, w.openPushed(t, "abandoned").number, w.openPushed(t, "open").number
	for _, n := range []int64{landed, abandoned, open} {
		if r := w.putCheck(t, n, "test", w.danaKey, map[string]any{"status": "pending"}); r.status != http.StatusOK {
			t.Fatalf("pending report on %d = %d %s, want 200", n, r.status, r.body)
		}
	}
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.SetLanded(ctx, tx, "api", landed, change.Landed{SHA: w.base, At: time.Now(),
			By: change.Actor{ActorType: change.CredentialAPIKey, ActorID: new(w.dana), Username: new("dana")}, LandID: "l1"}); err != nil {
			return err
		}
		return w.changes.Abandon(ctx, tx, "api", abandoned, "undone", w.danaCaller())
	})
	stored := map[int64]map[string]any{}
	for _, n := range []int64{landed, abandoned, open} {
		stored[n] = w.storedResult(t, n, 1, "test")
	}
	passed := map[string]any{"status": "passed"}
	archive := func() { w.setStatus(t, "api", workspace.StatusArchived) }
	cases := []struct {
		name      string
		before    func()
		number    int64
		body      any
		status    int
		errorType string
	}{
		{"landed change", nil, landed, passed, http.StatusConflict, "change_closed"},
		{"abandoned change", nil, abandoned, passed, http.StatusConflict, "change_closed"},
		{"landed change with an invalid body", nil, landed, map[string]any{"status": "ok"}, http.StatusBadRequest, "invalid_request"},
		{"open change in an archived workspace", archive, open, passed, http.StatusConflict, "workspace_archived"},
		{"landed change in an archived workspace", nil, landed, passed, http.StatusConflict, "workspace_archived"},
	}
	seq := w.seq(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != nil {
				tc.before()
			}
			if r := w.putCheck(t, tc.number, "test", w.danaKey, tc.body); r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("= %d %s, want %d %s", r.status, r.body, tc.status, tc.errorType)
			}
			for n, want := range stored {
				if got := w.storedResult(t, n, 1, "test"); !reflect.DeepEqual(got, want) || got["status"] != "pending" {
					t.Errorf("stored result of %d = %v, want it pending as before (%v)", n, got, want)
				}
			}
			if evs := w.eventsAfter(t, seq, eventCheck); len(evs) != 0 {
				t.Errorf("%d check events written, want none", len(evs))
			}
		})
	}
}

func TestPutCheck_WritesOneEventInTheResultsTransaction(t *testing.T) {
	t.Parallel()
	clock := &stepClock{at: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	probe := &txProbe{visible: map[string]bool{}}
	w := newWorld(t, func(d *policy.Deps) {
		d.Now = clock.now
		probe.EventWriter = d.Events
		d.Events = probe
	})
	n := w.openPushed(t, "work").number
	ci := w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"checks:write"}})
	cases := []struct {
		name, credential, status, actorType string
		actorID                             *string
	}{
		{"first report by dana's PAT", ci, "pending", "pat", new(w.dana)},
		{"replacement by the admin token", w.Token, "passed", "admin_token", nil},
		{"the same report again", w.Token, "passed", "admin_token", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock.tick()
			seq := w.seq(t)
			r := w.putCheck(t, n, "test", tc.credential, map[string]any{"status": tc.status})
			if r.status != http.StatusOK {
				t.Fatalf("= %d %s, want 200", r.status, r.body)
			}
			evs := w.eventsAfter(t, seq, eventCheck)
			if len(evs) != 1 {
				t.Fatalf("%d check events, want 1", len(evs))
			}
			e := evs[0]
			if e.Workspace == nil || *e.Workspace != "api" || e.ResourceType != "change" || e.ResourceID != "1" ||
				e.Action != "check" || e.ActorType != tc.actorType || !reflect.DeepEqual(e.ActorID, tc.actorID) || e.SessionID != nil {
				t.Errorf("event = %+v, want a check of api change 1 by %s %v with no session", e, tc.actorType, tc.actorID)
			}
			var meta map[string]any
			if err := json.Unmarshal(e.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"version": 1.0, "name": "test", "status": tc.status, "number": 1.0, "ref": change.Ref("api", 1)}
			if !reflect.DeepEqual(meta, want) {
				t.Errorf("metadata = %v, want %v", meta, want)
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			at, _ := time.Parse(time.RFC3339, r.json["updated_at"].(string))
			if len(probe.results) == 0 || probe.results[len(probe.results)-1] != [2]any{tc.status, at.Unix()} {
				t.Errorf("the event's transaction read %v, want the result as stored now (%s at %v)", probe.results, tc.status, at)
			}
		})
	}
}

func TestListChecks_ListsTheResultsThatApplyToTheVersionByName(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	files := map[string]string{"work.txt": "work\n"}
	n := w.openPushedFiles(t, "work", files).number
	melRead := w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"changes:read"}})
	on := func(status string, version int) map[string]any {
		return map[string]any{"status": status, "version": version}
	}
	test1, lint1, build1 := w.report(t, n, "test", on("passed", 1)), w.report(t, n, "lint", on("failed", 1)),
		w.report(t, n, "build", on("passed", 1))
	w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, w.danaCaller())
	lint2 := w.report(t, n, "lint", on("passed", 2))
	if r := w.get(t, checksPath(n), melRead); r.status != http.StatusOK || !reflect.DeepEqual(r.json["items"], []any{build1, lint2, test1}) ||
		r.json["next_cursor"] != nil {
		t.Errorf("list of the current version 2 = %d %s, want build and test carried from 1 and lint on 2", r.status, r.body)
	}
	w.edited(t, "work", "work.txt")
	for _, tc := range []struct {
		name, query string
		want        []any
		more        bool
	}{
		{"the current version 3, which reaches none", "", []any{}, false},
		{"version 2, with the results carried from 1", "?version=2", []any{build1, lint2, test1}, false},
		{"version 1", "?version=1", []any{build1, lint1, test1}, false},
		{"version 3", "?version=3", []any{}, false},
		{"a page of two of version 2", "?version=2&limit=2", []any{build1, lint2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := w.get(t, checksPath(n)+tc.query, melRead)
			if r.status != http.StatusOK || !reflect.DeepEqual(r.json["items"], tc.want) || (r.json["next_cursor"] != nil) != tc.more {
				t.Errorf("= %d %s, want %v with a next cursor %v", r.status, r.body, tc.want, tc.more)
			}
		})
	}
	first := w.get(t, checksPath(n)+"?version=2&limit=2", melRead)
	cursor, _ := first.json["next_cursor"].(string)
	next := w.get(t, checksPath(n)+"?version=2&limit=2&cursor="+url.QueryEscape(cursor), melRead)
	if next.status != http.StatusOK || !reflect.DeepEqual(next.json["items"], []any{test1}) || next.json["next_cursor"] != nil {
		t.Errorf("second page of two = %d %s, want the result of test on 1 alone", next.status, next.body)
	}
}
