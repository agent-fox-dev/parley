package forge_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// glProject is the path of the fake GitLab's project group/sub/proj, as it
// is sent: the project's path is one escaped segment.
const glProject = forgetest.GitLabPrefix + "/projects/group%2Fsub%2Fproj"

// The forge settings that point a workspace at the fake's GitLab.
func (w *world) gitlab(pairs ...string) map[string]json.RawMessage {
	return settings(append([]string{forge.SettingAPIURL, w.fake.GitLabURL()}, pairs...)...)
}

// accessLevel is a GitLab access member of a project's permissions.
func accessLevel(level int) map[string]int { return map[string]int{"access_level": level} }

func TestRepository_AnswersGitLabsProjectInTheHubsShape(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "w", GitURL: new("https://gitlab.example.com/group/sub/proj.git"), Settings: w.gitlab()})
	cases := []struct {
		name  string
		forge map[string]any
		want  string
	}{
		{"an internal project, developer access, fast-forward merges and no squash", map[string]any{
			"id": 42, "path_with_namespace": "group/sub/proj", "default_branch": "main", "visibility": "internal", "archived": true,
			"permissions":  map[string]any{"project_access": accessLevel(30), "group_access": nil},
			"merge_method": "ff", "squash_option": "never",
		}, `{"full_name":"group/sub/proj","default_branch":"main","private":true,"archived":true,
			"permissions":{"pull":true,"push":true,"admin":false},
			"allow_merge_commit":false,"allow_squash_merge":false,"allow_rebase_merge":true}`},
		{"a public project whose group access is the higher, no merge method and no squash option", map[string]any{
			"path_with_namespace": "group/sub/proj", "default_branch": "main", "visibility": "public", "archived": false,
			"permissions": map[string]any{"project_access": accessLevel(20), "group_access": accessLevel(40)},
		}, `{"full_name":"group/sub/proj","default_branch":"main","private":false,"archived":false,
			"permissions":{"pull":true,"push":true,"admin":true},
			"allow_merge_commit":null,"allow_squash_merge":null,"allow_rebase_merge":null}`},
		{"a private project, reporter access, merge commits and squash allowed", map[string]any{
			"path_with_namespace": "group/sub/proj", "default_branch": "dev", "visibility": "private", "archived": false,
			"permissions":  map[string]any{"project_access": accessLevel(20)},
			"merge_method": "merge", "squash_option": "default_on",
		}, `{"full_name":"group/sub/proj","default_branch":"dev","private":true,"archived":false,
			"permissions":{"pull":true,"push":false,"admin":false},
			"allow_merge_commit":true,"allow_squash_merge":true,"allow_rebase_merge":false}`},
		{"semi-linear merges", map[string]any{
			"path_with_namespace": "group/sub/proj", "default_branch": "main", "visibility": "public",
			"merge_method": "rebase_merge", "squash_option": "always",
		}, `{"full_name":"group/sub/proj","default_branch":"main","private":false,"archived":false,
			"permissions":{"pull":true,"push":false,"admin":false},
			"allow_merge_commit":false,"allow_squash_merge":true,"allow_rebase_merge":true}`},
	}
	for _, c := range cases {
		w.fake.SetObject(glProject, c.forge)
		status, body, sent := w.get(t, "w", "/forge/repository", w.Token)
		if status != http.StatusOK || !sameJSON(body, c.want) {
			t.Errorf("%s: %d %s, want 200 %s", c.name, status, body, c.want)
		}
		if last := w.fake.Last(t); sent != 1 || last.RawPath != glProject {
			t.Errorf("%s: the forge received %d requests, the last for %s, want 1 for %s", c.name, sent, last.RawPath, glProject)
		}
	}
}
