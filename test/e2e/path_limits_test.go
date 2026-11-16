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

// An agent whose token may change only docs/ asks to land a change of its
// owner that touches src/main.go and is refused, naming the file, with no
// land attempt recorded; it lands a change that touches only docs/a.md
// with the same token.
func TestPathLimits_ATokenLandsInsideItsPathsAndIsRefusedOutsideThem(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	docs := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"changes:read", "changes:write"},
		Paths: []string{"docs/**"}, CreatedAt: time.Now()})
	h.commitOnBranch(t, "src", "src/main.go", "package main\n", "Add main")
	outside := h.open(t, h.key, "demo", "src", map[string]any{"intent": "why"})
	h.commitOnBranch(t, "docs", "docs/a.md", "a\n", "Document a")
	inside := h.open(t, h.key, "demo", "docs", map[string]any{"intent": "why"})
	h.until(t, outside, "computed", computed)
	h.until(t, inside, "computed", computed)

	status, refused := h.land(t, docs, outside, "")
	if status != http.StatusForbidden || str(refused, "error", "error_type") != "path_not_allowed" || str(refused, "error", "path") != "src/main.go" {
		t.Errorf("the land of demo#%s with the docs token = %d %v, want 403 path_not_allowed naming src/main.go", outside, status, refused)
	}
	if attempts, _ := h.get(t, "/workspaces/demo/changes/"+outside+"/lands")["items"].([]any); len(attempts) != 0 {
		t.Errorf("demo#%s has the land attempts %v, want none", outside, attempts)
	}
	if status, a := h.land(t, docs, inside, ""); status != http.StatusOK || a["status"] != "landed" {
		t.Errorf("the land of demo#%s with the docs token = %d %v, want 200 landed", inside, status, a)
	}
	if c := h.read(t, h.key, inside); c["state"] != "landed" {
		t.Errorf("demo#%s is %v, want landed", inside, c["state"])
	}
}
