package land_test

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/land"
)

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
