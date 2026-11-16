package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// commitPath is commitFile for a file in a directory the clone may not
// have yet.
func commitPath(t *testing.T, run func(...string) string, file, content, message string) string {
	t.Helper()
	dir := strings.TrimSpace(run("rev-parse", "--show-toplevel"))
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0o700); err != nil {
		t.Fatal(err)
	}
	return commitFile(t, run, file, content, message)
}

// An agent whose token may write only under docs/ pushes a commit the
// repository already has, which touches src/a.go, to the source branch of
// its change with stock git: the push is refused naming the path, and
// neither the branch nor the change moves.
func TestPathLimits_APushOfExistingCommitsOutsideTheTokensPathsIsRefused(t *testing.T) {
	parallel(t)
	h := startForkHub(t)
	docs := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"git:read", "git:write", "changes:read", "changes:write"},
		Paths: []string{"docs/**"}, CreatedAt: time.Now()})
	agent := cloneAs(t, h, docs, "demo")
	agent("checkout", "--quiet", "-b", "feature")
	feature := commitPath(t, agent, "docs/a.md", "a\n", "Document a")
	if out := agent("push", "-o", "parley.change", "-o", "parley.intent=Document a", "origin", "feature"); !strings.Contains(out, "change demo#1 version 1 (opened)") {
		t.Fatalf("the push of feature printed:\n%s\nwant change demo#1 version 1 (opened)", out)
	}
	owner := cloneAs(t, h, h.key, "demo")
	owner("fetch", "--quiet", "origin", "feature")
	owner("checkout", "--quiet", "-b", "other", "origin/feature")
	x := commitPath(t, owner, "src/a.go", "package a\n", "Add a")
	owner("push", "--quiet", "origin", "other")
	agent("fetch", "--quiet", "origin", "other")

	g := newGitClient(t, h.URL(""), docs)
	dir := strings.TrimSpace(agent("rev-parse", "--show-toplevel"))
	out, err := g.git(dir, "push", "origin", x+":refs/heads/feature")

	if err == nil || !strings.Contains(out, "path src/a.go is outside this token's allowed paths") {
		t.Errorf("git push of %s to feature = %v:\n%s\nwant it refused naming src/a.go", x, err, out)
	}
	if got := strings.Fields(agent("ls-remote", "origin", "refs/heads/feature")); len(got) == 0 || got[0] != feature {
		t.Errorf("feature is at %v on the hub, want %s", got, feature)
	}
	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/1", docs, nil)
	var ch struct {
		Version int    `json:"version"`
		HeadSHA string `json:"head_sha"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &ch) != nil || ch.Version != 1 || ch.HeadSHA != feature {
		t.Errorf("GET changes/1 = %d %s, want version 1 at %s", status, body, feature)
	}
}
