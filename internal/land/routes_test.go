package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/land"
)

// isUUID reports whether s is a UUID in its 36-character form.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return len(s) == 36 && err == nil
}

// ids are the ids of the items of a list answer, in order.
func ids(t *testing.T, answer map[string]any) []string {
	t.Helper()
	items, ok := answer["items"].([]any)
	if !ok {
		t.Fatalf("answer %v has no items list", answer)
	}
	out := []string{}
	for _, it := range items {
		out = append(out, it.(map[string]any)["id"].(string))
	}
	return out
}

// pages follows a list from path with limit through every next_cursor and
// returns the ids of every page's items, in order.
func (w *world) pages(t *testing.T, path, credential string, limit int) []string {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	out, cursor := []string{}, ""
	for range 10 {
		q := fmt.Sprintf("%s%slimit=%d", path, sep, limit)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		status, answer := w.get(t, q, credential)
		if status != 200 {
			t.Fatalf("GET %s = %d %v", q, status, answer)
		}
		page := ids(t, answer)
		if len(page) > limit {
			t.Fatalf("GET %s answered %d items, want at most %d", q, len(page), limit)
		}
		out = append(out, page...)
		next, _ := answer["next_cursor"].(string)
		if next == "" {
			return out
		}
		cursor = next
	}
	t.Fatalf("GET %s did not end within 10 pages", path)
	return nil
}

