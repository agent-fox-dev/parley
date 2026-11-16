package e2e

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// startCarryPatchHub starts a hub as startWorkspaceHub does, whose git
// trusts both origin and the upstream up: each test origin has a
// certificate authority of its own, so git is given a bundle of the two.
func startCarryPatchHub(t *testing.T, origin, up *gitrepotest.Origin) *workspaceHub {
	t.Helper()
	var bundle []byte
	for _, o := range []*gitrepotest.Origin{origin, up} {
		b, err := os.ReadFile(o.CAFile())
		if err != nil {
			t.Fatal(err)
		}
		bundle = append(bundle, b...)
	}
	caFile := filepath.Join(t.TempDir(), "ca-bundle.pem")
	if err := os.WriteFile(caFile, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	h := &workspaceHub{root: t.TempDir(), origin: origin}
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Server: `external_url = "https://hub.example/"`,
		TOML:   "[workspace]\npath = " + quote(h.root) + "\nworkers = 2\n",
		Env:    map[string]string{"GIT_SSL_CAINFO": caFile, secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(raw)},
	})
	h.user = h.CreateUser(t, "dev")
	h.key = h.IssueKey(t, h.user)
	return h
}

func TestCarryPatch_CreateAndSetUp(t *testing.T) {
	parallel(t)
	origin := gitrepotest.NewOrigin(t)
	origin.Commit("main", "o1")
	originDeploy := origin.Commit("deploy", "the origin's own deploy")
	up := gitrepotest.NewOrigin(t)
	u := up.Commit("main", "u1")
	up.Tag("v9", "main")
	h := startCarryPatchHub(t, origin, up)

	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key, map[string]any{
		"slug": "variant", "git_url": origin.URL(), "workspace_mode": "carry_patch", "upstream_url": up.URL(),
	})
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", status, body)
	}
	w := h.waitStatus(t, "variant", workspace.StatusReady)
	if deref(w.IntegrationBranch) != "deploy" || w.CloneError != nil {
		t.Errorf("ready with integration_branch %s, clone_error %s; want deploy and null", deref(w.IntegrationBranch), deref(w.CloneError))
	}
	copyDir := filepath.Join(h.root, "variant.git")
	deploy := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/deploy")
	upstreamMain := gitrepotest.Git(t, copyDir, "rev-parse", "refs/remotes/upstream/main")
	if deploy != u || upstreamMain != u || u == originDeploy {
		t.Errorf("refs/heads/deploy %s, refs/remotes/upstream/main %s; want both upstream's main %s, not origin's deploy %s",
			deploy, upstreamMain, u, originDeploy)
	}
	if tags := gitrepotest.Git(t, copyDir, "tag"); tags != "" {
		t.Errorf("the copy has the tags %q, want none", tags)
	}
	if got := gitrepotest.Git(t, copyDir, "config", "--local", "--get", "rerere.enabled"); got != "true" {
		t.Errorf("rerere.enabled = %q, want true", got)
	}
}
