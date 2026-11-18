package forge_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The fake forges' issue lists of acme/widgets on GitHub and of
// group/sub/proj on GitLab.
const (
	ghIssues = ghRepo + "/issues"
	glIssues = glProject + "/issues"
)

// githubIssue is GitHub's issue number, with the members of more added.
func githubIssue(number int, title string, more map[string]any) map[string]any {
	issue := map[string]any{
		"number": number, "title": title, "body": "text", "state": "open",
		"html_url": fmt.Sprintf("https://github.com/acme/widgets/issues/%d", number),
		"user":     map[string]any{"login": "kim"}, "labels": []any{},
		"created_at": "2026-10-01T08:00:00Z", "updated_at": "2026-10-01T08:00:00Z", "closed_at": nil,
	}
	maps.Copy(issue, more)
	return issue
}

// gitlabIssue is GitLab's issue iid, with the members of more added.
func gitlabIssue(iid int, title string, more map[string]any) map[string]any {
	issue := map[string]any{
		"id": 1000 + iid, "iid": iid, "title": title, "description": "text", "state": "opened",
		"web_url": fmt.Sprintf("https://gitlab.example.com/group/sub/proj/-/issues/%d", iid),
		"author":  map[string]any{"username": "kim"}, "labels": []any{},
		"created_at": "2026-10-01T08:00:00Z", "updated_at": "2026-10-01T08:00:00Z", "closed_at": nil,
	}
	maps.Copy(issue, more)
	return issue
}