func TestChangeLands_ListsTheAttemptsCoveringTheChangeNewestFirst(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, key := w.member(t, "reader")
	a, b, c, none := w.open(t, "a"), w.open(t, "b"), w.open(t, "c"), w.open(t, "none")
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	minute := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	w.insert(t,
		finished(running(attemptID(1), "api", "main", minute(1), a.Number), land.StatusConflict),
		finished(running(attemptID(2), "api", "main", minute(2), a.Number), land.StatusFailed),
		finished(running(attemptID(3), "api", "main", minute(3), a.Number), land.StatusLanded),
		running(attemptID(4), "api", "main", minute(4), b.Number),
		running(attemptID(5), "api", "main", minute(5), a.Number, c.Number),
	)
	path := func(n int64) string { return fmt.Sprintf("/workspaces/api/changes/%d/lands", n) }

	status, answer := w.get(t, path(a.Number), key)
	want := []string{attemptID(5), attemptID(3), attemptID(2), attemptID(1)}
	if status != 200 || !reflect.DeepEqual(ids(t, answer), want) || answer["next_cursor"] != nil {
		t.Fatalf("GET %s = %d %v, want items %v and no next_cursor", path(a.Number), status, answer, want)
	}
	for _, it := range answer["items"].([]any) {
		id := it.(map[string]any)["id"].(string)
		stored, _, err := land.ReadAttempt(context.Background(), w.Store, "api", id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(it, asJSON(t, stored)) {
			t.Errorf("item %v, want the attempt object %v", it, asJSON(t, stored))
		}
	}
	if got := w.pages(t, path(a.Number), key, 2); !reflect.DeepEqual(got, want) {
		t.Errorf("pages of 2 = %v, want %v", got, want)
	}

	for _, tc := range []struct {
		number int64
		want   []string
	}{{b.Number, []string{attemptID(4)}}, {c.Number, []string{attemptID(5)}}, {none.Number, []string{}}} {
		if status, answer := w.get(t, path(tc.number), key); status != 200 || !reflect.DeepEqual(ids(t, answer), tc.want) {
			t.Errorf("GET %s = %d %v, want items %v", path(tc.number), status, answer, tc.want)
		}
	}
	for _, p := range []string{path(99), "/workspaces/api/changes/x/lands", "/workspaces/api/changes/0/lands"} {
		status, answer := w.get(t, p, key)
		if errObj, _ := answer["error"].(map[string]any); status != 404 || errObj["error_type"] != "change_not_found" {
			t.Errorf("GET %s = %d %v, want 404 change_not_found", p, status, answer)
		}
	}
}

func TestWorkspaceLands_ListsTheWorkspacesAttemptsNarrowedByEveryFilter(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, key := w.member(t, "reader")
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	minute := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	// Two attempts are shown at the same second, where the id orders them,
	// whatever their fraction of a second and the order they were made in.
	w.insert(t,
		finished(running(attemptID(1), "api", "main", minute(1), 1), land.StatusLanded),
		finished(running(attemptID(2), "api", "main", minute(2), 2), land.StatusConflict),
		finished(running(attemptID(3), "api", "release/1", minute(3), 3), land.StatusBlocked),
		finished(running(attemptID(4), "api", "main", minute(4), 4), land.StatusFailed),
		running(attemptID(5), "api", "release/1", minute(5), 5),
		finished(running(attemptID(7), "api", "release/1", minute(6).Add(100*time.Millisecond), 6), land.StatusLanded),
		finished(running(attemptID(6), "api", "main", minute(6).Add(500*time.Millisecond), 7), land.StatusLanded),
		finished(running(attemptID(8), "other", "main", minute(7), 1), land.StatusLanded),
		finished(running(attemptID(9), "api--f1", "main", minute(7), 1), land.StatusLanded),
	)
	since := url.QueryEscape(minute(4).Format(time.RFC3339))
	cases := []struct {
		query string
		want  []int
	}{
		{"", []int{7, 6, 5, 4, 3, 2, 1}},
		{"status=landed", []int{7, 6, 1}},
		{"status=conflict&status=failed", []int{4, 2}},
		{"target_branch=release/1", []int{7, 5, 3}},
		{"since=" + since, []int{7, 6, 5, 4}},
		{"since=" + url.QueryEscape(minute(4).Add(time.Second).Format(time.RFC3339)), []int{7, 6, 5}},
		{"status=landed&target_branch=main&since=" + url.QueryEscape(minute(2).Format(time.RFC3339)), []int{6}},
		{"status=running&status=blocked&target_branch=main", []int{}},
	}
	for _, tc := range cases {
		want := []string{}
		for _, n := range tc.want {
			want = append(want, attemptID(n))
		}
		path := "/workspaces/api/lands?" + tc.query
		status, answer := w.get(t, path, key)
		if status != 200 || !reflect.DeepEqual(ids(t, answer), want) || answer["next_cursor"] != nil {
			t.Errorf("GET %s = %d %v, want items %v", path, status, answer, want)
		}
		if got := w.pages(t, path, key, 2); !reflect.DeepEqual(got, want) {
			t.Errorf("pages of 2 of %s = %v, want %v", path, got, want)
		}
	}
}

func TestWorkspaceLands_RefusesAnInvalidQuery(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, key := w.member(t, "reader")
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	w.insert(t, running(attemptID(1), "api", "main", at, 1), running(attemptID(2), "api", "main", at.Add(time.Minute), 2))
	_, first := w.get(t, "/workspaces/api/lands?status=running&limit=1", key)
	foreign, _ := first["next_cursor"].(string)
	if foreign == "" {
		t.Fatalf("a page of one of two attempts has no next_cursor: %v", first)
	}
	for _, query := range []string{
		"status=done", "status=landed&status=nope", "since=yesterday", "since=2026-13-01T00:00:00Z",
		"limit=0", "limit=201", "cursor=garbage", "status=landed&cursor=" + url.QueryEscape(foreign),
	} {
		status, answer := w.get(t, "/workspaces/api/lands?"+query, key)
		if errObj, _ := answer["error"].(map[string]any); status != 400 || errObj["error_type"] != "invalid_request" {
			t.Errorf("GET …/lands?%s = %d %v, want 400 invalid_request", query, status, answer)
		}
	}
}

func TestLand_AnswersTheWorkspacesAttemptByID(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, key := w.member(t, "reader")
	w.addWorkspace(t, "other")
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	landed := finished(running(attemptID(1), "api", "main", at, 1, 2), land.StatusLanded)
	landed.SessionID = new("11111111-1111-4111-8111-111111111111")
	inOther := running(attemptID(2), "other", "main", at, 1)
	run := running(attemptID(3), "api", "main", at, 3)
	w.insert(t, landed, inOther, run)

	status, answer := w.get(t, "/workspaces/api/lands/"+landed.ID, key)
	if want := asJSON(t, landed); status != 200 || !reflect.DeepEqual(answer, want) {
		t.Errorf("GET …/lands/%s = %d %v, want 200 %v", landed.ID, status, answer, want)
	}
	if commits, _ := answer["commits"].([]any); len(commits) != 2 {
		t.Errorf("commits = %v, want one per covered change", answer["commits"])
	}
	if status, _ := w.get(t, "/workspaces/other/lands/"+inOther.ID, key); status != 200 {
		t.Errorf("GET other's attempt on other = %d, want 200", status)
	}
	for _, id := range []string{attemptID(42), "not-a-uuid", inOther.ID} {
		status, answer := w.get(t, "/workspaces/api/lands/"+id, key)
		if errObj, _ := answer["error"].(map[string]any); status != 404 || errObj["error_type"] != "land_not_found" {
			t.Errorf("GET …/api/lands/%s = %d %v, want 404 land_not_found", id, status, answer)
		}
	}

	path := "/workspaces/api/lands/" + run.ID
	if _, before := w.get(t, path, key); before["status"] != land.StatusRunning || before["finished_at"] != nil {
		t.Errorf("a running attempt reads %v, want running with no finished_at", before)
	}
	ended := finished(run, land.StatusFailed)
	if err := land.FinishAttempt(context.Background(), w.Store, ended); err != nil {
		t.Fatal(err)
	}
	_, after := w.get(t, path, key)
	if want := asJSON(t, ended); !reflect.DeepEqual(after, want) {
		t.Errorf("the ended attempt reads %v, want %v", after, want)
	}
	if err := land.FinishAttempt(context.Background(), w.Store, finished(run, land.StatusLanded)); err == nil {
		t.Error("a second finish of the attempt was accepted")
	}
	if _, again := w.get(t, path, key); !reflect.DeepEqual(again, after) {
		t.Errorf("the final attempt reads %v on a later request, had %v", again, after)
	}
}

func TestLand_RefusesATokenWithoutTheRoutesScope(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ch := w.open(t, "f")
	reader := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"changes:read"}})
	gitOnly := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:read"}})
	for _, slug := range []string{"api", "nope"} {
		for _, c := range []struct{ method, path, credential string }{
			{http.MethodPost, fmt.Sprintf("/workspaces/%s/changes/%d/land", slug, ch.Number), reader},
			{http.MethodGet, fmt.Sprintf("/workspaces/%s/changes/%d/lands", slug, ch.Number), gitOnly},
			{http.MethodGet, "/workspaces/" + slug + "/lands", gitOnly},
			{http.MethodGet, "/workspaces/" + slug + "/lands/" + attemptID(1), gitOnly},
		} {
			status, raw := w.Call(t, c.method, "/api/v1"+c.path, c.credential, nil)
			var answer map[string]any
			_ = json.Unmarshal(raw, &answer)
			if status != 403 || errorType(answer) != "insufficient_scope" {
				t.Errorf("%s %s = %d %s, want 403 insufficient_scope", c.method, c.path, status, raw)
			}
		}
	}
	if n := w.attempts(t); n != 0 {
		t.Errorf("%d land attempts, want none", n)
	}
}

