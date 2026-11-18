package forge_test

import (
	"encoding/json"
	"net/http"
	"testing"

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
