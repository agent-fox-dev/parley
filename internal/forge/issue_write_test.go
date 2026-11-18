package forge_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// glSubWidgets is the path of the fake GitLab's project group/sub/widgets, as
// it is sent.
const glSubWidgets = forgetest.GitLabPrefix + "/projects/group%2Fsub%2Fwidgets"

func TestCreateIssue_CreatesTheIssueWithTheFieldsThatAreSet(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "gl", GitURL: new("https://gitlab.com/group/sub/widgets.git"), Settings: w.gitlab()})
	w.putSecret(t, "demo", workspace.SecretGitPAT, "demo-token")
	w.putSecret(t, "gl", workspace.SecretGitPAT, "gl-token")
	answer := func(url, body, labels string) string {
		return `{"number":7,"title":"t","body":"` + body + `","state":"open","url":"` + url + `","author":{"login":"` + forgetest.Login + `"},
			"labels":` + labels + `,"created_at":"` + forgetest.Stamp + `","updated_at":"` + forgetest.Stamp + `","closed_at":null,"is_pr":false}`
	}
	ghURL, glURL := "https://github.com/acme/widgets/issues/7", w.fake.URL+"/group/sub/widgets/-/issues/7"
	full := map[string]any{"title": "t", "body": "b", "labels": []string{"bug", "p1"}, "assignees": []string{"ann"}}

	cases := []struct {
		name, slug, repo, token string
		body                    any
		// sent is the body the forge must receive, and want the answer.
		sent, want string
	}{
		{"GitHub, a title", "demo", ghRepo, "demo-token", map[string]any{"title": "t"},
			`{"title":"t"}`, answer(ghURL, "", "[]")},
		{"GitHub, every field", "demo", ghRepo, "demo-token", full,
			`{"title":"t","body":"b","labels":["bug","p1"],"assignees":["ann"]}`, answer(ghURL, "b", `["bug","p1"]`)},
		{"GitLab, a title", "gl", glSubWidgets, "gl-token", map[string]any{"title": "t"},
			`{"title":"t"}`, answer(glURL, "", "[]")},
		{"GitLab, every field", "gl", glSubWidgets, "gl-token", full,
			`{"title":"t","description":"b","labels":"bug,p1"}`, answer(glURL, "b", `["bug","p1"]`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w.fake.SetNext(c.repo, 7)
			status, body, sent := w.send(t, http.MethodPost, c.slug, "/issues", w.ownerKey, c.body)
			if status != http.StatusCreated || !sameJSON(body, c.want) {
				t.Errorf("POST %s/issues = %d %s, want 201 %s", c.slug, status, body, c.want)
			}
			last := w.fake.Last(t)
			if sent != 1 || last.Method != http.MethodPost || last.RawPath != c.repo+"/issues" {
				t.Fatalf("the forge received %d requests, the last %s %s, want 1, POST %s/issues", sent, last.Method, last.RawPath, c.repo)
			}
			if !sameJSON(last.Body, c.sent) || last.Header.Get("Authorization") != "Bearer "+c.token {
				t.Errorf("the forge got %s with Authorization %q, want %s with the token %s",
					last.Body, last.Header.Get("Authorization"), c.sent, c.token)
			}
		})
	}
}

