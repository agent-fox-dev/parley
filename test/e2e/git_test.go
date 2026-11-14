package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
)

// gitClient runs stock git as a developer's machine would against the hub,
// with none of this machine's git or parley configuration: an empty HOME, no
// system configuration, no terminal prompt, and the parley credential helper
// built from this tree configured for the hub's URL, with the hub URL and the
// credential in PARLEY_ENDPOINT_URL and PARLEY_API_KEY.
type gitClient struct {
	// env is git's whole environment; of a variable given twice the last
	// value counts.
	env []string
	// helper is the -c option that configures the credential helper.
	helper string
}

// newGitClient builds parley and returns the git client of the hub at hubURL
// with credential.
func newGitClient(t *testing.T, hubURL, credential string) *gitClient {
	t.Helper()
	parley := build(t, t.TempDir(), "./cmd/parley", "dev", "none", "")
	return &gitClient{
		env: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"PARLEY_ENDPOINT_URL=" + hubURL,
			"PARLEY_API_KEY=" + credential,
		},
		helper: "credential." + hubURL + ".helper=!" + parley + " credential-helper",
	}
}

// git runs git with args in dir, the test's working directory when dir is
// "", and returns its output, stdout and stderr together.
func (g *gitClient) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", g.helper}, args...)...)
	cmd.Dir, cmd.Env = dir, g.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// seedGitHistory replaces the main branch of the hub's repository dir by
