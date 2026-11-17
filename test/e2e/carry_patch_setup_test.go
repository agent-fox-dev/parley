package e2e

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// startCarryPatchHub starts a hub as startWorkspaceHub does, whose git
// trusts both origin and the upstream up: each test origin has a
// certificate authority of its own, so git is given a bundle of the two.
func startCarryPatchHub(t *testing.T, origin, up *gitrepotest.Origin) *workspaceHub {
	t.Helper()
	return startCarryPatchHubWith(t, origin, up, nil, nil)
}

// startCarryPatchHubWith starts a hub as startCarryPatchHub does, whose
// clock is now (time.Now when nil) and whose composition compose changes
// (nothing when nil), such as the job queue's handlers and clock.
func startCarryPatchHubWith(t *testing.T, origin, up *gitrepotest.Origin, now func() time.Time, compose func(*hub.Composition)) *workspaceHub {
	t.Helper()
	return startCarryPatchHubConfig(t, origin, up, hubtest.Config{Now: now, Compose: compose})
}

// startCarryPatchHubConfig starts a hub as startCarryPatchHub does, with
// what cfg sets of its clock, composition and maintenance interval.
func startCarryPatchHubConfig(t *testing.T, origin, up *gitrepotest.Origin, cfg hubtest.Config) *workspaceHub {
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
		Server:  `external_url = "https://hub.example/"`,
		TOML:    "[workspace]\npath = " + quote(h.root) + "\nworkers = 2\n",
		Env:     map[string]string{"GIT_SSL_CAINFO": caFile, secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(raw)},
		Now:                 cfg.Now,
		Compose:             cfg.Compose,
		MaintenanceInterval: cfg.MaintenanceInterval,
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

	// The new workspace's patch list is empty; a standard workspace has none.
	status, body = h.Call(t, http.MethodGet, "/api/v1/workspaces/variant/patches", h.key, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"items":[]`) {
		t.Errorf("GET the patches of variant = %d %s, want 200 with items []", status, body)
	}
	status, body = h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key, map[string]any{"slug": "plain", "git_url": origin.URL()})
	if status != http.StatusCreated {
		t.Fatalf("create the standard workspace = %d %s, want 201", status, body)
	}
	h.waitStatus(t, "plain", workspace.StatusReady)
	status, body = h.Call(t, http.MethodGet, "/api/v1/workspaces/plain/patches", h.key, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(body), `"workspace_mode_mismatch"`) {
		t.Errorf("GET the patches of a standard workspace = %d %s, want 400 workspace_mode_mismatch", status, body)
	}
}
