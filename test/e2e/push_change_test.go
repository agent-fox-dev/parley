package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// An agent opens a change by pushing a branch with stock git and a push
// option, adds a version with a plain push, and reads the change the git
// output named as soon as the push returns.
func TestPushChange_AnAgentOpensAChangeByPushingAndAddsAVersionWithTheNextPush(t *testing.T) {
	h := startForkHub(t)
	agent := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"git:read", "git:write", "changes:read", "changes:write"}, CreatedAt: time.Now()})
	g := newGitClient(t, h.URL(""), agent)
	dir := filepath.Join(t.TempDir(), "demo")
	run := func(args ...string) string {
		t.Helper()
		out, err := g.git(dir, args...)
		if err != nil {
			t.Fatalf("git %s = %v: %s", strings.Join(args, " "), err, out)
		}
		return out
	}
	if out, err := g.git("", "clone", "--quiet", h.URL("/git/acme/demo.git"), dir); err != nil {
		t.Fatalf("git clone = %v: %s", err, out)
	}
	commit := func(message string) {
		t.Helper()
		run("-c", "user.name=Agent", "-c", "user.email=agent@example.invalid", "commit", "--quiet", "--allow-empty", "-m", message, "-m", "Retries looped forever.")
	}
	run("checkout", "--quiet", "-b", "fix-retry")
	commit("Stop the retry loop")
	if out := run("push", "-o", "parley.change", "-o", "parley.title=Fix retry loop", "origin", "fix-retry"); !strings.Contains(out, "remote: change demo#1 version 1 (opened)") {
		t.Errorf("the first push printed:\n%s\nwant remote: change demo#1 version 1 (opened)", out)
	}
	commit("Bound the retries")
	if out := run("push", "origin", "fix-retry"); !strings.Contains(out, "remote: change demo#1 version 2") {
		t.Errorf("the second push printed:\n%s\nwant remote: change demo#1 version 2", out)
	}

	head := strings.TrimSpace(run("rev-parse", "fix-retry"))
	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/1", agent, nil)
	var ch struct {
		State   string `json:"state"`
		Version int    `json:"version"`
		HeadSHA string `json:"head_sha"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &ch) != nil || ch.State != "open" || ch.Version != 2 || ch.HeadSHA != head {
		t.Errorf("GET changes/1 = %d %s, want open, version 2 at %s", status, body, head)
	}
	run("fetch", "--quiet", "origin", "refs/parley/changes/1/1", "refs/parley/changes/1/2")
}