func TestLand_AdmitsAWriterOfTheTargetAndTheAuthorTheSettingAllows(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	authorID, authorKey := w.member(t, "member-a")
	_, otherKey := w.member(t, "member-b")
	strangerKey := w.IssueKey(t, w.CreateUser(t, "stranger"))
	dir := w.fork(t, "api--a", authorID)
	author := change.Caller{UserID: authorID, Username: "member-a", Credential: change.Credential{Type: change.CredentialAPIKey, ID: "ka"}}
	for i, c := range []struct {
		name, credential string
		byAuthor         bool
		status           int
		errorType        string
	}{
		{"the owner", w.ownerKey, true, 200, ""},
		{"the admin token", w.Token, true, 200, ""},
		{"the author while land.by_author is true", authorKey, true, 200, ""},
		{"the author while land.by_author is false", authorKey, false, 403, "forbidden"},
		{"a member who is not the author", otherKey, true, 403, "forbidden"},
		{"a user who cannot read the workspace", strangerKey, true, 404, "not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.configure(t, fmt.Sprintf(`{"changes.auto_restack":false,"land.by_author":%t}`, c.byAuthor))
			ch := w.openFrom(t, "api--a", dir, "f"+strconv.Itoa(i), author)
			before, main := w.attempts(t), head(t, w.api, "main")
			status, raw := w.post(t, c.credential, "api", ch.Number, "", nil)
			var answer map[string]any
			_ = json.Unmarshal(raw, &answer)
			if status != c.status || errorType(answer) != c.errorType {
				t.Fatalf("POST land = %d %s, want %d %s", status, raw, c.status, c.errorType)
			}
			if status == 200 {
				if answer["status"] != land.StatusLanded {
					t.Errorf("the attempt is %v, want landed", answer["status"])
				}
				return
			}
			if after := w.attempts(t); after != before {
				t.Errorf("a refused land made %d attempts", after-before)
			}
			if got := head(t, w.api, "main"); got != main {
				t.Errorf("a refused land moved main from %s to %s", main, got)
			}
			if status == 404 {
				if _, nope := w.post(t, c.credential, "nope", ch.Number, "", nil); string(nope) != string(raw) {
					t.Errorf("the answer on api %s differs from the one on nope %s", raw, nope)
				}
			}
		})
	}
}

func TestLand_ConfinesATokenBoundToAForkToTheForksChanges(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	f, g := w.fork(t, "api--f", w.owner), w.fork(t, "api--g", w.owner)
	ofF, ofG := w.openFrom(t, "api--f", f, "f1", w.caller()), w.openFrom(t, "api--g", g, "g1", w.caller())
	above := w.openFrom(t, "api--f", f, "f2", w.caller())
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		return w.changes.SetDependsOn(ctx, tx, "api", above.Number, &ofG.Number)
	}); err != nil {
		t.Fatal(err)
	}
	onlyG := finished(running(attemptID(1), "api", "main", time.Now().UTC(), ofG.Number), land.StatusFailed)
	onlyG.Covered[0].SourceWorkspace = "api--g"
	w.insert(t, onlyG)
	bound := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"changes:read", "changes:write"}, Workspace: "api--f"})

	status, landed := w.landCall(t, bound, ofF.Number, "", nil)
	if status != 200 || landed["status"] != land.StatusLanded {
		t.Fatalf("the bound token's land of its fork's change = %d %v, want 200 landed", status, landed)
	}
	before := w.attempts(t)
	for _, c := range []struct {
		name, method, path string
		body               any
		status             int
		errorType          string
	}{
		{"the attempts of the fork's change", http.MethodGet, fmt.Sprintf("changes/%d/lands", ofF.Number), nil, 200, ""},
		{"the attempt that landed it", http.MethodGet, "lands/" + landed["id"].(string), nil, 200, ""},
		{"the land of another fork's change", http.MethodPost, fmt.Sprintf("changes/%d/land", ofG.Number), nil, 403, "token_bound"},
		{"the attempts of another fork's change", http.MethodGet, fmt.Sprintf("changes/%d/lands", ofG.Number), nil, 403, "token_bound"},
		{"an attempt of another fork's change only", http.MethodGet, "lands/" + onlyG.ID, nil, 403, "token_bound"},
		{"a stack whose lower change is another fork's", http.MethodPost, fmt.Sprintf("changes/%d/land", above.Number), map[string]any{"stack": true}, 403, "token_bound"},
		{"the workspace's attempts", http.MethodGet, "lands", nil, 403, "token_bound"},
		{"an unknown attempt", http.MethodGet, "lands/" + attemptID(42), nil, 404, "land_not_found"},
		{"an unknown change", http.MethodPost, "changes/999/land", nil, 404, "change_not_found"},
	} {
		status, raw := w.Call(t, c.method, "/api/v1/workspaces/api/"+c.path, bound, c.body)
		var answer map[string]any
		_ = json.Unmarshal(raw, &answer)
		if status != c.status || errorType(answer) != c.errorType {
			t.Errorf("%s: %s %s = %d %s, want %d %s", c.name, c.method, c.path, status, raw, c.status, c.errorType)
		}
	}
	if after := w.attempts(t); after != before {
		t.Errorf("the refused lands made %d attempts", after-before)
	}
}