// issueJSON is the issue i of the fake forge, at the web URL url, as the
// hub answers it.
func issueJSON(t *testing.T, url string, i forgetest.Issue) string {
	t.Helper()
	var closedAt any
	if i.State == "closed" {
		closedAt = forgetest.Stamp
	}
	out, err := json.Marshal(map[string]any{"number": i.Number, "title": i.Title, "body": i.Body, "state": i.State, "url": url,
		"author": map[string]string{"login": forgetest.Login}, "labels": append([]string{}, i.Labels...),
		"created_at": forgetest.Stamp, "updated_at": forgetest.Stamp, "closed_at": closedAt, "is_pr": i.Pull})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestUpdateIssue_SendsOnlyTheSetFields(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "gl", GitURL: new("https://gitlab.com/group/sub/widgets.git"), Settings: w.gitlab()})
	w.putSecret(t, "demo", workspace.SecretGitPAT, "demo-token")
	w.putSecret(t, "gl", workspace.SecretGitPAT, "gl-token")
	every := map[string]any{"title": "new", "body": "b", "state": "closed", "labels": []string{"a"}, "assignees": []string{"ann"}}

	cases := []struct {
		name string
		body any
		// github and gitlab are the bodies each forge must receive, "" when
		// the update is not sent to it, and ghFields and glFields the
		// fields its event names.
		github, gitlab     string
		ghFields, glFields []string
	}{
		{"a title", map[string]any{"title": "new"}, `{"title":"new"}`, `{"title":"new"}`, []string{"title"}, []string{"title"}},
		{"an empty body", map[string]any{"body": ""}, `{"body":""}`, `{"description":""}`, []string{"body"}, []string{"body"}},
		{"state closed", map[string]any{"state": "closed"}, `{"state":"closed"}`, `{"state_event":"close"}`, []string{"state"}, []string{"state"}},
		{"state open", map[string]any{"state": "open"}, `{"state":"open"}`, `{"state_event":"reopen"}`, []string{"state"}, []string{"state"}},
		{"no labels", map[string]any{"labels": []string{}}, `{"labels":[]}`, `{"labels":""}`, []string{"labels"}, []string{"labels"}},
		{"two labels", map[string]any{"labels": []string{"a", "b"}}, `{"labels":["a","b"]}`, `{"labels":"a,b"}`,
			[]string{"labels"}, []string{"labels"}},
		{"no assignees", map[string]any{"assignees": []string{}}, `{"assignees":[]}`, "", []string{"assignees"}, nil},
		{"a title and assignees", map[string]any{"title": "new", "assignees": []string{"ann"}}, `{"title":"new","assignees":["ann"]}`,
			`{"title":"new"}`, []string{"assignees", "title"}, []string{"title"}},
		{"every field", every, `{"title":"new","body":"b","state":"closed","labels":["a"],"assignees":["ann"]}`,
			`{"title":"new","description":"b","state_event":"close","labels":"a"}`,
			[]string{"assignees", "body", "labels", "state", "title"}, []string{"body", "labels", "state", "title"}},
	}
	for _, c := range cases {
		forges := []struct {
			name, slug, repo, method, url, sent string
			fields                              []string
		}{
			{"GitHub", "demo", ghRepo, http.MethodPatch, "https://github.com/acme/widgets/issues/7", c.github, c.ghFields},
			{"GitLab", "gl", glSubWidgets, http.MethodPut, w.fake.URL + "/group/sub/widgets/-/issues/7", c.gitlab, c.glFields},
		}
		for _, f := range forges {
			if f.sent == "" {
				continue
			}
			t.Run(f.name+", "+c.name, func(t *testing.T) {
				w.fake.AddIssue(f.repo, forgetest.Issue{Number: 7, Title: "Crash", Body: "old", State: "open",
					Labels: []string{"bug"}, Assignees: []string{"bob"}})
				n := w.rec.count()
				status, body, sent := w.send(t, http.MethodPatch, f.slug, "/issues/7", w.ownerKey, c.body)
				if status != http.StatusOK || sent != 1 {
					t.Fatalf("PATCH %s/issues/7 = %d %.300s after %d forge requests, want 200 after 1", f.slug, status, body, sent)
				}
				last := w.fake.Last(t)
				if last.Method != f.method || last.RawPath != f.repo+"/issues/7" || !sameJSON(last.Body, f.sent) {
					t.Errorf("the forge got %s %s %s, want %s %s/issues/7 %s", last.Method, last.RawPath, last.Body, f.method, f.repo, f.sent)
				}
				if issue, _ := w.fake.Issue(f.repo, 7); !sameJSON(body, issueJSON(t, f.url, issue)) {
					t.Errorf("the answer is %s, want the forge's issue %s", body, issueJSON(t, f.url, issue))
				}
				events := w.rec.since(n)
				if len(events) != 1 {
					t.Fatalf("the update recorded %d events, want one", len(events))
				}
				got, _ := json.Marshal(events[0].meta(t)["fields"])
				if want, _ := json.Marshal(f.fields); string(got) != string(want) {
					t.Errorf("the event names the fields %s, want %s", got, want)
				}
			})
		}
	}
}