// three commits that hold a.txt (10 bytes) and big.bin (200 kB), and adds
// the version refs refs/parley/changes/1/1 and refs/parley/changes/7/1, each
// on a commit no branch holds. dev and v1 keep what the clone gave them.
// It returns the new head of main.
func seedGitHistory(t *testing.T, dir string) string {
	t.Helper()
	var s strings.Builder
	commit := func(ref string, mark int, from string, files ...string) {
		fmt.Fprintf(&s, "commit %s\nmark :%d\ncommitter Dev <dev@example.invalid> now\ndata 6\ncommit\n", ref, mark)
		if from != "" {
			fmt.Fprintf(&s, "from %s\n", from)
		}
		for i := 0; i < len(files); i += 2 {
			fmt.Fprintf(&s, "M 100644 inline %s\ndata %d\n%s\n", files[i], len(files[i+1]), files[i+1])
		}
	}
	s.WriteString("reset refs/heads/main\n\n")
	commit("refs/heads/main", 1, "", "a.txt", "0123456789")
	commit("refs/heads/main", 2, ":1", "big.bin", strings.Repeat("0123456789abcdef", 12800))
	commit("refs/heads/main", 3, ":2", "a.txt", "9876543210")
	commit("refs/parley/changes/1/1", 4, ":3", "change.txt", "one")
	commit("refs/parley/changes/7/1", 5, ":3", "change.txt", "seven")
	if _, err := gitrepotest.Run(dir, s.String(), "fast-import", "--quiet", "--force", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	return gitrepotest.Git(t, dir, "rev-parse", "refs/heads/main")
}

// gitRequests are the git server's request counts since before.
func gitRequests(h *forkHub, before map[gitserver.RequestLabels]uint64) map[gitserver.RequestLabels]uint64 {
	after := h.comp.GitServer().Requests()
	for k, n := range before {
		after[k] -= n
	}
	return after
}

func TestGit_ADeveloperClonesAWorkspaceWithStockGitAndTheCredentialHelper(t *testing.T) {
	h := startForkHub(t)
	head := seedGitHistory(t, h.repo("demo"))
	url := h.URL("/git/acme/demo.git")
	g := newGitClient(t, h.URL(""), h.key)
	before := h.comp.GitServer().Requests()
	dir := filepath.Join(t.TempDir(), "demo")

	if out, err := g.git("", "clone", url, dir); err != nil {
		t.Fatalf("git clone %s: %v\n%s", url, err, out)
	}
	if out, _ := g.git(dir, "symbolic-ref", "HEAD"); strings.TrimSpace(out) != "refs/heads/main" {
		t.Errorf("the clone's HEAD = %q, want refs/heads/main", out)
	}
	if out, _ := g.git(dir, "rev-parse", "HEAD"); strings.TrimSpace(out) != head {
		t.Errorf("the clone's HEAD is at %q, want the hub's main %s", out, head)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Errorf("the clone's working tree: %v, want a.txt", err)
	}
	refs, _ := g.git(dir, "for-each-ref", "--format=%(refname)")
	if !strings.Contains(refs, "refs/remotes/origin/dev\n") || !strings.Contains(refs, "refs/tags/v1\n") || strings.Contains(refs, "parley") {
		t.Errorf("the clone's refs are\n%s\nwant origin/dev and v1 and none under refs/parley/", refs)
	}

	// The helper is asked for the hub's address while its settings name the
	// same hub under another host name, so it answers nothing.
	other := &gitClient{env: append(slices.Clone(g.env), "PARLEY_ENDPOINT_URL="+strings.Replace(h.URL(""), "127.0.0.1", "localhost", 1)), helper: g.helper}
	if out, err := other.git("", "ls-remote", url); err == nil || !strings.Contains(out, "could not read Username") {
		t.Errorf("git ls-remote %s with the helper set for another host: %v, output %q; want git to find no credential", url, err, out)
	}

	// Without a credential the helper answers nothing, and git has no other.
	g.env = append(g.env, "PARLEY_API_KEY=")
	if out, err := g.git("", "clone", url, filepath.Join(t.TempDir(), "again")); err == nil {
		t.Errorf("git clone without PARLEY_API_KEY succeeded, want a failure:\n%s", out)
	}

	counts := gitRequests(h, before)
	for _, want := range []gitserver.RequestLabels{
		{Service: gitserver.LabelInfoRefs, Result: gitserver.ResultRefused},
		{Service: gitserver.LabelInfoRefs, Result: gitserver.ResultOK},
		{Service: gitserver.LabelUploadPack, Result: gitserver.ResultOK},
	} {
		if counts[want] < 1 {
			t.Errorf("parley_git_requests_total%+v grew by %d, want at least 1 (all: %v)", want, counts[want], counts)
		}
	}
}

// withChange is the hub's fork view naming one more change of the fork, as
// the change feature will once it records the source of a change.
type withChange struct {
	gitserver.ForkView
	number int64
}

func (v withChange) ForkRefs(ctx context.Context, slug string) (gitserver.ForkRefs, error) {
	refs, err := v.ForkView.ForkRefs(ctx, slug)
	refs.Changes = append(refs.Changes, v.number)
	return refs, err
}

func TestGit_AnAgentFetchesFromATaskForkWithABoundToken(t *testing.T) {
	h := startForkHubWith(t, nil, func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.ForkView = withChange{d.ForkView, 7} })
	})
	parent := h.repo("demo")
	seedGitHistory(t, parent)
	if status, body := h.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", h.key,
		map[string]any{"settings": map[string]any{"audit_fetches": true}}); status != http.StatusOK {
		t.Fatalf("PATCH demo settings = %d %s, want 200", status, body)
	}
	r := h.runAs(t, h.memberKey, "fork", "create", "demo", "--name", "t1", "--token-permissions", "git:read")
	if r.code != 0 {
		t.Fatalf("fork create exited %d: %s", r.code, r.stderr)
	}
	f := r.json(t)
	slug, pat := str(f, "workspace", "slug"), str(f, "token", "token")
	if slug != "demo--t1" || pat == "" {
		t.Fatalf("fork %q with token %q, want demo--t1 and a token", slug, pat)
	}
	g := newGitClient(t, h.URL(""), pat)
	forkURL := h.URL("/git/acme/" + slug + ".git")
	d := filepath.Join(t.TempDir(), "fork")
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := g.git(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}

	run("", "clone", "--depth", "1", "--filter=blob:none", "--no-checkout", forkURL, d)
	if n := len(strings.Fields(run(d, "rev-list", "origin/main"))); n != 1 {
		t.Errorf("the shallow clone holds %d commits of origin/main, want 1", n)
	}
	if blobs := strings.Count(run(d, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)"), "blob"); blobs != 0 {
		t.Errorf("the blob-less clone holds %d blobs, want none", blobs)
	}
	if got, want := run(d, "rev-parse", "origin/main"), gitrepotest.Git(t, parent, "rev-parse", "main"); got != want {
		t.Errorf("origin/main = %s, want the parent's main %s", got, want)
	}

	// The parent's main moves; the fork follows it with no other call.
	moved := gitrepotest.PushCommit(t, parent, parent, "main", "moved\n")
	run(d, "fetch", "origin")
	if got := run(d, "rev-parse", "origin/main"); got != moved {
		t.Errorf("after the parent moved, origin/main = %s, want %s", got, moved)
	}

	run(d, "fetch", "origin", "refs/parley/changes/7/1")
	if got, want := run(d, "rev-parse", "FETCH_HEAD"), gitrepotest.Git(t, parent, "rev-parse", "refs/parley/changes/7/1"); got != want {
		t.Errorf("FETCH_HEAD = %s, want the parent's refs/parley/changes/7/1 %s", got, want)
	}

	rows, err := h.Store.Main().Query(`SELECT actor_type, metadata FROM hub_events WHERE type = ? AND workspace = ? ORDER BY seq`, gitserver.EventFetch, slug)
	if err != nil {
		t.Fatal(err)
	}
	type fetchEvent struct{ actor, meta string }
	var events []fetchEvent
	for rows.Next() {
		var e fetchEvent
		if err := rows.Scan(&e.actor, &e.meta); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatalf("no %s event of %s", gitserver.EventFetch, slug)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(events[0].meta), &first); err != nil {
		t.Fatal(err)
	}
	if events[0].actor != "pat" || first["shallow"] != true || first["filter"] != "blob:none" {
		t.Errorf("the first fetch event = actor %s metadata %v, want pat {shallow: true, filter: blob:none}", events[0].actor, first)
	}
	for _, e := range events {
		if e.actor != "pat" {
			t.Errorf("a fetch event of %s has actor_type %s, want pat", slug, e.actor)
		}
	}

	out, err := g.git("", "ls-remote", h.URL("/git/acme/demo.git"))
	if err == nil || !strings.Contains(out, "repository not found") {
		t.Errorf("git ls-remote of the parent with the fork's token: %v\n%s\nwant a failure naming repository not found", err, out)
	}
}