func TestLand_RefusesARequestItCannotTakeAndCreatesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ch := w.open(t, "f")
	main := head(t, w.api, "main")
	for _, c := range []struct {
		number          any
		query           string
		body            any
		status          int
		errorType, path string
	}{
		{number: 999, status: 404, errorType: "change_not_found"},
		{number: "abc", status: 404, errorType: "change_not_found"},
		{path: "/workspaces/api/changes/999/lands", status: 404, errorType: "change_not_found"},
		{body: map[string]any{"expected_version": 0}},
		{body: map[string]any{"expected_version": "x"}},
		{body: map[string]any{"expected_version": 1.5}},
		{body: map[string]any{"stack": "yes"}},
		{body: map[string]any{"override": map[string]any{}}},
		{body: map[string]any{"override": map[string]any{"reason": ""}}},
		{body: map[string]any{"override": map[string]any{"reason": "   "}}},
		{body: map[string]any{"override": map[string]any{"reason": 7}}},
		{body: map[string]any{"override": "text"}},
		{body: map[string]any{"session_id": 5}},
		{body: "[]"},
		{query: "wait=-1"},
		{query: "wait=61"},
		{query: "wait=abc"},
		{query: "wait=1.5"},
		{query: "wait="},
	} {
		if c.status == 0 {
			c.status, c.errorType = 400, "invalid_request"
		}
		if c.number == nil {
			c.number = ch.Number
		}
		var status int
		var answer map[string]any
		if c.path != "" {
			status, answer = w.get(t, c.path, w.ownerKey)
		} else {
			status, answer = w.landCall(t, w.ownerKey, c.number, c.query, c.body)
		}
		if status != c.status || errorType(answer) != c.errorType {
			t.Errorf("land %v?%s %v = %d %v, want %d %s", c.number, c.query, c.body, status, answer, c.status, c.errorType)
		}
	}
	if n, got := w.attempts(t), head(t, w.api, "main"); n != 0 || got != main {
		t.Errorf("the refused requests made %d attempts and moved main to %s from %s", n, got, main)
	}

	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = 'archived' WHERE slug = 'api'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status, answer := w.landCall(t, w.ownerKey, ch.Number, "", nil); status != 409 || errorType(answer) != "workspace_archived" {
		t.Errorf("land in an archived workspace = %d %v, want 409 workspace_archived", status, answer)
	}
	if n := w.attempts(t); n != 0 {
		t.Errorf("the land in an archived workspace made %d attempts", n)
	}
}

func TestLand_TakesTheWaitsAtTheEndsOfItsRange(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, query := range []string{"wait=0", "wait=60"} {
		ch := w.open(t, "w"+strings.TrimPrefix(query, "wait="))
		if status, answer := w.landCall(t, w.ownerKey, ch.Number, query, map[string]any{}); status != 200 && status != 202 {
			t.Errorf("land ?%s = %d %v, want 200 or 202", query, status, answer)
		}
	}
}

func TestLand_RefusesASessionTheLinkingRuleRefuses(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	other := w.CreateUser(t, "other")
	dir := w.fork(t, "api--f", w.owner)
	ch := w.openFrom(t, "api--f", dir, "f", w.caller())
	unknown, others, unrelated, own := uuid.New().String(), uuid.New().String(), uuid.New().String(), uuid.New().String()
	w.sessions.add(others, other, "api")
	w.sessions.add(unrelated, w.owner, "web")
	w.sessions.add(own, w.owner, "api--f")
	for _, id := range []string{unknown, others, unrelated} {
		status, answer := w.landCall(t, w.ownerKey, ch.Number, "wait=0", map[string]any{"session_id": id})
		if status != 400 || errorType(answer) != "unknown_session" {
			t.Errorf("land in session %s = %d %v, want 400 unknown_session", id, status, answer)
		}
	}
	if n := w.attempts(t); n != 0 {
		t.Fatalf("the refused sessions made %d attempts", n)
	}

	w.land.RegisterSessionLinker(nil)
	if status, answer := w.landCall(t, w.ownerKey, ch.Number, "wait=0", map[string]any{"session_id": own}); status != 400 ||
		errorType(answer) != "unknown_session" {
		t.Errorf("land in a session with no session linker = %d %v, want 400 unknown_session", status, answer)
	}
	w.land.RegisterSessionLinker(w.sessions)

	status, answer := w.landCall(t, w.ownerKey, ch.Number, "", map[string]any{"session_id": own})
	if status != 200 || answer["session_id"] != own {
		t.Fatalf("land in the caller's session of the source fork = %d %v, want 200 in %s", status, answer, own)
	}
	if want := []string{own + " api " + answer["id"].(string)}; !reflect.DeepEqual(w.sessions.recorded(), want) {
		t.Errorf("links %v, want %v", w.sessions.recorded(), want)
	}
}

