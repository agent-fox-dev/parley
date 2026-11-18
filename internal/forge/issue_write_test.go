package forge_test

import (
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
