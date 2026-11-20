package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	outlinepkg "github.com/agent-fox-dev/agentkit-go/outline"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// startSCMHub starts a hub whose workspace demo is cloned, the way a
// workspace is registered through the API (create, then clone), from an
// origin that holds what stream imports, and returns the hub and the place
// of demo's repository on disk.
func startSCMHub(t *testing.T, stream string) (*workspaceHub, string) {
	t.Helper()
	origin := gitrepotest.NewOrigin(t, gitrepotest.WithSmartHTTP())
	if out, err := gitrepotest.Run(origin.Dir(), stream, "fast-import", "--quiet"); err != nil {
		t.Fatalf("import the origin's history: %v: %s", err, out)
	}
	var workspaces *workspace.Service
	h := startWorkspaceHub(t, origin, "demo", func(c *hub.Composition) { workspaces = c.Workspaces() })
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key, map[string]any{"slug": "demo", "git_url": origin.URL()})
	if status != http.StatusCreated {
		t.Fatalf("create demo = %d %s, want 201", status, body)
	}
	h.waitStatus(t, "demo", workspace.StatusReady)
	return h, workspaces.RepoPath("demo")
}

// scmAnswer is a source control read's status, headers and body.
type scmAnswer struct {
	status int
	header http.Header
	body   []byte
}

// scmGet sends GET path to the hub with credential as the bearer credential
// and the headers in header, given as name, value pairs.
func scmGet(t *testing.T, h *workspaceHub, path, credential string, header ...string) scmAnswer {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.URL(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return scmAnswer{res.StatusCode, res.Header, body}
}

// An agent with a PAT holding git:read reads a file by name, revalidates it,
// reads it again by the commit id the hub named, and asks for the diff
// between two commits of the history, all without a clone and without
// moving a ref.
func TestSCMRead_AnAgentReadsAFileByNameThenByIdThenADiff(t *testing.T) {
	parallel(t)
	const readmeV2 = "second version\n"
	h, repo := startSCMHub(t, ""+
		"commit refs/heads/main\nmark :1\ncommitter A <a@example.com> 1700000000 +0000\ndata 6\nfirst\n"+
		"M 100644 inline README.md\ndata 15\nfirst version\n\n"+
		"commit refs/heads/main\nmark :2\ncommitter A <a@example.com> 1700000100 +0000\ndata 7\nsecond\nfrom :1\n"+
		"M 100644 inline README.md\ndata 15\n"+readmeV2+"\n")
	pat := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"git:read"}, CreatedAt: time.Now()})
	before := gitrepotest.Git(t, repo, "for-each-ref")

	file := "/api/v1/workspaces/demo/scm/file?ref=main&path=README.md"
	r := scmGet(t, h, file, pat)
	sha := r.header.Get("X-Parley-Resolved-Sha")
	if r.status != http.StatusOK || string(r.body) != readmeV2 || len(sha) != 40 {
		t.Fatalf("GET %s = %d %q, resolved %q; want 200 with README.md at the head of main", file, r.status, r.body, sha)
	}
	if got := r.header.Get("Cache-Control"); got != "private, no-cache" {
		t.Errorf("Cache-Control of a read by name = %q, want private, no-cache", got)
	}
	if again := scmGet(t, h, file, pat, "If-None-Match", r.header.Get("ETag")); again.status != http.StatusNotModified || len(again.body) != 0 {
		t.Errorf("the conditional read = %d with %d bytes, want 304 with none", again.status, len(again.body))
	}
	byID := scmGet(t, h, "/api/v1/workspaces/demo/scm/file?ref="+sha+"&path=README.md", pat)
	if byID.status != http.StatusOK || string(byID.body) != readmeV2 || byID.header.Get("Cache-Control") != "private, max-age=31536000, immutable" {
		t.Errorf("the read by commit id = %d %q, Cache-Control %q; want the same bytes, immutable", byID.status, byID.body, byID.header.Get("Cache-Control"))
	}

	var log struct {
		Items []struct {
			SHA string `json:"sha"`
		} `json:"items"`
	}
	if l := scmGet(t, h, "/api/v1/workspaces/demo/scm/commits?ref=main", pat); l.status != http.StatusOK || json.Unmarshal(l.body, &log) != nil || len(log.Items) != 2 {
		t.Fatalf("GET commits = %d %s, want the two commits of main", l.status, l.body)
	}
	var diff struct {
		Files []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
			Patch  string `json:"patch"`
		} `json:"files"`
	}
	d := scmGet(t, h, "/api/v1/workspaces/demo/scm/diff?base="+log.Items[1].SHA+"&head="+log.Items[0].SHA, pat)
	if d.status != http.StatusOK || json.Unmarshal(d.body, &diff) != nil {
		t.Fatalf("GET diff = %d %s, want 200", d.status, d.body)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "README.md" || diff.Files[0].Status != "modified" || diff.Files[0].Patch == "" {
		t.Errorf("the diff's files = %+v, want README.md modified with a patch", diff.Files)
	}
	if after := gitrepotest.Git(t, repo, "for-each-ref"); after != before {
		t.Errorf("the reads moved refs:\n%s\nwant\n%s", after, before)
	}
}