func TestIssueList(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "gh", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "gl", GitURL: new("https://gitlab.example.com/group/sub/proj.git"), Settings: w.gitlab()})
	// Issue 7 was created at 10:00:00.123 two hours east of UTC, has no
	// body, the labels bug and ui and the assignee kim; issue 8 is closed.
	crash := map[string]any{"body": nil, "created_at": "2026-10-01T10:00:00.123+02:00", "updated_at": "2026-10-02T11:30:00+02:00"}
	answer := func(url string) string {
		return `{"number":7,"title":"Crash","body":"","state":"open","url":"` + url + `","author":{"login":"kim"},
			"labels":["bug","ui"],"created_at":"2026-10-01T08:00:00Z","updated_at":"2026-10-02T09:30:00Z","closed_at":null,"is_pr":false}`
	}
	closed := `{"number":8,"title":"Old","body":"text","state":"closed","url":"%s","author":{"login":"kim"},"labels":[],
		"created_at":"2026-10-01T08:00:00Z","updated_at":"2026-10-01T08:00:00Z","closed_at":"2026-10-03T12:00:00Z","is_pr":false}`
	forges := []struct {
		slug, path string
		issues     []any
		want       []string
		// queries are the forge's query for no filter, every filter, and
		// closed issues.
		queries [3]string
	}{
		{"gh", ghIssues, []any{
			githubIssue(7, "Crash", with(crash, "labels", []any{map[string]any{"name": "bug"}, map[string]any{"name": "ui"}},
				"assignee", map[string]any{"login": "kim"})),
			githubIssue(8, "Old", map[string]any{"state": "closed", "closed_at": "2026-10-03T14:00:00+02:00"}),
		}, []string{answer("https://github.com/acme/widgets/issues/7"), fmt.Sprintf(closed, "https://github.com/acme/widgets/issues/8")},
			[3]string{
				"state=open&sort=created&direction=desc&per_page=50&page=1",
				"state=all&sort=updated&direction=asc&labels=bug,ui&assignee=kim&per_page=50&page=1",
				"state=closed&sort=created&direction=desc&per_page=50&page=1",
			}},
		{"gl", glIssues, []any{
			gitlabIssue(7, "Crash", with(crash, "description", nil, "labels", []any{"bug", "ui"},
				"assignees", []any{map[string]any{"username": "kim"}})),
			gitlabIssue(8, "Old", map[string]any{"state": "closed", "closed_at": "2026-10-03T14:00:00+02:00"}),
		}, []string{answer("https://gitlab.example.com/group/sub/proj/-/issues/7"),
			fmt.Sprintf(closed, "https://gitlab.example.com/group/sub/proj/-/issues/8")},
			[3]string{
				"state=opened&order_by=created_at&sort=desc&per_page=50&page=1",
				"order_by=updated_at&sort=asc&labels=bug,ui&assignee_username=kim&per_page=50&page=1",
				"state=closed&order_by=created_at&sort=desc&per_page=50&page=1",
			}},
	}

	t.Run("asks each forge for the filters in its own terms and answers its issues in one shape", func(t *testing.T) {
		for _, f := range forges {
			w.fake.SetList(f.path, f.issues...)
			for i, query := range []string{"", "?state=all&sort=updated&direction=asc&labels=bug,ui&assignee=kim", "?state=closed"} {
				l, sent := w.list(t, f.slug, "/issues"+query)
				want, _ := url.ParseQuery(f.queries[i])
				if last := w.fake.Last(t); sent != 1 || last.RawPath != f.path || !maps.EqualFunc(w.lastQuery(t), want, slices.Equal) {
					t.Errorf("%s/issues%s: the forge received %d requests, the last %s?%s, want 1 for %s?%s",
						f.slug, query, sent, last.RawPath, last.RawQuery, f.path, f.queries[i])
				}
				if i > 0 {
					continue
				}
				for j, item := range l.Items {
					if got, _ := json.Marshal(item); j >= len(f.want) || !sameJSON(got, f.want[j]) {
						t.Errorf("%s/issues: item %d = %s, want %s", f.slug, j, got, f.want[min(j, len(f.want)-1)])
					}
				}
				if len(l.Items) != len(f.want) || l.NextCursor != nil {
					t.Errorf("%s/issues: %d items and next_cursor %v, want %d and null", f.slug, len(l.Items), l.NextCursor, len(f.want))
				}
			}
		}
		w.fake.SetList(glIssues, gitlabIssue(9, "Locked", map[string]any{"state": "locked"}))
		if l, _ := w.list(t, "gl", "/issues"); len(l.Items) != 1 || l.Items[0]["state"] != "open" {
			t.Errorf("a locked GitLab issue answers %v, want the state open", l.Items)
		}
	})

	t.Run("leaves GitHub's pull requests out of the page without refilling it", func(t *testing.T) {
		w.fake.SetList(ghIssues, githubIssue(7, "Crash", nil),
			githubIssue(9, "A change", map[string]any{"pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widgets/pulls/9"}}),
			githubIssue(10, "Typo", nil))
		l, sent := w.list(t, "gh", "/issues?limit=3")
		var numbers []any
		for _, item := range l.Items {
			numbers = append(numbers, item["number"])
			if item["is_pr"] != false {
				t.Errorf("issue %v has is_pr %v, want false", item["number"], item["is_pr"])
			}
		}
		if fmt.Sprint(numbers) != "[7 10]" || sent != 1 {
			t.Errorf("GET gh/issues?limit=3 = the numbers %v after %d forge requests, want [7 10] after 1", numbers, sent)
		}
	})

	t.Run("refuses a filter given twice, outside its set or empty without asking the forge", func(t *testing.T) {
		for _, query := range []string{
			"state=merged", "state=open&state=closed", "sort=comments", "sort=created&sort=created", "direction=up",
			"direction=asc&direction=desc", "labels=a&labels=b", "labels=a,,b", "labels=", "labels=a,", "assignee=a&assignee=b", "assignee=",
		} {
			for _, slug := range []string{"gh", "gl"} {
				status, body, sent := w.get(t, slug, "/issues?"+query, w.Token)
				if status != http.StatusBadRequest || errorOf(t, body).Type != "invalid_request" || sent != 0 {
					t.Errorf("GET %s/issues?%s = %d %s after %d forge requests, want 400 invalid_request after none", slug, query, status, body, sent)
				}
			}
		}
	})
}

// with is a copy of m with the name and value pairs added.
func with(m map[string]any, pairs ...any) map[string]any {
	out := maps.Clone(m)
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].(string)] = pairs[i+1]
	}
	return out
}

// second is the time n seconds after 2026-10-01 08:00 UTC, as the forges
// write it.
func second(n int) string {
	return time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Second).Format(time.RFC3339)
}

// githubComment is GitHub's comment id on issue 7, written id seconds
// after 08:00.
func githubComment(id int, body string) map[string]any {
	return map[string]any{
		"id": id, "body": body, "user": map[string]any{"login": "kim"}, "created_at": second(id), "updated_at": second(id),
		"html_url": fmt.Sprintf("https://github.com/acme/widgets/issues/7#issuecomment-%d", id),
	}
}

// gitlabNote is GitLab's note id on issue 7, written id seconds after
// 08:00; system marks a note GitLab wrote itself, such as a label change.
func gitlabNote(id int, body string, system bool) map[string]any {
	return map[string]any{
		"id": id, "body": body, "system": system, "author": map[string]any{"username": "kim"}, "noteable_type": "Issue",
		"noteable_iid": 7, "created_at": second(id), "updated_at": second(id),
	}
}

