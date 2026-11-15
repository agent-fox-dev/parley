package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// cloneAs clones the workspace slug of the hub h with stock git and
// credential, and returns what runs git in the clone, failing the test when
// git fails, and gives its output.
func cloneAs(t *testing.T, h *forkHub, credential, slug string) func(args ...string) string {
	t.Helper()
	g := newGitClient(t, h.URL(""), credential)
	dir := filepath.Join(t.TempDir(), slug)
	if out, err := g.git("", "clone", "--quiet", h.URL("/git/acme/"+slug+".git"), dir); err != nil {
		t.Fatalf("git clone %s = %v: %s", slug, err, out)
	}
	return func(args ...string) string {
		t.Helper()
		out, err := g.git(dir, args...)
		if err != nil {
			t.Fatalf("git %s = %v: %s", strings.Join(args, " "), err, out)
		}
		return out
	}
}

// An agent opens a change by pushing a branch with stock git and a push
// option, adds a version with a plain push, and reads the change the git
// output named as soon as the push returns.
func TestPushChange_AnAgentOpensAChangeByPushingAndAddsAVersionWithTheNextPush(t *testing.T) {
	h := startForkHub(t)
	agent := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"git:read", "git:write", "changes:read", "changes:write"}, CreatedAt: time.Now()})
	run := cloneAs(t, h, agent, "demo")
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
		Title   string `json:"title"`
		State   string `json:"state"`
		Version int    `json:"version"`
		HeadSHA string `json:"head_sha"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &ch) != nil || ch.Title != "Fix retry loop" || ch.State != "open" || ch.Version != 2 || ch.HeadSHA != head {
		t.Errorf("GET changes/1 = %d %s, want \"Fix retry loop\", open, version 2 at %s", status, body, head)
	}
	run("fetch", "--quiet", "origin", "refs/parley/changes/1/1", "refs/parley/changes/1/2")
	if n := gitPushEventsOf(t, h, "refs/heads/fix-retry"); n != 2 {
		t.Errorf("the log holds %d parley.git.push events of refs/heads/fix-retry, want 2", n)
	}
}

// gitPushEventsOf counts the parley.git.push events of ref in the hub's
// log.
func gitPushEventsOf(t *testing.T, h *forkHub, ref string) int {
	t.Helper()
	n := 0
	for _, ev := range logItems(t, h.Hub) {
		if ev.Type == "parley.git.push" && ev.ResourceID == ref {
			n++
		}
	}
	return n
}

// A developer who mistypes a push option has the whole push refused before
// anything is written, and is told which option and why.
func TestPushChange_APushWithAnInvalidOptionIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	h := startForkHub(t)
	g := newGitClient(t, h.URL(""), h.key)
	dir := filepath.Join(t.TempDir(), "demo")
	if out, err := g.git("", "clone", "--quiet", h.URL("/git/acme/demo.git"), dir); err != nil {
		t.Fatalf("git clone demo = %v: %s", err, out)
	}
	for _, args := range [][]string{{"checkout", "--quiet", "-b", "topic"}, {"-c", "user.name=Dev", "-c", "user.email=dev@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "Topic"}} {
		if out, err := g.git(dir, args...); err != nil {
			t.Fatalf("git %s = %v: %s", strings.Join(args, " "), err, out)
		}
	}

	out, err := g.git(dir, "push", "-o", "parley.titel=x", "origin", "topic")
	if err == nil || !strings.Contains(out, "remote: error: invalid push option parley.titel: unknown option") {
		t.Errorf("git push -o parley.titel=x = %v:\n%s\nwant it refused with remote: error: invalid push option parley.titel: unknown option", err, out)
	}
	if out, err := g.git(dir, "ls-remote", "origin", "refs/heads/topic"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("git ls-remote origin refs/heads/topic = %v %q, want nothing", err, out)
	}
	if n := gitPushEventsOf(t, h, "refs/heads/topic"); n != 0 {
		t.Errorf("the log holds %d parley.git.push events of refs/heads/topic, want none", n)
	}
	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes", h.key, nil)
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil || len(page.Items) != 0 {
		t.Errorf("GET /workspaces/demo/changes = %d %s, want an empty list", status, body)
	}
}

// An agent's plain push to its task fork, which opens changes on its own,
// opens a change in the fork's parent from the fork's branch, onto the
// fork's base branch and with the fork's purpose as its intent, while the
// branch stays in the fork.
func TestPushChange_APushToATaskForkOpensAChangeInTheParent(t *testing.T) {
	h := startForkHub(t)
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", h.key, map[string]any{
		"name": "t1", "auto_change": true, "purpose": "speed up retries",
		"token": map[string]any{"permissions": []string{"git:write", "changes:read", "changes:write"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create demo--t1 = %d %s, want 201", status, body)
	}
	created := decoded(t, body)
	slug, token := str(created, "workspace", "slug"), str(created, "token", "token")
	run := cloneAs(t, h, token, slug)
	run("checkout", "--quiet", "-b", "work")
	run("-c", "user.name=Agent", "-c", "user.email=agent@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "Speed up retries")
	if out := run("push", "origin", "work"); !strings.Contains(out, "remote: change demo#1 version 1 (opened)") {
		t.Errorf("the push printed:\n%s\nwant remote: change demo#1 version 1 (opened)", out)
	}

	status, body = h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/1", token, nil)
	ch := decoded(t, body)
	if status != http.StatusOK || str(ch, "source", "workspace_slug") != slug || str(ch, "source", "branch") != "work" ||
		str(ch, "target_branch") != "main" || str(ch, "state") != "open" || str(ch, "intent") != "speed up retries" {
		t.Errorf("GET changes/1 with the fork's token = %d %s, want an open change from %s:work onto main with the fork's purpose", status, body, slug)
	}
	if fork, parent := gitrepotest.Refs(t, h.repo(slug), "refs/heads/"), gitrepotest.Refs(t, h.repo("demo"), "refs/heads/"); fork["work"] == "" || parent["work"] != "" {
		t.Errorf("work is at %q in the fork and %q in demo, want it in the fork alone", fork["work"], parent["work"])
	}
}
