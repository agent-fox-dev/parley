package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

// greetingFile is api/greeting.go as the agent of the declaration flow
// first commits it: a doc comment, Greet and a second function.
const greetingFile = "package api\n\n// Greet greets.\nfunc Greet() string {\n\treturn \"hello\"\n}\n\nfunc Farewell() string {\n\treturn \"bye\"\n}\n"

// stderrError is the error envelope parley printed on stderr.
func stderrError(t *testing.T, r ran) map[string]any {
	t.Helper()
	var doc struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.stderr), &doc); err != nil || doc.Error == nil {
		t.Fatalf("stderr is not an error envelope (%v): %q", err, r.stderr)
	}
	return doc.Error
}

// An agent without a clone reads the outline of a file of its fork, reads
// one function, replaces it by name from the shell, and reads it again;
// the commit has the consequences of any other: the branch moves, the push
// and commit events are written, and the change of the branch has a new
// version.
func TestSCMWrite_AnAgentChangesOneFunctionWithoutAClone(t *testing.T) {
	parallel(t)
	h := startForkHub(t)
	r := h.runAs(t, h.memberKey, "fork", "create", "demo", "--name", "decl", "--token-permissions", "git:read,git:write")
	if r.code != 0 {
		t.Fatalf("fork create exited %d: %s", r.code, r.stderr)
	}
	f := r.json(t)
	slug, pat := str(f, "workspace", "slug"), str(f, "token", "token")
	first := scmWrite(t, h.workspaceHub, http.MethodPost, "/api/v1/workspaces/"+slug+"/scm/commits", pat, map[string]any{
		"branch": "greeting", "base_ref": "main", "message": "Add a greeting",
		"operations": []map[string]any{{"op": "put", "path": "api/greeting.go", "content": greetingFile}},
		"change":     map[string]any{"target_branch": "main", "title": "Greet", "intent": "say hello"},
	})
	change, _ := first.json["change"].(map[string]any)
	number, _ := change["number"].(float64)
	if first.status != http.StatusCreated || number == 0 {
		t.Fatalf("the first commit = %d %s, want 201 opening a change", first.status, first.body)
	}

	if o := h.runAs(t, pat, "scm", "outline", slug, "api/greeting.go", "--ref", "greeting"); o.code != 0 || !strings.Contains(o.stdout, `"Greet"`) {
		t.Fatalf("scm outline exited %d: %s %s, want Greet among the declarations", o.code, o.stdout, o.stderr)
	}
	read := func(want string) {
		t.Helper()
		d := h.runAs(t, pat, "scm", "declaration", slug, "api/greeting.go", "Greet", "--ref", "greeting")
		if d.code != 0 {
			t.Fatalf("scm declaration exited %d: %s", d.code, d.stderr)
		}
		decls, _ := d.json(t)["declarations"].([]any)
		if len(decls) != 1 || decls[0].(map[string]any)["content"] != want {
			t.Errorf("scm declaration printed %s, want one declaration whose content is %q", d.stdout, want)
		}
	}
	read("func Greet() string {\n\treturn \"hello\"\n}\n")

	const greet = "func Greet(name string) string {\n\treturn \"hello \" + name\n}\n"
	local := filepath.Join(t.TempDir(), "greet.go")
	if err := os.WriteFile(local, []byte(greet), 0o600); err != nil {
		t.Fatal(err)
	}
	pushesBefore, commitsBefore := len(gitPushEvents(t, h.workspaceHub, slug, "refs/heads/greeting")), len(h.events(t, "parley.scm.commit", slug))
	c := h.runAs(t, pat, "scm", "commit", slug, "--branch", "greeting", "--message", "Greet by name",
		"--replace-declaration", "api/greeting.go#Greet="+local)
	if c.code != 0 {
		t.Fatalf("scm commit exited %d: %s", c.code, c.stderr)
	}
	commit, _ := c.json(t)["commit"].(map[string]any)
	sha, _ := commit["sha"].(string)
	if head := refHead(t, h.workspaceHub, slug, "greeting", pat); head != sha || len(sha) != 40 {
		t.Fatalf("greeting is at %s, want the commit printed, %q", head, sha)
	}
	want := strings.Replace(greetingFile, "func Greet() string {\n\treturn \"hello\"\n}\n", greet, 1)
	if file := scmGet(t, h.workspaceHub, "/api/v1/workspaces/"+slug+"/scm/file?ref=greeting&path=api/greeting.go", pat); string(file.body) != want {
		t.Errorf("api/greeting.go at the new head = %q, want %q", file.body, want)
	}
	pushes := gitPushEvents(t, h.workspaceHub, slug, "refs/heads/greeting")
	if len(pushes) != pushesBefore+1 || pushes[len(pushes)-1].Via != "api" || *pushes[len(pushes)-1].After != sha {
		t.Errorf("push events of greeting = %+v, want one more, via api, after %s", pushes, sha)
	}
	if n := len(h.events(t, "parley.scm.commit", slug)); n != commitsBefore+1 {
		t.Errorf("%d parley.scm.commit events, want %d", n, commitsBefore+1)
	}
	n := strconv.Itoa(int(number))
	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/"+n+"/versions", h.memberKey, nil)
	var versions struct {
		Items []map[string]any `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &versions) != nil || len(versions.Items) != 2 || versions.Items[len(versions.Items)-1]["head_sha"] != sha {
		t.Errorf("GET the change's versions = %d %s, want two, the second at %s", status, body, sha)
	}
	read(greet)
}

// An agent's replacement is refused twice, for an ambiguous name and then
// for a result that does not parse, the branch staying where it was, and
// is accepted once the agent names the method by its type and closes the
// brace.
func TestSCMWrite_AnAgentCorrectsAReplacementTheHubRefused(t *testing.T) {
	parallel(t)
	h := startForkHub(t)
	const svc = "package svc\n\ntype Reader struct{}\n\ntype Writer struct{}\n\nfunc (r *Reader) Close() error {\n\treturn nil\n}\n\nfunc (w *Writer) Close() error {\n\treturn nil\n}\n"
	work := scmWrite(t, h.workspaceHub, http.MethodPost, "/api/v1/workspaces/demo/scm/commits", h.key, map[string]any{
		"branch": "work", "base_ref": "main", "message": "Add svc",
		"operations": []map[string]any{{"op": "put", "path": "svc.go", "content": svc}},
	})
	if work.status != http.StatusCreated {
		t.Fatalf("the commit of svc.go = %d %s, want 201", work.status, work.body)
	}
	local := filepath.Join(t.TempDir(), "close.go")
	write := func(text string) {
		if err := os.WriteFile(local, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(address string) ran {
		return h.runAs(t, h.key, "scm", "commit", "demo", "--branch", "work", "--message", "Flush on close", "--replace-declaration", address+"="+local)
	}
	refused := func(what string, r ran, head string, commits int) map[string]any {
		t.Helper()
		if r.code != 1 {
			t.Fatalf("%s: exited %d, stdout %q; want 1", what, r.code, r.stdout)
		}
		if got := refHead(t, h.workspaceHub, "demo", "work", h.key); got != head {
			t.Errorf("%s: work moved to %s", what, got)
		}
		if n := len(h.events(t, "parley.scm.commit", "demo")); n != commits {
			t.Errorf("%s: %d parley.scm.commit events, want %d", what, n, commits)
		}
		return stderrError(t, r)
	}
	head, commits := refHead(t, h.workspaceHub, "demo", "work", h.key), len(h.events(t, "parley.scm.commit", "demo"))

	write("func (w *Writer) Close() error {\n\treturn w.flush()\n")
	e := refused("svc.go#Close", commit("svc.go#Close"), head, commits)
	details, _ := e["details"].(map[string]any)
	candidates, _ := details["candidates"].([]any)
	var containers []string
	for _, c := range candidates {
		containers = append(containers, c.(map[string]any)["container"].(string))
	}
	if e["error_type"] != "ambiguous_declaration" || strings.Join(containers, ",") != "Reader,Writer" {
		t.Errorf("svc.go#Close printed %s, want ambiguous_declaration with the candidates of Reader and Writer", r2s(e))
	}

	e = refused("svc.go#Writer.Close", commit("svc.go#Writer.Close"), head, commits)
	details, _ = e["details"].(map[string]any)
	errs, _ := details["errors"].([]any)
	if e["error_type"] != "syntax_error" || details["path"] != "svc.go" || len(errs) == 0 {
		t.Errorf("svc.go#Writer.Close printed %s, want syntax_error on svc.go with its positions", r2s(e))
	} else if pos := errs[0].(map[string]any); pos["line"] == nil || pos["column"] == nil {
		t.Errorf("the first error is %v, want a line and a column", pos)
	}

	write("func (w *Writer) Close() error {\n\treturn w.flush()\n}\n")
	c := commit("svc.go#Writer.Close")
	if c.code != 0 {
		t.Fatalf("the corrected commit exited %d: %s", c.code, c.stderr)
	}
	sha, _ := c.json(t)["commit"].(map[string]any)["sha"].(string)
	if got := refHead(t, h.workspaceHub, "demo", "work", h.key); got != sha {
		t.Errorf("work is at %s, want the commit printed, %s", got, sha)
	}
	want := strings.Replace(svc, "func (w *Writer) Close() error {\n\treturn nil\n}\n", "func (w *Writer) Close() error {\n\treturn w.flush()\n}\n", 1)
	if file := scmGet(t, h.workspaceHub, "/api/v1/workspaces/demo/scm/file?ref=work&path=svc.go", h.key); string(file.body) != want {
		t.Errorf("svc.go at work = %q, want the new Close of Writer and the old Close of Reader", file.body)
	}
}

// r2s is an error envelope as text, for a message.
func r2s(e map[string]any) string {
	b, _ := json.Marshal(e)
	return string(b)
}
