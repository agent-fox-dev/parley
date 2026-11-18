package forge_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"testing"

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

	t.Run("asks the forge anew on every request", func(t *testing.T) {
		before := w.fake.Count()
		for _, title := range []string{"Crash", "Crash on start"} {
			w.fake.SetList(ghIssues, githubIssue(7, title, nil))
			if l, _ := w.list(t, "gh", "/issues"); len(l.Items) != 1 || l.Items[0]["title"] != title {
				t.Errorf("GET gh/issues after the forge renamed issue 7 to %q = %v", title, l.Items)
			}
		}
		if n := w.fake.Count() - before; n != 2 {
			t.Errorf("the forge received %d requests for two reads, want 2", n)
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
