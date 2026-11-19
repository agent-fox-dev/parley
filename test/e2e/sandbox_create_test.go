package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// sandboxHead is the commit git rev-parse HEAD prints in a sandbox of the
// fake runtime.
const sandboxHead = "89abcdef0123456789abcdef0123456789abcdef"

// sandboxCreateHub is a hub with sandboxes on over the fake runtime, which
// runs every git command of a checkout with success, and the ready
// repository demo of the user whose API key is key.
type sandboxCreateHub struct {
	*hubtest.Hub
	fake *runtimetest.Fake
	key  string
}

// startSandboxCreateHub starts a sandbox create hub with the profile agent,
// marked default, without credentials, and the profile needs, whose
// credential model's secret MODEL_KEY is set nowhere.
func startSandboxCreateHub(t *testing.T) *sandboxCreateHub {
	t.Helper()
	h := &sandboxCreateHub{fake: runtimetest.New(t)}
	h.fake.AddProviderProfile("parley", runtime.ProviderProfile{ID: "anthropic", Variables: []string{"ANTHROPIC_API_KEY"}})
	h.fake.SetExec(func(ctx context.Context, _ string, req runtime.ExecRequest, stdout, _ io.Writer) int {
		if slices.Equal(req.Command, []string{"git", "rev-parse", "HEAD"}) {
			_, _ = io.WriteString(stdout, sandboxHead+"\n")
		}
		return 0
	})
	var workspaces *workspace.Service
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Server: "external_url = \"https://hub.test\"\n",
		TOML: "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" +
			"[[sandbox.credentials]]\nname = \"model\"\nprovider_profile = \"anthropic\"\nsecret = \"MODEL_KEY\"\n" +
			"[[sandbox.profiles]]\nname = \"agent\"\ndefault = true\nimage = \"img:1\"\nttl = \"4h\"\nhub_access = \"fork\"\n" +
			"[[sandbox.profiles]]\nname = \"needs\"\nttl = \"4h\"\ncredentials = [\"model\"]\n",
		Files: map[string]string{"gateway-token": "gw-tok-SECRET\n"},
		Env:   map[string]string{secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))},
		Compose: func(c *hub.Composition) {
			workspaces = c.Workspaces()
			c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
				return h.fake.Client(s.OpenShellWorkspace, deps.Recorder), nil
			})
		},
	})
	user := h.CreateUser(t, "agent")
	h.key = h.IssueKey(t, user)
	org := h.CreateOrg(t, "acme")
	h.AddMember(t, org, user)
	dir := workspaces.RepoPath("demo")
	gitrepotest.InitBare(t, dir, "main")
	gitrepotest.SetRef(t, dir, "refs/heads/main", gitrepotest.Commit(t, dir, "first", map[string]string{"README": "demo\n"}))
	now := time.Now().UTC().Truncate(time.Second)
	err := h.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		return workspace.Insert(context.Background(), tx, workspace.Workspace{
			Slug: "demo", Kind: access.KindRepository, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
			OwnerID: user, OrgID: org, Status: workspace.StatusReady, DisplayName: "demo",
			CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// object sends method path with the hub's key and body and decodes the
// JSON object of an answer that must have status.
func (h *sandboxCreateHub) object(t *testing.T, method, path string, body any, status int) map[string]any {
	t.Helper()
	got, raw := h.Call(t, method, path, h.key, body)
	if got != status {
		t.Fatalf("%s %s = %d %s; want %d", method, path, got, raw, status)
	}
	var o map[string]any
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("%s %s: %s: %v", method, path, raw, err)
	}
	return o
}

func TestForkAndSandboxInOneCall(t *testing.T) {
	parallel(t)
	h := startSandboxCreateHub(t)

	r := h.object(t, http.MethodPost, "/api/v1/workspaces/demo/forks", map[string]any{"name": "task2", "sandbox": map[string]any{}}, http.StatusCreated)
	created, _ := r["sandbox"].(map[string]any)
	if created["state"] != sandbox.StateProvisioning {
		t.Fatalf("the fork create's sandbox = %v; want one in provisioning", created)
	}
	id, _ := created["id"].(string)
	s := h.object(t, http.MethodGet, "/api/v1/workspaces/demo--task2/sandboxes/"+id+"?wait_for=ready&wait=30", nil, http.StatusOK)
	checkout, _ := s["checkout"].(map[string]any)
	ha, _ := s["hub_access"].(map[string]any)
	scopes, _ := ha["scopes"].([]any)
	if s["state"] != sandbox.StateReady || checkout["branch"] != "task2" || len(scopes) != 6 {
		t.Errorf("sandbox = %v, checkout %v, hub_access %v; want ready on the branch task2 with the six scopes", s["state"], s["checkout"], s["hub_access"])
	}
	items, _ := h.object(t, http.MethodGet, "/api/v1/workspaces/demo--task2/sandboxes", nil, http.StatusOK)["items"].([]any)
	if len(items) != 1 {
		t.Errorf("demo--task2 lists %d sandboxes; want 1", len(items))
	}
	expires, err := time.Parse(time.RFC3339, s["expires_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var tokenID string
	var tokens int
	err = h.Store.Main().QueryRow(`SELECT count(*), max(token_id) FROM pats WHERE workspace = ? AND revoked_at IS NULL`, "demo--task2").Scan(&tokens, &tokenID)
	if err != nil || tokens != 1 {
		t.Fatalf("live tokens bound to demo--task2: %d (%v); want one", tokens, err)
	}
	if pat, err := identity.PATByID(context.Background(), h.Store.Main(), tokenID); err != nil || pat.ExpiresAt == nil || !pat.ExpiresAt.Equal(expires) {
		t.Errorf("the token bound to demo--task2 expires at %v (%v); want the sandbox's expiry %v", pat.ExpiresAt, err, expires)
	}

	status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", h.key, map[string]any{"name": "task3", "sandbox": map[string]any{"profile": "needs"}})
	var refused struct {
		Error struct {
			ErrorType   string `json:"error_type"`
			Credentials []any  `json:"credentials"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &refused)
	if status != http.StatusUnprocessableEntity || refused.Error.ErrorType != "credential_missing" || !reflect.DeepEqual(refused.Error.Credentials, []any{"model"}) {
		t.Errorf("the fork create with profile needs = %d %s; want 422 credential_missing naming model", status, raw)
	}
	if status, raw := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo--task3", h.key, nil); status != http.StatusNotFound {
		t.Errorf("GET demo--task3 = %d %s; want 404", status, raw)
	}
	held, err := h.fake.Client("parley", sandbox.NopRecorder{}).ListSandboxes(context.Background())
	if err != nil || len(held) != 1 {
		t.Errorf("the runtime holds %d sandboxes (%v); want 1", len(held), err)
	}
}