// issueThread is an issue thread as the hub answers it.
type issueThread struct {
	Issue              map[string]any   `json:"issue"`
	Comments           []map[string]any `json:"comments"`
	CommentsNextCursor *string          `json:"comments_next_cursor"`
	CommentsError      map[string]any   `json:"comments_error"`
}

// thread sends GET /api/v1/workspaces/<slug><path> with the admin token and
// returns the issue thread it answers, failing the test unless the answer
// is 200 with exactly the members issue, comments, an array,
// comments_next_cursor and comments_error. sent is how many requests the
// forge received meanwhile.
func (w *world) thread(t *testing.T, slug, path string) (th issueThread, sent int) {
	t.Helper()
	status, body, sent := w.get(t, slug, path, w.Token)
	var members map[string]json.RawMessage
	if status != http.StatusOK || json.Unmarshal(body, &members) != nil || json.Unmarshal(body, &th) != nil ||
		!slices.Equal(slices.Sorted(maps.Keys(members)), []string{"comments", "comments_error", "comments_next_cursor", "issue"}) ||
		!strings.HasPrefix(string(members["comments"]), "[") {
		t.Fatalf("GET %s%s = %d %.300s, want 200 with issue, comments, an array, comments_next_cursor and comments_error",
			slug, path, status, body)
	}
	return th, sent
}

// ids are the members id of items.
func ids(items []map[string]any) []any {
	out := []any{}
	for _, item := range items {
		out = append(out, item["id"])
	}
	return out
}