func TestLand_CreatesARunningAttemptBeforeAnyReplay(t *testing.T) {
	t.Parallel()
	w := newWorld(t, func(d *land.Deps) { d.Paused = true })
	w.open(t, "one")
	two, three := w.open(t, "two"), w.open(t, "three")
	s := uuid.New().String()
	w.sessions.add(s, w.owner, "api")

	status, a := w.landCall(t, w.ownerKey, three.Number, "wait=0", map[string]any{"session_id": s})
	if status != 202 {
		t.Fatalf("land = %d %v, want 202", status, a)
	}
	if id, _ := a["id"].(string); !isUUID(id) {
		t.Errorf("id %v is no UUID", a["id"])
	}
	want := map[string]any{"workspace_slug": "api", "target_branch": "main", "changes": []any{float64(three.Number)},
		"status": "running", "trigger": "manual", "strategy": "rebase", "attempts": float64(0), "noop": false,
		"commits": []any{}, "session_id": s, "onto_sha": nil, "landed_sha": nil, "conflict": nil, "blocked": nil,
		"error": nil, "override": nil, "finished_at": nil, "policy_revision": nil,
		"requested_by": map[string]any{"actor_type": "api_key", "actor_id": w.owner, "username": "owner"}}
	for k, v := range want {
		if got, ok := a[k]; !ok || !reflect.DeepEqual(got, v) {
			t.Errorf("%s = %v (present %t), want %v", k, got, ok, v)
		}
	}
	if created, _ := a["created_at"].(string); !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`).MatchString(created) {
		t.Errorf("created_at %v is not RFC 3339 UTC in whole seconds", a["created_at"])
	}
	if got := w.attempt(t, a["id"].(string)); got.Status != land.StatusRunning || !reflect.DeepEqual(asJSON(t, got), a) {
		t.Errorf("the store holds %v, answered %v", asJSON(t, got), a)
	}
	if _, read := w.get(t, "/workspaces/api/lands/"+a["id"].(string), w.ownerKey); !reflect.DeepEqual(read, a) {
		t.Errorf("GET the attempt = %v, want %v", read, a)
	}

	w.configure(t, `{"changes.auto_restack":false,"land.strategy":"squash"}`)
	if status, b := w.landCall(t, w.ownerKey, two.Number, "wait=0", nil); status != 202 || b["strategy"] != "squash" {
		t.Errorf("land under land.strategy squash = %d, strategy %v; want 202 squash", status, b["strategy"])
	}

	auto, err := w.land.Start(context.Background(), land.StartInput{
		Workspace: "api", Number: 1, Trigger: land.TriggerAutoLand,
		RequestedBy: change.Actor{ActorType: change.CredentialAPIKey, ActorID: new(w.owner), Username: new("owner")},
		Caller:      w.caller(), Override: &land.Override{Reason: "auto"},
	})
	if err != nil || auto.Trigger != land.TriggerAutoLand || auto.Status != land.StatusRunning || !reflect.DeepEqual(auto.Numbers(), []int64{1}) {
		t.Fatalf("Start = %+v, %v; want a running auto_land attempt of change 1", auto, err)
	}
	if got := w.attempt(t, auto.ID); got.Trigger != land.TriggerAutoLand || *got.RequestedBy.ActorID != w.owner ||
		got.RequestedOverride == nil || got.RequestedOverride.Reason != "auto" || got.Override != nil {
		t.Errorf("the store holds %+v, want the auto_land attempt requested by the owner with the override asked, none applied", got)
	}
}

func TestLand_AnswersARunningAttemptOnceTheWaitRanOut(t *testing.T) {
	t.Parallel()
	clk := newClock()
	w := newWorld(t, func(d *land.Deps) { d.After = clk.After })
	release, parked := make(chan struct{}), make(chan string, 8)
	land.SetSeams(w.land, land.Seams{BeforeSwap: func(_ context.Context, id string, try int) {
		if try == 1 {
			parked <- id
			<-release
		}
	}})
	type answer struct {
		status int
		body   map[string]any
	}
	send := func(ch change.Change, query string) <-chan answer {
		out := make(chan answer, 1)
		go func() {
			status, raw := w.post(t, w.ownerKey, "api", ch.Number, query, nil)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			out <- answer{status, body}
		}()
		return out
	}
	var ids []string
	for _, c := range []struct {
		query string
		// asked is the wait the request asks the clock for, 0 for none.
		asked time.Duration
	}{{"wait=0", 0}, {"wait=2", 2 * time.Second}, {"", 30 * time.Second}} {
		ch := w.open(t, "w"+strconv.Itoa(len(ids)))
		got := send(ch, c.query)
		if c.asked > 0 {
			if d := clk.waitAsked(t); d != c.asked {
				t.Fatalf("?%s waits %v, want %v", c.query, d, c.asked)
			}
			clk.advance(c.asked - time.Second)
			select {
			case a := <-got:
				t.Fatalf("?%s answered %d %v before its wait ran out", c.query, a.status, a.body)
			case <-time.After(200 * time.Millisecond):
			}
			clk.advance(time.Second)
		}
		var a answer
		select {
		case a = <-got:
		case <-time.After(30 * time.Second):
			t.Fatalf("?%s was not answered within 30 s", c.query)
		}
		if id, _ := a.body["id"].(string); a.status != 202 || a.body["status"] != land.StatusRunning || id == "" {
			t.Fatalf("?%s = %d %v, want 202 running", c.query, a.status, a.body)
		}
		ids = append(ids, a.body["id"].(string))
	}

	// A caller that goes away while it waits leaves its attempt running.
	ch := w.open(t, "gone")
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL(fmt.Sprintf("/api/v1/workspaces/api/changes/%d/land", ch.Number)), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.ownerKey)
	sent := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
		sent <- err
	}()
	clk.waitAsked(t)
	cancel()
	if err := <-sent; err == nil {
		t.Fatal("the cancelled request was answered")
	}
	for range 4 {
		select {
		case id := <-parked:
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("fewer than 4 attempts reached the swap within 30 s")
		}
	}

	close(release)
	for _, id := range ids {
		if a := w.ended(t, id); a.Status != land.StatusLanded {
			t.Errorf("attempt %s ended %s (%v), want landed", id, a.Status, a.Error)
		}
		if _, got := w.get(t, "/workspaces/api/lands/"+id, w.ownerKey); got["status"] != land.StatusLanded {
			t.Errorf("GET attempt %s reads %v, want landed", id, got["status"])
		}
	}
}

func TestLand_RefusesALandWhosePreconditionsFailWithTheFirstThatFails(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i, c := range []struct {
		name string
		// state is the state the change is put in, "" for open; it is at
		// version 2.
		state string
		// expected is the request's expected_version, 0 for none.
		expected int64
		// running stores a running attempt covering the change; below
		// makes it depend on an open change.
		running, below bool
		want           string
	}{
		{name: "a draft change", state: change.StateDraft, want: "change_not_open"},
		{name: "a landed change", state: change.StateLanded, want: "change_not_open"},
		{name: "an abandoned change", state: change.StateAbandoned, want: "change_not_open"},
		{name: "an earlier version expected", expected: 1, want: "version_mismatch"},
		{name: "an open change below", below: true, want: "depends_on_unlanded"},
		{name: "a draft change at another version than expected", state: change.StateDraft, expected: 1, want: "change_not_open"},
		{name: "another version expected while an attempt runs", expected: 1, running: true, want: "version_mismatch"},
		{name: "an attempt running with an open change below", running: true, below: true, want: "land_in_progress"},
		{name: "another version expected with an open change below", expected: 1, below: true, want: "version_mismatch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch := "p" + strconv.Itoa(i)
			ch := w.open
			if c.state == change.StateDraft {
				ch = w.openDraft
			}
			named := w.nextVersion(t, ch(t, branch))
			switch c.state {
			case change.StateLanded:
				w.landNow(t, named, w.caller(), nil)
			case change.StateAbandoned:
				w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
					return w.changes.SetAbandoned(ctx, tx, "api", named.Number, "dropped")
				})
			}
			var runs string
			if c.running {
				a := running(uuid.New().String(), "api", "main", time.Now().UTC(), named.Number)
				w.insert(t, a)
				runs = a.ID
			}
			if c.below {
				below := w.open(t, branch+"-below")
				w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
					return w.changes.SetDependsOn(ctx, tx, "api", named.Number, &below.Number)
				})
			}
			body := map[string]any{}
			if c.expected > 0 {
				body["expected_version"] = c.expected
			}
			before, main := w.attempts(t), head(t, w.api, "main")
			status, answer := w.landCall(t, w.ownerKey, named.Number, "", body)
			if status != 409 || errorType(answer) != c.want {
				t.Fatalf("land = %d %v, want 409 %s", status, answer, c.want)
			}
			details, has := answer["error"].(map[string]any)["details"]
			if c.want == "land_in_progress" {
				if d, _ := details.(map[string]any); d["id"] != runs || d["status"] != land.StatusRunning {
					t.Errorf("details %v, want the running attempt %s", details, runs)
				}
			} else if has {
				t.Errorf("the %s answer has details %v", c.want, details)
			}
			if n, got := w.attempts(t), head(t, w.api, "main"); n != before || got != main {
				t.Errorf("the refused land made %d attempts and moved main to %s from %s", n-before, got, main)
			}
		})
	}

	at2 := w.nextVersion(t, w.open(t, "v"))
	if status, a := w.landCall(t, w.ownerKey, at2.Number, "", map[string]any{"expected_version": 2}); status != 200 ||
		a["status"] != land.StatusLanded {
		t.Errorf("land expecting the current version = %d %v, want 200 landed", status, a)
	}
	one, two := w.open(t, "one"), w.open(t, "two")
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetDependsOn(ctx, tx, "api", two.Number, &one.Number)
	})
	for _, n := range []int64{one.Number, two.Number} {
		if status, a := w.landCall(t, w.ownerKey, n, "", nil); status != 200 || a["status"] != land.StatusLanded {
			t.Errorf("land of %d with every change below landed = %d %v, want 200 landed", n, status, a)
		}
	}
}

func TestLand_RefusesALandOfAChangeARunningAttemptCovers(t *testing.T) {
	t.Parallel()
	w := newWorld(t, func(d *land.Deps) { d.Paused = true })
	ch := w.open(t, "f")
	status, a := w.landCall(t, w.ownerKey, ch.Number, "wait=0", nil)
	if status != 202 {
		t.Fatalf("the first land = %d %v, want 202", status, a)
	}
	status, r := w.landCall(t, w.ownerKey, ch.Number, "wait=0", nil)
	errObj, _ := r["error"].(map[string]any)
	details, _ := errObj["details"].(map[string]any)
	if status != 409 || errorType(r) != "land_in_progress" || details["id"] != a["id"] || details["status"] != land.StatusRunning {
		t.Fatalf("the second land = %d %v, want 409 land_in_progress naming the running attempt %v", status, r, a["id"])
	}

	other := w.open(t, "g")
	start, answers := make(chan struct{}), make(chan string, 8)
	for range 8 {
		go func() {
			<-start
			status, raw := w.post(t, w.ownerKey, "api", other.Number, "wait=0", nil)
			var answer map[string]any
			_ = json.Unmarshal(raw, &answer)
			answers <- fmt.Sprintf("%d %s", status, errorType(answer))
		}()
	}
	close(start)
	got := map[string]int{}
	for range 8 {
		select {
		case s := <-answers:
			got[s]++
		case <-time.After(30 * time.Second):
			t.Fatalf("not every land was answered within 30 s: %v", got)
		}
	}
	if want := map[string]int{"202 ": 1, "409 land_in_progress": 7}; !reflect.DeepEqual(got, want) {
		t.Errorf("8 simultaneous lands answered %v, want %v", got, want)
	}
	if _, lands := w.get(t, fmt.Sprintf("/workspaces/api/changes/%d/lands", other.Number), w.ownerKey); len(ids(t, lands)) != 1 {
		t.Errorf("change %d has attempts %v, want one", other.Number, ids(t, lands))
	}
}

func TestLand_AnswersAConflictWithItsReportAndLeavesTheTargetWhereItWas(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	base := commit(t, w.api, "main", w.base, "f", map[string]string{"f.txt": "l1\nl2\nl3\n"})
	commit(t, w.api, "one", base, "one", map[string]string{"f.txt": "l1\nl2\none\n"})
	theirs := commit(t, w.api, "two", base, "two", map[string]string{"f.txt": "l1\nl2\ntwo\n"})
	one, two := w.openBranch(t, "one", w.caller()), w.openBranch(t, "two", w.caller())
	if status, r := w.landCall(t, w.ownerKey, one.Number, "", nil); status != 200 {
		t.Fatalf("land of change 1 = %d %v, want 200", status, r)
	}
	// The check the move of main scheduled stores its own report first,
	// which the land's would otherwise race with.
	w.mergeable(t, two.Number, change.MergeableConflict)
	target, refs := head(t, w.api, "main"), gitrepotest.Refs(t, w.api, "refs/")

	status, r := w.landCall(t, w.ownerKey, two.Number, "", nil)

	if status != 409 || errorType(r) != "land_conflict" {
		t.Fatalf("land of change 2 = %d %v, want 409 land_conflict", status, r)
	}
	a, _ := r["error"].(map[string]any)["details"].(map[string]any)
	if a["status"] != land.StatusConflict || a["landed_sha"] != nil || a["finished_at"] == nil {
		t.Errorf("the attempt is %v, landed %v, finished %v; want conflict, nothing landed, finished",
			a["status"], a["landed_sha"], a["finished_at"])
	}
	k, _ := a["conflict"].(map[string]any)
	for member, want := range map[string]any{"target_sha": target, "head_sha": theirs, "base_sha": base, "commit_sha": theirs,
		"change": nil, "landed_changes": []any{map[string]any{"number": float64(one.Number), "title": one.Title,
			"landed_sha": target, "session_id": nil}}} {
		if !reflect.DeepEqual(k[member], want) {
			t.Errorf("the report's %s is %v, want %v", member, k[member], want)
		}
	}
	files, _ := k["files"].([]any)
	file, _ := files[0].(map[string]any)
	if len(files) != 1 || file["path"] != "f.txt" || file["kind"] != "content" ||
		!reflect.DeepEqual(file["landed_by"], []any{float64(one.Number)}) {
		t.Errorf("the report's files are %v, want f.txt, content, landed by %d", files, one.Number)
	}
	for _, side := range []string{"base", "ours", "theirs"} {
		if s, _ := file[side].(map[string]any); s["sha"] == nil || s["mode"] == nil || s["path"] != "f.txt" {
			t.Errorf("the %s side is %v, want f.txt with its sha and mode", side, file[side])
		}
	}
	if got := head(t, w.api, "main"); got != target {
		t.Errorf("main moved to %s from %s", got, target)
	}
	if got := gitrepotest.Refs(t, w.api, "refs/"); !reflect.DeepEqual(got, refs) {
		t.Errorf("the refs are %v after the land, want %v", got, refs)
	}
	_, ch := w.get(t, fmt.Sprintf("/workspaces/api/changes/%d", two.Number), w.ownerKey)
	if ch["state"] != change.StateOpen || ch["mergeable"] != change.MergeableConflict || !reflect.DeepEqual(ch["conflict"], any(k)) {
		t.Errorf("change 2 reads %v, mergeable %v, conflict %v; want open, conflict, the land's report",
			ch["state"], ch["mergeable"], ch["conflict"])
	}
	fails := w.landEvents(t, land.EventLandFail, a["id"].(string))
	if len(fails) != 1 {
		t.Fatalf("%d land_fail events, want 1", len(fails))
	}
	if m := metadata(t, fails[0]); m["status"] != land.StatusConflict || m["reason"] != "conflict" ||
		!reflect.DeepEqual(m["files"], []any{"f.txt"}) {
		t.Errorf("land_fail metadata %v, want conflict, conflict, files [f.txt]", m)
	}
}

func TestLand_AnswersAFinalAttemptInTheFormOfItsStatus(t *testing.T) {
	t.Parallel()
	w := newWorld(t, func(d *land.Deps) { d.MaxAttempts = 1 })
	gate := &stubGate{}
	w.land.RegisterGate(gate)
	var contend atomic.Bool
	land.SetSeams(w.land, land.Seams{BeforeSwap: func(context.Context, string, int) {
		if contend.Load() {
			commit(t, w.api, "main", head(t, w.api, "main"), "moved", map[string]string{"m.txt": "m\n"})
		}
	}})
	unmet := json.RawMessage(`{"kind":"approvals","satisfied":false}`)
	for i, c := range []struct {
		name string
		// arrange makes the land of the change ch end as wanted.
		arrange func(t *testing.T, ch change.Change)
		status  int
		// errorType, final and reason are the answer's error type, the
		// attempt's status and the land_fail event's reason.
		errorType, final, reason string
		blocked                  map[string]any
	}{
		{name: "landed", status: 200, final: land.StatusLanded},
		{name: "a conflict", status: 409, errorType: "land_conflict", final: land.StatusConflict, reason: "conflict",
			arrange: func(t *testing.T, ch change.Change) {
				commit(t, w.api, "main", head(t, w.api, "main"), "clash", map[string]string{ch.Source.Branch + ".txt": "main\n"})
			}},
		{name: "a gate pending with one unsatisfied requirement", status: 409, errorType: "policy_blocked",
			final: land.StatusBlocked, reason: "policy",
			blocked: map[string]any{"status": "pending", "requirements": []any{map[string]any{"kind": "approvals", "satisfied": false}}},
			arrange: func(_ *testing.T, ch change.Change) {
				gate.answer(ch.Number, land.GateAnswer{Status: change.LandingPending, Requirements: []json.RawMessage{unmet}})
			}},
		{name: "a gate blocking for merge commits", status: 409, errorType: "policy_blocked", final: land.StatusBlocked,
			reason: "merge_commits", blocked: map[string]any{"status": "blocked", "requirements": []any{}},
			arrange: func(_ *testing.T, ch change.Change) {
				gate.answer(ch.Number, land.GateAnswer{Status: change.LandingBlocked, Reason: new("merge_commits")})
			}},
		{name: "contention", status: 503, errorType: "land_failed", final: land.StatusFailed, reason: land.ErrorContention,
			arrange: func(*testing.T, change.Change) { contend.Store(true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer contend.Store(false)
			ch := w.open(t, "s"+strconv.Itoa(i))
			if c.arrange != nil {
				c.arrange(t, ch)
			}
			main := head(t, w.api, "main")

			status, r := w.landCall(t, w.ownerKey, ch.Number, "", nil)

			if c.errorType == "" {
				if status != c.status || r["status"] != c.final {
					t.Fatalf("land = %d %v, want %d %s", status, r, c.status, c.final)
				}
				return
			}
			errObj, _ := r["error"].(map[string]any)
			d, _ := errObj["details"].(map[string]any)
			id, _ := d["id"].(string)
			if status != c.status || errorType(r) != c.errorType || errObj["code"] != float64(c.status) || !isUUID(id) ||
				d["status"] != c.final {
				t.Fatalf("land = %d %v, want %d %s with the %s attempt in details", status, r, c.status, c.errorType, c.final)
			}
			if _, read := w.get(t, "/workspaces/api/lands/"+id, w.ownerKey); !reflect.DeepEqual(read, d) {
				t.Errorf("GET the attempt = %v, want the details %v", read, d)
			}
			if fails := w.landEvents(t, land.EventLandFail, id); len(fails) != 1 || metadata(t, fails[0])["status"] != c.final ||
				metadata(t, fails[0])["reason"] != c.reason {
				t.Errorf("land_fail events %v, want one %s with reason %s", fails, c.final, c.reason)
			}
			if c.blocked == nil {
				return
			}
			c.blocked["change"] = float64(ch.Number)
			if !reflect.DeepEqual(d["blocked"], any(c.blocked)) || d["finished_at"] == nil || d["landed_sha"] != nil {
				t.Errorf("the attempt is blocked %v, finished %v, landed %v; want %v, finished, nothing landed",
					d["blocked"], d["finished_at"], d["landed_sha"], c.blocked)
			}
			if got, state := head(t, w.api, "main"), w.readChange(t, ch.Number).State; got != main || state != change.StateOpen {
				t.Errorf("main moved to %s from %s, the change is %s; want it unmoved and open", got, main, state)
			}
			if lands := w.landEvents(t, land.EventLand, id); len(lands) != 0 {
				t.Errorf("%d parley.change.land events for the blocked attempt, want none", len(lands))
			}
		})
	}
}
