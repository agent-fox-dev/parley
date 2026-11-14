package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// workspaceHub is a hub whose git trusts a test origin, with its
// repository root in a directory of the test's own, two clone workers, an
// external URL and a secret key, and a user with an API key.
type workspaceHub struct {
	*hubtest.Hub
	root   string
	key    string
	origin *gitrepotest.Origin
	// starts records, at every start of the hub, what the workspace part
	// finds before its clone workers take up any work.
	mu     sync.Mutex
	starts []startState
}

// startState is what one start of the hub found for the workspace slug
// before its workers started: its status, and the root's entries.
type startState struct {
	status  string
	entries []string
}

// startWorkspaceHub starts the hub for origin, recording the state of the
// workspace slug at every start.
func startWorkspaceHub(t *testing.T, origin *gitrepotest.Origin, slug string) *workspaceHub {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	h := &workspaceHub{root: t.TempDir(), origin: origin}
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Server: `external_url = "https://hub.example/"`,
		TOML:   "[workspace]\npath = " + quote(h.root) + "\nworkers = 2\n",
		Env: map[string]string{
			"GIT_SSL_CAINFO":     origin.CAFile(),
			secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(raw),
		},
		Compose: func(c *hub.Composition) {
			c.AdjustWorkspace(func(d *workspace.Deps) {
				w, _, err := workspace.BySlug(context.Background(), d.Store.Main(), slug)
				if err != nil {
					t.Errorf("read %s at the start: %v", slug, err)
				}
				h.mu.Lock()
				defer h.mu.Unlock()
				h.starts = append(h.starts, startState{string(w.Status), rootEntries(t, d.Root)})
			})
		},
	})
	h.key = h.IssueKey(t, h.CreateUser(t, "dev"))
	clientEnv(t, t.TempDir())
	return h
}