// A review agent asks from the shell whether feature still applies on main,
// gets the conflict as data and the exit status 3, then reads the target's
// side of the conflicted file; no ref of the workspace moves.
func TestSCMRead_AReviewAgentGetsTheConflictOfABranchFromTheShell(t *testing.T) {
	const onMain = "line from main\n"
	h, repo := startSCMHub(t, ""+
		"commit refs/heads/main\nmark :1\ncommitter A <a@example.com> 1700000000 +0000\ndata 4\nbase\n"+
		"M 100644 inline f.txt\ndata 10\nbase line\n\n"+
		"commit refs/heads/main\nmark :2\ncommitter A <a@example.com> 1700000100 +0000\ndata 5\nmain\nfrom :1\n"+
		"M 100644 inline f.txt\ndata 15\n"+onMain+"\n"+
		"commit refs/heads/feature\nmark :3\ncommitter B <b@example.com> 1700000200 +0000\ndata 8\nfeature\nfrom :1\n"+
		"M 100644 inline f.txt\ndata 18\nline from feature\n\n")
	t.Setenv("PARLEY_ENDPOINT_URL", h.URL(""))
	t.Setenv("PARLEY_API_KEY", h.key)
	before := gitrepotest.Git(t, repo, "for-each-ref")
	run := func(args ...string) ran {
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), args, &stdout, &stderr)
		return ran{code, stdout.String(), stderr.String()}
	}

	c := run("scm", "compare", "demo", "main", "feature", "--fail-on-conflict")
	if c.code != 3 {
		t.Fatalf("scm compare --fail-on-conflict exited %d (stderr %s), want 3", c.code, c.stderr)
	}
	var answer struct {
		Ahead     int    `json:"ahead"`
		Behind    int    `json:"behind"`
		Mergeable string `json:"mergeable"`
		Conflict  struct {
			Files []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"files"`
		} `json:"conflict"`
	}
	if err := json.Unmarshal([]byte(c.stdout), &answer); err != nil {
		t.Fatalf("stdout of scm compare is not one JSON document: %v\n%s", err, c.stdout)
	}
	if answer.Mergeable != "conflict" || answer.Ahead != 1 || answer.Behind != 1 || len(answer.Conflict.Files) != 1 ||
		answer.Conflict.Files[0].Path != "f.txt" || answer.Conflict.Files[0].Kind != "content" {
		t.Errorf("scm compare answered %s, want a content conflict on f.txt, ahead 1, behind 1", c.stdout)
	}
	var envelope struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(c.stderr), &envelope) != nil || envelope.Error.ErrorType != "condition_not_met" {
		t.Errorf("stderr of scm compare = %q, want an envelope with condition_not_met", c.stderr)
	}

	if cat := run("scm", "cat", "demo", "f.txt", "--ref", "main"); cat.code != 0 || cat.stdout != onMain {
		t.Errorf("scm cat f.txt --ref main = %d %q (stderr %s), want 0 and the bytes of f.txt on main", cat.code, cat.stdout, cat.stderr)
	}
	if after := gitrepotest.Git(t, repo, "for-each-ref"); after != before {
		t.Errorf("the compare moved refs:\n%s\nwant\n%s", after, before)
	}
}

// An agent reads from the shell what a file of demo declares, and on which
// lines, before it forks: on main, on a branch that changed the file, and
// for a file in another language than Go.
func TestSCMRead_AnAgentOutlinesAFileBeforeItForks(t *testing.T) {
	const router = "package api\n\ntype Router struct{}\n\nfunc NewRouter() *Router {\n\treturn &Router{}\n}\n"
	const onFeat = router + "\nfunc (r *Router) Mount(prefix string) {}\n"
	const app = "def serve(port):\n    return port\n"
	file := func(path, content string) string {
		return "M 100644 inline " + path + "\ndata " + strconv.Itoa(len(content)) + "\n" + content + "\n"
	}
	h, repo := startSCMHub(t, ""+
		"commit refs/heads/main\nmark :1\ncommitter A <a@example.com> 1700000000 +0000\ndata 5\nmain\n\n"+
		file("internal/api/router.go", router)+file("app.py", app)+
		"\ncommit refs/heads/feat\ncommitter B <b@example.com> 1700000100 +0000\ndata 5\nfeat\n\nfrom :1\n"+
		file("internal/api/router.go", onFeat)+"\n")
	t.Setenv("PARLEY_ENDPOINT_URL", h.URL(""))
	t.Setenv("PARLEY_API_KEY", h.key)
	type outlined struct {
		ResolvedSHA  string `json:"resolved_sha"`
		Language     string `json:"language"`
		Supported    bool   `json:"supported"`
		Declarations []struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Container string `json:"container"`
			StartLine int    `json:"start_line"`
		} `json:"declarations"`
	}
	outline := func(args ...string) outlined {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), append([]string{"scm", "outline", "demo"}, args...), &stdout, &stderr)
		var o outlined
		if err := json.Unmarshal(stdout.Bytes(), &o); code != 0 || err != nil {
			t.Fatalf("parley scm outline demo %v exited %d (stderr %s) and printed %q (%v); want 0 and one JSON document", args, code, stderr.String(), stdout.String(), err)
		}
		return o
	}
	lineOf := func(o outlined, kind, name string) int {
		for _, d := range o.Declarations {
			if d.Kind == kind && d.Name == name {
				return d.StartLine
			}
		}
		return 0
	}
	head := func(branch string) string { return gitrepotest.Refs(t, repo, "refs/heads/")[branch] }

	main := outline("internal/api/router.go")
	if main.ResolvedSHA != head("main") || !main.Supported || lineOf(main, "func", "NewRouter") != 5 || lineOf(main, "type", "Router") != 3 ||
		lineOf(main, "method", "Mount") != 0 {
		t.Errorf("the outline of router.go on main = %+v; want resolved_sha %s, Router on line 3, NewRouter on line 5 and no Mount", main, head("main"))
	}
	feat := outline("internal/api/router.go", "--ref", "feat")
	if feat.ResolvedSHA != head("feat") || lineOf(feat, "func", "NewRouter") != 5 || lineOf(feat, "method", "Mount") != 9 {
		t.Errorf("the outline of router.go on feat = %+v; want resolved_sha %s and the method Mount, which feat added, on line 9", feat, head("feat"))
	}
	// Python has a backend only in a hub built with cgo, as the hub of this
	// test binary is when the outline package says so.
	probe, err := outlinepkg.Outline(context.Background(), "probe.py", []byte(app), outlinepkg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	py := outline("app.py")
	if py.Language != "Python" || py.Supported != (probe.Backend != outlinepkg.BackendNone) {
		t.Errorf("the outline of app.py = %+v; want the language Python, supported as this build outlines it (%s)", py, probe.Backend)
	}
	if py.Supported && lineOf(py, "func", "serve") != 1 {
		t.Errorf("the outline of app.py = %+v; want the function serve on line 1", py)
	}

	resp, err := http.Get(h.URL("/metrics"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, samples := parseExposition(t, resp.Body)
	var counted float64
	for series, v := range samples {
		if strings.HasPrefix(series, "parley_scm_requests_total{") && strings.Contains(series, `op="outline"`) {
			if !strings.Contains(series, `result="ok"`) && v != 0 {
				t.Errorf("%s = %v, want 0", series, v)
			}
			counted += v
		}
	}
	if counted != 3 {
		t.Errorf("the hub's metrics count %v requests under the op outline, want 3", counted)
	}
}