func TestUpdateIssue_RefusesAnUpdateWithNothingToSend(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "gl", GitURL: new("https://gitlab.com/group/sub/widgets.git"), Settings: w.gitlab()})
	w.putSecret(t, "demo", workspace.SecretGitPAT, "demo-token")
	w.putSecret(t, "gl", workspace.SecretGitPAT, "gl-token")
	w.seedIssue(ghRepo)
	w.seedIssue(glSubWidgets)

	assignees := []map[string]any{{"assignees": []string{"ann"}}, {"assignees": []string{}}, {"assignees": []string{"ann"}, "title": nil}}
	cases := []struct {
		name, slug string
		body       any
		status     int
	}{
		{"no member", "demo", map[string]any{}, http.StatusBadRequest},
		{"a null title", "demo", map[string]any{"title": nil}, http.StatusBadRequest},
		{"every field null", "demo", map[string]any{"title": nil, "body": nil, "state": nil, "labels": nil, "assignees": nil}, http.StatusBadRequest},
		{"an unknown member only", "demo", map[string]any{"unknown": 1}, http.StatusBadRequest},
		{"GitLab, assignees only", "gl", assignees[0], http.StatusBadRequest},
		{"GitLab, no assignees only", "gl", assignees[1], http.StatusBadRequest},
		{"GitLab, assignees and a null title", "gl", assignees[2], http.StatusBadRequest},
		{"GitHub, assignees only", "demo", assignees[0], http.StatusOK},
		{"GitHub, no assignees only", "demo", assignees[1], http.StatusOK},
		{"GitHub, assignees and a null title", "demo", assignees[2], http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := w.rec.count()
			status, body, sent := w.send(t, http.MethodPatch, c.slug, "/issues/7", w.ownerKey, c.body)
			if c.status == http.StatusOK {
				if status != http.StatusOK || sent != 1 {
					t.Errorf("PATCH %s/issues/7 = %d %.300s after %d forge requests, want 200 after 1", c.slug, status, body, sent)
				}
				return
			}
			if e := errorOf(t, body); status != http.StatusBadRequest || e.Type != "invalid_request" || sent != 0 || w.rec.count() != n {
				t.Errorf("PATCH %s/issues/7 = %d %s after %d forge requests and %d events, want 400 invalid_request after none",
					c.slug, status, body, sent, w.rec.count()-n)
			}
		})
	}
}

// forgesWorld is a world with the GitHub workspace demo and the GitLab
// workspace gl, each with a stored token, issue 7 and pull request 12.
func forgesWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "gl", GitURL: new("https://gitlab.com/group/sub/widgets.git"), Settings: w.gitlab()})
	w.putSecret(t, "demo", workspace.SecretGitPAT, "demo-token")
	w.putSecret(t, "gl", workspace.SecretGitPAT, "gl-token")
	w.seedIssue(ghRepo)
	w.seedIssue(glSubWidgets)
	return w
}

// lastComment is the latest comment on the issue or pull request n of the
// fake forge's repository repo, ghRepo or glSubWidgets, and its web URL as
// the forge gives it on GitHub, or as GitLab's web site shows it.
func (w *world) lastComment(t *testing.T, repo string, n int64) (forgetest.Comment, string) {
	t.Helper()
	i, _ := w.fake.Issue(repo, n)
	if len(i.Comments) == 0 {
		t.Fatalf("the forge holds no comment on %d", n)
	}
	c := i.Comments[len(i.Comments)-1]
	if repo == glSubWidgets {
		page := "issues"
		if i.Pull {
			page = "merge_requests"
		}
		return c, fmt.Sprintf("%s/group/sub/widgets/-/%s/%d#note_%d", w.fake.URL, page, n, c.ID)
	}
	page := "issues"
	if i.Pull {
		page = "pull"
	}
	return c, fmt.Sprintf("https://github.com/acme/widgets/%s/%d#issuecomment-%d", page, n, c.ID)
}