// quote is s as a TOML basic string.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// rootEntries are the names in the repository root.
func rootEntries(t *testing.T, root string) []string {
	t.Helper()
	des, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

// newOrigin is an origin with the branches main (its default) and dev and
// the tag v1.
func newOrigin(t *testing.T, opts ...gitrepotest.Option) *gitrepotest.Origin {
	t.Helper()
	o := gitrepotest.NewOrigin(t, opts...)
	o.History()
	return o
}

// record reads the workspace slug from the hub's store.
func (h *workspaceHub) record(t *testing.T, slug string) workspace.Workspace {
	t.Helper()
	w, found, err := workspace.BySlug(context.Background(), h.Store.Main(), slug)
	if err != nil || !found {
		t.Fatalf("read workspace %s: found %v, %v", slug, found, err)
	}
	return w
}

// waitStatus waits up to 20 s for the workspace slug to reach status.
func (h *workspaceHub) waitStatus(t *testing.T, slug string, status workspace.Status) workspace.Workspace {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		w := h.record(t, slug)
		if w.Status == status {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatalf("workspace %s is %s (clone_error %v), want %s", slug, w.Status, deref(w.CloneError), status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// createEvents are the metadata of the parley.workspace.create events of
// the workspace slug in the hub's event log.
func (h *workspaceHub) createEvents(t *testing.T, slug string) []string {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT metadata FROM hub_events WHERE type = 'parley.workspace.create' AND workspace = ?`, slug)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var metas []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		metas = append(metas, m)
	}
	return metas
}

// refs are the refs of the copy at dir with the commit each points at, as
// git itself lists them.
func refs(t *testing.T, dir string) map[string]string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", dir, "for-each-ref", "--format=%(refname) %(objectname)").Output()
	if err != nil {
		t.Fatalf("list the refs of the copy %s: %v", dir, err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ref, id, ok := strings.Cut(line, " "); ok {
			got[ref] = id
		}
	}
	return got
}

func TestWorkspaceCreate_ADeveloperRegistersAPublicRepositoryAndTheHubClonesIt(t *testing.T) {
	origin := newOrigin(t)
	h := startWorkspaceHub(t, origin, "demo")

	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"--endpoint-url", h.URL(""), "--api-key", h.key,
		"workspace", "create", "--slug", "demo", "--git-url", origin.URL()}, &stdout, &stderr)
	r := ran{code, stdout.String(), stderr.String()}
	if r.code != 0 {
		t.Fatalf("workspace create exited %d; stderr = %q", r.code, r.stderr)
	}
	printed := r.json(t)
	if printed["status"] != "pending" || printed["access"] != "owner" || printed["hub_url"] != "https://hub.example/git/dev/demo.git" {
		t.Errorf("printed status %v, access %v, hub_url %v; want pending, owner, https://hub.example/git/dev/demo.git",
			printed["status"], printed["access"], printed["hub_url"])
	}

	w := h.waitStatus(t, "demo", workspace.StatusReady)
	if deref(w.DefaultBranch) != "main" || deref(w.HeadSHA) != origin.Head("main") {
		t.Errorf("ready with default_branch %s, head_sha %s; want main, %s", deref(w.DefaultBranch), deref(w.HeadSHA), origin.Head("main"))
	}
	copyDir := filepath.Join(h.root, "demo.git")
	got := refs(t, copyDir)
	want := map[string]string{"refs/heads/main": origin.Head("main"), "refs/heads/dev": origin.Head("dev"), "refs/tags/v1": gitrepotest.Git(t, origin.Dir(), "rev-parse", "v1")}
	for ref, id := range want {
		if got[ref] != id {
			t.Errorf("the copy's %s = %q, want %q", ref, got[ref], id)
		}
	}
	if events := h.createEvents(t, "demo"); len(events) != 1 {
		t.Errorf("create events of demo = %d, want 1", len(events))
	}
}

func TestWorkspaceCreate_ADeveloperRegistersAPrivateRepositoryWithAToken(t *testing.T) {
	const token = "tok-123"
	origin := newOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", token))
	h := startWorkspaceHub(t, origin, "priv")

	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key,
		map[string]any{"slug": "priv", "git_url": origin.URL(), "git_pat": token})
	if status != http.StatusCreated || bytes.Contains(body, []byte(token)) {
		t.Fatalf("create priv = %d %s, want 201 without the token", status, body)
	}
	h.waitStatus(t, "priv", workspace.StatusReady)
	if got := refs(t, filepath.Join(h.root, "priv.git")); got["refs/heads/main"] != origin.Head("main") || got["refs/heads/dev"] != origin.Head("dev") {
		t.Errorf("the copy's branches = %v, want main and dev at origin's heads", got)
	}

	status, body = h.Call(t, http.MethodGet, "/api/v1/workspaces/priv/secrets", h.key, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"GIT_PAT"`)) {
		t.Errorf("the secrets of priv = %d %s, want GIT_PAT listed", status, body)
	}
	if bytes.Contains(body, []byte(token)) {
		t.Errorf("the secret list shows the token: %s", body)
	}

	status, body = h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key,
		map[string]any{"slug": "priv2", "git_url": origin.URL(), "git_pat": "wrong"})
	var refusal struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil || status != http.StatusBadRequest || refusal.Error.ErrorType != "credential_check_failed" {
		t.Errorf("create priv2 with a wrong token = %d %s, want 400 credential_check_failed", status, body)
	}
	if _, found, err := workspace.BySlug(context.Background(), h.Store.Main(), "priv2"); found || err != nil {
		t.Errorf("priv2 exists after the refused create (found %v, %v)", found, err)
	}

	for _, meta := range h.createEvents(t, "priv") {
		if strings.Contains(meta, token) {
			t.Errorf("the create event holds the token: %s", meta)
		}
	}
	if strings.Contains(h.Log.String(), token) {
		t.Error("the hub log holds the token")
	}
	err := filepath.WalkDir(h.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err == nil && bytes.Contains(content, []byte(token)) {
			t.Errorf("%s holds the token", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceCreate_AnOperatorRestartsTheHubAndAnInterruptedCloneIsResumed(t *testing.T) {
	origin := newOrigin(t)
	h := startWorkspaceHub(t, origin, "slow")
	origin.Block()

	if status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key,
		map[string]any{"slug": "slow", "git_url": origin.URL()}); status != http.StatusCreated {
		t.Fatalf("create slow = %d %s, want 201", status, body)
	}
	h.waitStatus(t, "slow", workspace.StatusCloning)
	for deadline := time.Now().Add(10 * time.Second); origin.Requests() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the clone never reached origin")
		}
	}

	h.Restart(t)
	h.mu.Lock()
	starts := slices.Clone(h.starts)
	h.mu.Unlock()
	if len(starts) != 2 {
		t.Fatalf("recorded %d starts, want 2", len(starts))
	}
	if s := starts[1]; (s.status != "pending" && s.status != "cloning") || len(s.entries) != 0 {
		t.Errorf("after the stop: status %q and root entries %v; want pending or cloning and an empty root", s.status, s.entries)
	}

	origin.Release()
	w := h.waitStatus(t, "slow", workspace.StatusReady)
	if deref(w.HeadSHA) != origin.Head("main") {
		t.Errorf("head_sha after the restart = %s, want %s", deref(w.HeadSHA), origin.Head("main"))
	}
}
