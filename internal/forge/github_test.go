package forge_test

import (
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