// wantComment sends text as the body of a comment to path of the workspace
// slug and checks that the forge received it, byte for byte, in one
// request, method POST to sent, as a comment on n of repo, and that the
// answer is 201 with exactly the comment's web URL.
func (w *world) wantComment(t *testing.T, slug, path, repo, sent string, n int64, text string) {
	t.Helper()
	status, body, requests := w.send(t, http.MethodPost, slug, path, w.ownerKey, map[string]any{"body": text})
	if status != http.StatusCreated || requests != 1 {
		t.Fatalf("POST %s%s = %d %.300s after %d forge requests, want 201 after 1", slug, path, status, body, requests)
	}
	c, url := w.lastComment(t, repo, n)
	if !sameJSON(body, `{"url":"`+url+`"}`) {
		t.Errorf("the answer is %s, want exactly the url %s", body, url)
	}
	last := w.fake.Last(t)
	var got struct{ Body *string }
	if last.Method != http.MethodPost || last.RawPath != sent || json.Unmarshal(last.Body, &got) != nil || got.Body == nil ||
		*got.Body != text || c.Body != text {
		t.Errorf("the forge got %s %s %q, want POST %s with the body %q", last.Method, last.RawPath, last.Body, sent, text)
	}
}

func TestCommentOnIssue_PostsTheTextAsGiven(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)
	for _, f := range []struct{ name, slug, repo, sent string }{
		{"GitHub", "demo", ghRepo, ghRepo + "/issues/7/comments"},
		{"GitLab", "gl", glSubWidgets, glSubWidgets + "/issues/7/notes"},
	} {
		t.Run(f.name, func(t *testing.T) {
			w.wantComment(t, f.slug, "/issues/7/comments", f.repo, f.sent, 7, "  line one\n\nline two \n")
		})
	}
}

// closers are the forges as a close sees them: the workspace, its
// repository, the issue 7's web URL, the path the comment is posted to, and
// the close's method and body.
func closers(w *world) []struct{ name, slug, repo, url, comment, method, sent string } {
	return []struct{ name, slug, repo, url, comment, method, sent string }{
		{"GitHub", "demo", ghRepo, "https://github.com/acme/widgets/issues/7", ghRepo + "/issues/7/comments",
			http.MethodPatch, `{"state":"closed"}`},
		{"GitLab", "gl", glSubWidgets, w.fake.URL + "/group/sub/widgets/-/issues/7", glSubWidgets + "/issues/7/notes",
			http.MethodPut, `{"state_event":"close"}`},
	}
}

func TestCloseIssue_ClosesWithoutACommentWhenNoneIsGiven(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)
	bodies := []struct {
		name string
		body any
	}{
		{"no body", nil}, {"an empty body", ""}, {"{}", map[string]any{}},
		{"a null comment", map[string]any{"comment": nil}}, {"a blank comment", map[string]any{"comment": " \t\n"}},
	}
	for _, f := range closers(w) {
		for _, b := range bodies {
			for _, state := range []string{"open", "closed"} {
				t.Run(f.name+", "+b.name+", an issue "+state, func(t *testing.T) {
					w.fake.AddIssue(f.repo, forgetest.Issue{Number: 7, Title: "Crash", State: state})
					n := w.rec.count()
					status, body, sent := w.send(t, http.MethodPost, f.slug, "/issues/7/close", w.ownerKey, b.body)
					issue, _ := w.fake.Issue(f.repo, 7)
					if status != http.StatusOK || issue.State != "closed" || !sameJSON(body, issueJSON(t, f.url, issue)) {
						t.Errorf("POST %s/issues/7/close = %d %.300s, want 200 with the closed issue", f.slug, status, body)
					}
					last := w.fake.Last(t)
					if sent != 1 || last.Method != f.method || last.RawPath != f.repo+"/issues/7" || !sameJSON(last.Body, f.sent) {
						t.Errorf("the forge got %d requests, the last %s %s %s, want the close alone, %s %s/issues/7 %s",
							sent, last.Method, last.RawPath, last.Body, f.method, f.repo, f.sent)
					}
					if events := w.rec.since(n); len(events) != 1 || events[0].meta(t)["comment_url"] != nil {
						t.Errorf("the close recorded %+v, want one event without comment_url", events)
					}
				})
			}
		}
	}
}

