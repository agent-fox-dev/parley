package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// writeAnswer is a source control write's status and decoded body.
type writeAnswer struct {
	status int
	body   []byte
	json   map[string]any
}

// scmWrite sends method path with body as JSON and credential as the bearer
// credential, and decodes the answer.
func scmWrite(t *testing.T, h *workspaceHub, method, path, credential string, body any) writeAnswer {
	t.Helper()
	status, raw := h.Call(t, method, path, credential, body)
	a := writeAnswer{status: status, body: raw}
	_ = json.Unmarshal(raw, &a.json)
	return a
}

// errorType is the error_type of an error envelope, "" for any other body.
func (a writeAnswer) errorType() string {
	e, _ := a.json["error"].(map[string]any)
	s, _ := e["error_type"].(string)
	return s
}

// refHead is the commit the ref name of the workspace slug points at, as
// the read route answers it.
func refHead(t *testing.T, h *workspaceHub, slug, name, credential string) string {
	t.Helper()
	r := scmGet(t, h, "/api/v1/workspaces/"+slug+"/scm/refs/"+name, credential)
	var ref struct {
		SHA string `json:"sha"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &ref) != nil || len(ref.SHA) != 40 {
		t.Fatalf("GET refs/%s of %s = %d %s, want 200 with its sha", name, slug, r.status, r.body)
	}
	return ref.SHA
}

// refPushEvent is the metadata of a parley.git.push event.
type refPushEvent struct {
	Ref     string  `json:"ref"`
	Before  *string `json:"before"`
	After   *string `json:"after"`
	Created bool    `json:"created"`
	Forced  bool    `json:"forced"`
	Direct  bool    `json:"direct"`
	Via     string  `json:"via"`
}

// gitPushEvents are the parley.git.push events of the workspace slug for ref.
func gitPushEvents(t *testing.T, h *workspaceHub, slug, ref string) []refPushEvent {
	t.Helper()
	var out []refPushEvent
	for _, m := range h.events(t, "parley.git.push", slug) {
		var e refPushEvent
		if err := json.Unmarshal([]byte(m), &e); err != nil {
			t.Fatal(err)
		}
		if e.Ref == ref {
			out = append(out, e)
		}
	}
	return out
}

// An agent with a token bound to its task fork commits three edits to a new
// branch of the fork without a clone, reads the file back, sees the branch
// with stock git, and is told the branch moved when it commits again from a
// head it has not seen.
func TestSCMWrite_AnAgentCommitsToItsForkWithoutACloneAndIsToldTheBranchMoved(t *testing.T) {
	parallel(t)
	h := startForkHub(t)
	r := h.runAs(t, h.memberKey, "fork", "create", "demo", "--name", "t1", "--token-permissions", "git:read,git:write")
	if r.code != 0 {
		t.Fatalf("fork create exited %d: %s", r.code, r.stderr)
	}
	f := r.json(t)
	slug, pat := str(f, "workspace", "slug"), str(f, "token", "token")
	if slug != "demo--t1" || pat == "" {
		t.Fatalf("fork %q with token %q, want demo--t1 and a token", slug, pat)
	}
	mainHead := refHead(t, h.workspaceHub, slug, "main", pat)
	const greeting = "package api\n\nconst Greeting = \"hello\"\n"

	commits := "/api/v1/workspaces/" + slug + "/scm/commits"
	first := scmWrite(t, h.workspaceHub, http.MethodPost, commits, pat, map[string]any{
		"branch": "greeting", "base_ref": "main", "message": "Add a greeting",
		"operations": []map[string]any{
			{"op": "put", "path": "api/greeting.go", "content": greeting},
			{"op": "put", "path": "docs/g.md", "content": "# Greeting\n"},
			{"op": "delete", "path": "README"},
		},
	})
	if first.status != http.StatusCreated {
		t.Fatalf("the first commit = %d %s, want 201", first.status, first.body)
	}
	commit, _ := first.json["commit"].(map[string]any)
	sha, _ := commit["sha"].(string)
	parents, _ := commit["parents"].([]any)
	if len(sha) != 40 || len(parents) != 1 || parents[0] != mainHead {
		t.Fatalf("the commit = %v, want one parent, main's head %s", commit, mainHead)
	}

	if got := refHead(t, h.workspaceHub, slug, "greeting", pat); got != sha {
		t.Errorf("refs/heads/greeting of the fork = %s, want the commit the answer named, %s", got, sha)
	}
	file := scmGet(t, h.workspaceHub, "/api/v1/workspaces/"+slug+"/scm/file?ref=greeting&path=api/greeting.go", pat)
	if file.status != http.StatusOK || string(file.body) != greeting {
		t.Errorf("the file read back = %d %q, want 200 and the bytes put", file.status, file.body)
	}
	g := newGitClient(t, h.URL(""), pat)
	out, err := g.git("", "ls-remote", h.URL("/git/acme/"+slug+".git"), "refs/heads/greeting")
	if err != nil || !strings.HasPrefix(out, sha) {
		t.Errorf("git ls-remote of the fork = %v %q, want refs/heads/greeting at %s", err, out, sha)
	}

	second := scmWrite(t, h.workspaceHub, http.MethodPost, commits, pat, map[string]any{
		"branch": "greeting", "expected_head_sha": mainHead, "message": "Again",
		"operations": []map[string]any{{"op": "put", "path": "docs/g.md", "content": "again\n"}},
	})
	details, _ := second.json["error"].(map[string]any)["details"].(map[string]any)
	if second.status != http.StatusConflict || second.errorType() != "ref_moved" || details["current_sha"] != sha {
		t.Errorf("the second commit = %d %s, want 409 ref_moved with current_sha %s", second.status, second.body, sha)
	}
	if got := refHead(t, h.workspaceHub, slug, "greeting", pat); got != sha {
		t.Errorf("after the refused commit greeting = %s, want it left at %s", got, sha)
	}

	pushes := gitPushEvents(t, h.workspaceHub, slug, "refs/heads/greeting")
	if len(pushes) != 1 || pushes[0].Via != "api" || !pushes[0].Created || pushes[0].After == nil || *pushes[0].After != sha {
		t.Errorf("push events of refs/heads/greeting = %+v, want one, created, via api, after %s", pushes, sha)
	}
	if c := h.events(t, "parley.scm.commit", slug); len(c) != 1 || !strings.Contains(c[0], sha) {
		t.Errorf("parley.scm.commit events of %s = %v, want one naming %s", slug, c, sha)
	}
}

// A developer commits a local file to a new branch from the shell, another
// commit reaches main, and the developer rebases the branch onto it from the
// shell; the branch holds one commit on top of main's new head.
func TestSCMWrite_ADeveloperCommitsALocalFileAndRebasesTheBranchFromTheShell(t *testing.T) {
	parallel(t)
	h := startForkHub(t)
	if status, body := h.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", h.key,
		map[string]any{"settings": map[string]any{"protected_branches": []string{}}}); status != http.StatusOK {
		t.Fatalf("PATCH demo settings = %d %s, want 200", status, body)
	}
	notes := []byte("notes\x00with a NUL byte\n")
	local := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(local, notes, 0o600); err != nil {
		t.Fatal(err)
	}

	c := h.runAs(t, h.key, "scm", "commit", "demo", "--branch", "topic", "--base", "main", "--message", "Add notes", "--put", "notes.md="+local)
	if c.code != 0 {
		t.Fatalf("scm commit exited %d: %s", c.code, c.stderr)
	}
	if branch, _ := c.json(t)["branch"].(map[string]any); branch["name"] != "refs/heads/topic" {
		t.Errorf("scm commit printed %s, want the branch refs/heads/topic", c.stdout)
	}

	head := refHead(t, h.workspaceHub, "demo", "main", h.key)
	other := scmWrite(t, h.workspaceHub, http.MethodPost, "/api/v1/workspaces/demo/scm/commits", h.key, map[string]any{
		"branch": "main", "expected_head_sha": head, "message": "Elsewhere",
		"operations": []map[string]any{{"op": "put", "path": "other.txt", "content": "other\n"}},
	})
	if other.status != http.StatusCreated {
		t.Fatalf("the commit on main = %d %s, want 201", other.status, other.body)
	}
	newMain := refHead(t, h.workspaceHub, "demo", "main", h.key)

	rb := h.runAs(t, h.key, "scm", "rebase", "demo", "topic", "--onto", "main")
	if rb.code != 0 {
		t.Fatalf("scm rebase exited %d: %s", rb.code, rb.stderr)
	}
	j := rb.json(t)
	if j["status"] != "rebased" || j["onto_sha"] != newMain || j["new_head_sha"] == j["old_head_sha"] {
		t.Errorf("scm rebase printed %s, want status rebased onto %s with a new head", rb.stdout, newMain)
	}

	cmp := scmGet(t, h.workspaceHub, "/api/v1/workspaces/demo/scm/compare?base=main&head=topic", h.key)
	var counts struct {
		Ahead  int `json:"ahead"`
		Behind int `json:"behind"`
	}
	if cmp.status != http.StatusOK || json.Unmarshal(cmp.body, &counts) != nil || counts.Ahead != 1 || counts.Behind != 0 {
		t.Errorf("compare main...topic = %d %s, want ahead 1, behind 0", cmp.status, cmp.body)
	}
	file := scmGet(t, h.workspaceHub, "/api/v1/workspaces/demo/scm/file?ref=topic&path=notes.md", h.key)
	if file.status != http.StatusOK || !bytes.Equal(file.body, notes) {
		t.Errorf("notes.md at topic = %d %q, want the local file's bytes %q", file.status, file.body, notes)
	}
	pushes := gitPushEvents(t, h.workspaceHub, "demo", "refs/heads/topic")
	if n := len(pushes); n != 2 || !pushes[1].Forced || pushes[1].Via != "api" {
		t.Errorf("push events of refs/heads/topic = %+v, want the create and then a forced move, via api", pushes)
	}
}

// The workspace's owner commits to the protected default branch with an API
// key as a direct push; the same request with the owner's token bound to the
// workspace is refused by the push rules, and an organization member's by
// the write gate.
func TestSCMWrite_TheOwnerPushesDirectlyToTheProtectedBranchAndOtherCredentialsCannot(t *testing.T) {
	parallel(t)
	h := startForkHub(t)
	bound := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"git:read", "git:write"}, Workspace: "demo", CreatedAt: time.Now()})
	head := refHead(t, h.workspaceHub, "demo", "main", h.key)
	commits := "/api/v1/workspaces/demo/scm/commits"
	changelog := func(expected, line string) map[string]any {
		return map[string]any{"branch": "main", "expected_head_sha": expected, "message": "Changelog",
			"operations": []map[string]any{{"op": "put", "path": "CHANGELOG.md", "content_base64": base64.StdEncoding.EncodeToString([]byte(line))}}}
	}

	owner := scmWrite(t, h.workspaceHub, http.MethodPost, commits, h.key, changelog(head, "owner\n"))
	if owner.status != http.StatusCreated || owner.json["change"] != nil {
		t.Fatalf("the owner's commit = %d %s, want 201 with change null", owner.status, owner.body)
	}
	sha, _ := owner.json["commit"].(map[string]any)["sha"].(string)
	if rec := h.record(t, "demo"); rec.HeadSHA == nil || *rec.HeadSHA != sha {
		t.Errorf("demo's head_sha = %s, want the owner's commit %s", deref(rec.HeadSHA), sha)
	}
	pushes := gitPushEvents(t, h.workspaceHub, "demo", "refs/heads/main")
	if len(pushes) != 1 || !pushes[0].Direct || pushes[0].Via != "api" {
		t.Errorf("push events of refs/heads/main = %+v, want one, direct, via api", pushes)
	}

	if r := scmWrite(t, h.workspaceHub, http.MethodPost, commits, bound, changelog(sha, "bound\n")); r.status != http.StatusForbidden || r.errorType() != "protected_branch" {
		t.Errorf("the bound token's commit = %d %s, want 403 protected_branch", r.status, r.body)
	}
	if r := scmWrite(t, h.workspaceHub, http.MethodPost, commits, h.memberKey, changelog(sha, "member\n")); r.status != http.StatusForbidden || r.errorType() != "forbidden" {
		t.Errorf("the member's commit = %d %s, want 403 forbidden", r.status, r.body)
	}
	if got := refHead(t, h.workspaceHub, "demo", "main", h.key); got != sha {
		t.Errorf("main = %s, want it where the owner put it, %s", got, sha)
	}
	if got := gitrepotest.Git(t, h.repo("demo"), "rev-parse", "refs/heads/main"); strings.TrimSpace(got) != sha {
		t.Errorf("refs/heads/main on disk = %s, want %s", got, sha)
	}
}
