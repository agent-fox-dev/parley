package forge_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// ghRepo is the path of the fake GitHub's repository acme/widgets.
const ghRepo = forgetest.GitHubPrefix + "/repos/acme/widgets"

func TestRepository_AnswersGitHubsRepositoryInTheHubsShape(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "w", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	cases := []struct {
		name  string
		forge map[string]any
		want  string
	}{
		{"every member given", map[string]any{
			"id": 1, "full_name": "acme/widgets", "default_branch": "main", "private": true, "archived": false,
			"permissions":        map[string]bool{"pull": true, "push": true, "admin": false, "maintain": true},
			"allow_merge_commit": true, "allow_squash_merge": false, "allow_rebase_merge": true,
		}, `{"full_name":"acme/widgets","default_branch":"main","private":true,"archived":false,
			"permissions":{"pull":true,"push":true,"admin":false},
			"allow_merge_commit":true,"allow_squash_merge":false,"allow_rebase_merge":true}`},
		{"no permissions and no allow members", map[string]any{
			"full_name": "acme/widgets", "default_branch": "trunk", "private": false, "archived": true,
		}, `{"full_name":"acme/widgets","default_branch":"trunk","private":false,"archived":true,
			"permissions":{"pull":true,"push":false,"admin":false},
			"allow_merge_commit":null,"allow_squash_merge":null,"allow_rebase_merge":null}`},
	}
	for _, c := range cases {
		w.fake.SetObject(ghRepo, c.forge)
		status, body, sent := w.get(t, "w", "/forge/repository", w.Token)
		if status != http.StatusOK || !sameJSON(body, c.want) {
			t.Errorf("%s: %d %s, want 200 %s", c.name, status, body, c.want)
		}
		if last := w.fake.Last(t); sent != 1 || last.RawPath != ghRepo {
			t.Errorf("%s: the forge received %d requests, the last for %s, want 1 for %s", c.name, sent, last.RawPath, ghRepo)
		}
	}
}

func TestIssueThread_AnswersGitHubsIssueAndComments(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "w", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.fake.SetObject(ghIssues+"/7", githubIssue(7, "Crash", nil))
	w.fake.SetObject(ghIssues+"/9", githubIssue(9, "A change",
		map[string]any{"pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widgets/pulls/9"}}))
	w.fake.SetList(ghIssues + "/9/comments")
	// The forge's order, which is not the order of the ids; the second
	// comment has no body, no update and a time two hours east of UTC.
	w.fake.SetList(ghIssues+"/7/comments", githubComment(12, "first"),
		with(githubComment(4, ""), "body", nil, "updated_at", nil, "created_at", "2026-10-01T10:00:20.5+02:00"))
	want := `[{"id":12,"body":"first","author":{"login":"kim"},"created_at":"2026-10-01T08:00:12Z","updated_at":"2026-10-01T08:00:12Z",
		"url":"https://github.com/acme/widgets/issues/7#issuecomment-12"},
		{"id":4,"body":"","author":{"login":"kim"},"created_at":"2026-10-01T08:00:20Z","updated_at":null,
		"url":"https://github.com/acme/widgets/issues/7#issuecomment-4"}]`

	for number, isPR := range map[string]bool{"9": true, "7": false} {
		if th, _ := w.thread(t, "w", "/issues/"+number); th.Issue["is_pr"] != isPR {
			t.Errorf("GET w/issues/%s: is_pr %v, want %v", number, th.Issue["is_pr"], isPR)
		}
	}
	th, _ := w.thread(t, "w", "/issues/7")
	l, _ := w.list(t, "w", "/issues/7/comments")
	for name, items := range map[string][]map[string]any{"the thread's comments": th.Comments, "the comments": l.Items} {
		if got, _ := json.Marshal(items); !sameJSON(got, want) {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}