func TestCloseIssue_PostsTheCommentBeforeItCloses(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)
	for _, f := range closers(w) {
		t.Run(f.name, func(t *testing.T) {
			n, before := w.rec.count(), w.fake.Count()
			status, body, _ := w.send(t, http.MethodPost, f.slug, "/issues/7/close", w.ownerKey, map[string]any{"comment": "done in v2"})
			issue, _ := w.fake.Issue(f.repo, 7)
			if status != http.StatusOK || issue.State != "closed" || !sameJSON(body, issueJSON(t, f.url, issue)) {
				t.Errorf("POST %s/issues/7/close = %d %.300s, want 200 with the closed issue", f.slug, status, body)
			}
			got := w.fake.Requests()[before:]
			if len(got) != 2 || got[0].Method != http.MethodPost || got[0].RawPath != f.comment || !sameJSON(got[0].Body, `{"body":"done in v2"}`) ||
				got[1].Method != f.method || got[1].RawPath != f.repo+"/issues/7" || !sameJSON(got[1].Body, f.sent) {
				t.Fatalf("the forge got %+v, want the comment POST %s, then the close %s %s/issues/7", got, f.comment, f.method, f.repo)
			}
			_, url := w.lastComment(t, f.repo, 7)
			events := w.rec.since(n)
			if len(events) != 1 || events[0].Type != "parley.forge.issue.close" || events[0].meta(t)["ok"] != true ||
				events[0].meta(t)["comment_url"] != url {
				t.Errorf("the close recorded %+v, want one parley.forge.issue.close, ok, with comment_url %s", events, url)
			}
		})
	}
}

func TestCloseIssue_AnswersTheFailureOfItsCommentOrOfTheClose(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)
	comment, closing := ghRepo+"/issues/7/comments", ghRepo+"/issues/7"
	cases := []struct {
		name string
		// failed is the forge request that fails, with the status forge.
		failed    string
		forge     int
		status    int
		errorType string
		// sent is how many requests the forge receives: the comment alone,
		// or the comment and the close.
		sent int
	}{
		{"the comment, 404", comment, 404, 404, forge.ErrorTypeNotFound, 1},
		{"the comment, 422", comment, 422, 422, forge.ErrorTypeRejected, 1},
		{"the comment, 500", comment, 500, 502, forge.ErrorTypeError, 1},
		{"the close, 403", closing, 403, 502, forge.ErrorTypeForbidden, 2},
		{"the close, 500", closing, 500, 502, forge.ErrorTypeError, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w.seedIssue(ghRepo)
			w.fake.Script(forgetest.Answer{Path: c.failed, Status: c.forge, Body: `{"message":"no"}`})
			n := w.rec.count()
			status, body, sent := w.send(t, http.MethodPost, "demo", "/issues/7/close", w.ownerKey, map[string]any{"comment": "c"})
			if e := errorOf(t, body); status != c.status || e.Type != c.errorType || sent != c.sent {
				t.Errorf("POST demo/issues/7/close = %d %s after %d forge requests, want %d %s after %d",
					status, body, sent, c.status, c.errorType, c.sent)
			}
			if issue, _ := w.fake.Issue(ghRepo, 7); issue.State != "open" {
				t.Errorf("the issue is %s, want it still open", issue.State)
			}
			events := w.rec.since(n)
			if len(events) != 1 {
				t.Fatalf("the close recorded %d events, want one", len(events))
			}
			var commentURL any
			if c.sent == 2 {
				_, commentURL = w.lastComment(t, ghRepo, 7)
			}
			if m := events[0].meta(t); events[0].Type != "parley.forge.issue.close" || m["ok"] != false || m["error_type"] != c.errorType ||
				m["comment_url"] != commentURL {
				t.Errorf("the close recorded %s with %s, want parley.forge.issue.close, not ok, %s, comment_url %v",
					events[0].Type, events[0].Metadata, c.errorType, commentURL)
			}
		})
	}
}