func TestIssueThread(t *testing.T) {
	t.Parallel()
	// A comments read the forge holds times out after the deadline; every
	// other answer comes at once.
	w := newWorldWith(t, hubtest.Config{}, func(_ *world, d *forge.Deps) { d.Client.Deadline = 2 * time.Second })
	w.addWorkspace(t, workspace.Workspace{Slug: "gh", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "gl", GitURL: new("https://gitlab.example.com/group/sub/proj.git"), Settings: w.gitlab()})
	type forgeIssue struct {
		slug, issue, comments string
		seed                  func(n int)
	}
	forges := []forgeIssue{
		{"gh", ghIssues + "/7", ghIssues + "/7/comments", func(n int) {
			w.fake.SetObject(ghIssues+"/7", githubIssue(7, "Crash", nil))
			comments := make([]any, n)
			for i := range comments {
				comments[i] = githubComment(i+1, "comment")
			}
			w.fake.SetList(ghIssues+"/7/comments", comments...)
		}},
		{"gl", glIssues + "/7", glIssues + "/7/notes", func(n int) {
			w.fake.SetObject(glIssues+"/7", gitlabIssue(7, "Crash", nil))
			notes := make([]any, n)
			for i := range notes {
				notes[i] = gitlabNote(i+1, "note", false)
			}
			w.fake.SetList(glIssues+"/7/notes", notes...)
		}},
	}

	t.Run("answers the issue with its first page of comments oldest first", func(t *testing.T) {
		for _, f := range forges {
			f.seed(3)
			th, sent := w.thread(t, f.slug, "/issues/7")
			if th.Issue["number"] != 7.0 || len(th.Comments) != 3 || th.CommentsNextCursor != nil || th.CommentsError != nil {
				t.Errorf("%s/issues/7: issue %v with %d comments, comments_next_cursor %v and comments_error %v, "+
					"want issue 7 with 3 comments and both null", f.slug, th.Issue["number"], len(th.Comments), th.CommentsNextCursor, th.CommentsError)
			}
			q := w.lastQuery(t)
			if last := w.fake.Last(t); sent != 2 || last.RawPath != f.comments || q.Get("per_page") != "100" || q.Get("page") != "1" {
				t.Errorf("%s/issues/7: the forge received %d requests, the last %s?%s, want 2, the last %s with per_page=100&page=1",
					f.slug, sent, last.RawPath, last.RawQuery, f.comments)
			}
			var created []string
			for _, c := range th.Comments {
				created = append(created, fmt.Sprint(c["created_at"]))
			}
			if !slices.IsSorted(created) {
				t.Errorf("%s/issues/7: the comments were created at %v, want oldest first", f.slug, created)
			}
			if keys := slices.Sorted(maps.Keys(th.Comments[0])); !slices.Equal(keys, []string{"author", "body", "created_at", "id", "updated_at", "url"}) {
				t.Errorf("%s/issues/7: a comment has the members %v", f.slug, keys)
			}
		}
	})

	t.Run("continues the comments past the first hundred on the comments route", func(t *testing.T) {
		for _, f := range forges {
			f.seed(130)
			th, _ := w.thread(t, f.slug, "/issues/7")
			if len(th.Comments) != 100 || th.CommentsNextCursor == nil {
				t.Fatalf("%s/issues/7 of 130 comments: %d comments and comments_next_cursor %v, want 100 and a cursor",
					f.slug, len(th.Comments), th.CommentsNextCursor)
			}
			l, _ := w.list(t, f.slug, "/issues/7/comments?cursor="+url.QueryEscape(*th.CommentsNextCursor))
			if q := w.lastQuery(t); len(l.Items) != 30 || l.Items[0]["id"] != 101.0 || l.NextCursor != nil || q.Get("per_page") != "100" || q.Get("page") != "2" {
				t.Errorf("%s/issues/7/comments from the thread's cursor: %d items from %v with next_cursor %v after per_page=%s&page=%s, "+
					"want 30 from 101 and null after per_page=100&page=2", f.slug, len(l.Items), ids(l.Items[:1]), l.NextCursor, q.Get("per_page"), q.Get("page"))
			}
		}
		th, _ := w.thread(t, "gh", "/issues/7?target=origin")
		c := url.QueryEscape(*th.CommentsNextCursor)
		if l, _ := w.list(t, "gh", "/issues/7/comments?target=origin&cursor="+c); len(l.Items) != 30 {
			t.Errorf("the cursor of gh/issues/7?target=origin answers %d comments on the same target, want 30", len(l.Items))
		}
		for _, path := range []string{"/issues/7/comments?cursor=" + c, "/issues/8/comments?target=origin&cursor=" + c} {
			if status, body, sent := w.get(t, "gh", path, w.Token); status != http.StatusBadRequest || errorOf(t, body).Type != "invalid_request" || sent != 0 {
				t.Errorf("the cursor of gh/issues/7?target=origin on %s = %d %s after %d forge requests, want 400 invalid_request after none",
					path, status, body, sent)
			}
		}
	})

	t.Run("answers the issue when its comments fail", func(t *testing.T) {
		forges[0].seed(3)
		// GitHub's path holds no escape, so the decoded path is the one sent.
		path := forges[0].comments
		for _, c := range []struct {
			name      string
			answer    forgetest.Answer
			errorType string
		}{
			{"500", forgetest.Answer{Status: 500, Body: `{"message":"boom"}`}, forge.ErrorTypeError},
			{"403 without rate-limit headers", forgetest.Answer{Status: 403, Body: `{"message":"no"}`}, forge.ErrorTypeForbidden},
			{"429 without a wait", forgetest.Answer{Status: 429}, forge.ErrorTypeRateLimited},
			{"no answer before the deadline", forgetest.Answer{Hold: true}, forge.ErrorTypeTimeout},
		} {
			c.answer.Path = path
			w.fake.Script(c.answer)
			th, _ := w.thread(t, "gh", "/issues/7")
			e := th.CommentsError
			if th.Issue["number"] != 7.0 || len(th.Comments) != 0 || th.CommentsNextCursor != nil || len(e) != 2 ||
				e["error_type"] != c.errorType || e["message"] == "" || e["message"] == nil {
				t.Errorf("%s: issue %v, comments %v, comments_next_cursor %v and comments_error %v, "+
					"want issue 7, [], null and {error_type %s, message}", c.name, th.Issue["number"], th.Comments, th.CommentsNextCursor, e, c.errorType)
			}
		}
	})

	t.Run("asks the forge anew on every request", func(t *testing.T) {
		before := len(w.fake.Requests())
		for _, title := range []string{"Crash", "Crash on start"} {
			w.fake.SetObject(ghIssues+"/7", githubIssue(7, title, nil))
			if th, _ := w.thread(t, "gh", "/issues/7"); th.Issue["title"] != title {
				t.Errorf("GET gh/issues/7 after the forge renamed issue 7 to %q = %v", title, th.Issue)
			}
		}
		served := 0
		for _, r := range w.fake.Requests()[before:] {
			if r.RawPath == ghIssues+"/7" {
				served++
			}
		}
		if served != 2 {
			t.Errorf("the forge served issue 7 %d times for two reads, want 2", served)
		}
	})
}
