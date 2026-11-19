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
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/cli"
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
// runs every command with success, and the ready repository demo, in the
// org acme, of the user whose API key is key.
type sandboxCreateHub struct {
	*hubtest.Hub
	fake    *runtimetest.Fake
	key     string
	org     string
	secrets *secret.Component
	// workspaces is the hub's workspace service, which knows where each
	// workspace's repository lives.
	workspaces *workspace.Service

	mu sync.Mutex
	// commands are the commands the fake ran, by sandbox.
	commands map[string][][]string
}

// startSandboxCreateHub starts a sandbox create hub with the credential
// model, whose secret is MODEL_KEY, and profiles, the TOML of the
// [[sandbox.profiles]]; each of compose is also run on the hub's
// composition.
func startSandboxCreateHub(t *testing.T, profiles string, compose ...func(*hub.Composition)) *sandboxCreateHub {
	t.Helper()
	return startSandboxCreateHubWith(t, sandboxHubConfig{profiles: profiles, compose: compose})
}

// sandboxHubConfig is what a sandbox create hub is started with besides
// what every one has: the TOML of its [[sandbox.profiles]], keys of its
// [sandbox] table, the options of its fake runtime, what wraps the hub's
// client of the fake (nil for nothing), functions run on its composition
// and the clock the test moves (nil for the real clock), which is the hub's
// clock and the one the sandbox feature's timers run on.
type sandboxHubConfig struct {
	profiles, sandboxKeys string
	fake                  []runtimetest.Option
	wrap                  func(runtime.Client) runtime.Client
	compose               []func(*hub.Composition)
	clock                 *hubtest.JobClock
}

// startSandboxCreateHubWith starts a sandbox create hub as cfg says.
func startSandboxCreateHubWith(t *testing.T, cfg sandboxHubConfig) *sandboxCreateHub {
	t.Helper()
	h := &sandboxCreateHub{fake: runtimetest.New(t, cfg.fake...), commands: map[string][][]string{}}
	h.fake.AddProviderProfile("parley", runtime.ProviderProfile{ID: "anthropic", Variables: []string{"ANTHROPIC_API_KEY"}})
	h.fake.SetExec(func(ctx context.Context, name string, req runtime.ExecRequest, stdout, _ io.Writer) int {
		h.mu.Lock()
		h.commands[name] = append(h.commands[name], req.Command)
		h.mu.Unlock()
		if slices.Equal(req.Command, []string{"git", "rev-parse", "HEAD"}) {
			_, _ = io.WriteString(stdout, sandboxHead+"\n")
		}
		return 0
	})
	var (
		workspaces *workspace.Service
		hubNow     func() time.Time
	)
	if cfg.clock != nil {
		hubNow = cfg.clock.Now
	}
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Now:    hubNow,
		Server: "external_url = \"https://hub.test\"\n",
		TOML: "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" + cfg.sandboxKeys +
			"[[sandbox.credentials]]\nname = \"model\"\nprovider_profile = \"anthropic\"\nsecret = \"MODEL_KEY\"\n" + cfg.profiles,
		Files: map[string]string{"gateway-token": "gw-tok-SECRET\n"},
		Env:   map[string]string{secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))},
		Compose: func(c *hub.Composition) {
			workspaces, h.secrets = c.Workspaces(), c.Secrets()
			h.workspaces = workspaces
			if cfg.clock != nil {
				c.AdjustSandbox(func(d *sandbox.Deps) {
					d.NewTimer = func(d time.Duration) sandbox.Timer { return cfg.clock.NewTimer(d) }
				})
			}
			c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
				client := h.fake.Client(s.OpenShellWorkspace, deps.Recorder)
				if cfg.wrap != nil {
					client = cfg.wrap(client)
				}
				return client, nil
			})
			for _, fn := range cfg.compose {
				fn(c)
			}
		},
	})
	user := h.CreateUser(t, "agent")
	h.key = h.IssueKey(t, user)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, user)
	dir := workspaces.RepoPath("demo")
	gitrepotest.InitBare(t, dir, "main")
	gitrepotest.SetRef(t, dir, "refs/heads/main", gitrepotest.Commit(t, dir, "first", map[string]string{"README": "demo\n"}))
	now := time.Now().UTC().Truncate(time.Second)
	err := h.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		return workspace.Insert(context.Background(), tx, workspace.Workspace{
			Slug: "demo", Kind: access.KindRepository, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
			OwnerID: user, OrgID: h.org, Status: workspace.StatusReady, DisplayName: "demo",
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

// putSecret stores the secret name with value in scope of the owner id.
func (h *sandboxCreateHub) putSecret(t *testing.T, scope secret.Scope, id, name, value string) {
	t.Helper()
	err := h.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		return h.secrets.Put(context.Background(), tx, scope, id, name, value)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// parley runs the CLI in-process against the hub with the hub's key.
func (h *sandboxCreateHub) parley(t *testing.T, args ...string) ran {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--endpoint-url", h.URL(""), "--api-key", h.key}, args...), &stdout, &stderr)
	return ran{code, stdout.String(), stderr.String()}
}

// ran is the commands the fake ran in the sandbox name.
func (h *sandboxCreateHub) ran(name string) [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.commands[name])
}

func TestSandboxCreateAndWait(t *testing.T) {
	parallel(t)
	h := startSandboxCreateHub(t, "[[sandbox.profiles]]\nname = \"full\"\ndefault = true\nttl = \"4h\"\nhub_access = \"fork\"\n"+
		"credentials = [\"model\"]\nexpose_secrets = [\"NPM_TOKEN\"]\n")
	h.object(t, http.MethodPost, "/api/v1/workspaces/demo/forks", map[string]any{"name": "t1"}, http.StatusCreated)
	const npmValue = "npm-SECRET"
	h.putSecret(t, secret.ScopeOrg, h.org, "MODEL_KEY", "model-SECRET")
	h.putSecret(t, secret.ScopeWorkspace, "demo--t1", "NPM_TOKEN", npmValue)

	// 1s is the shortest --poll-interval the duration form allows.
	r := h.parley(t, "sandbox", "create", "demo--t1", "--wait", "--poll-interval", "1s", "--", "make", "test")

	if r.code != 0 {
		t.Fatalf("parley sandbox create --wait = %d, stdout %q, stderr %q; want 0", r.code, r.stdout, r.stderr)
	}
	s := r.json(t)
	id, _ := s["id"].(string)
	checkout, _ := s["checkout"].(map[string]any)
	if s["state"] != sandbox.StateReady || !reflect.DeepEqual(s["credentials"], []any{"model"}) ||
		!reflect.DeepEqual(s["exposed_secrets"], []any{"NPM_TOKEN"}) || checkout["branch"] != "t1" {
		t.Errorf("printed %v; want a ready sandbox with credentials [model], exposed_secrets [NPM_TOKEN] on the branch t1", s)
	}
	rows, err := h.Store.Main().Query(`SELECT type FROM hub_events
		WHERE resource_id = ? OR resource_id IN (SELECT token_id FROM pats WHERE workspace = ?) ORDER BY seq`, id, "demo--t1")
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		events = append(events, typ)
	}
	rows.Close()
	if !reflect.DeepEqual(slices.Sorted(slices.Values(events)), []string{"parley.sandbox.create", "parley.sandbox.ready", "parley.token.bind"}) ||
		events[len(events)-1] != "parley.sandbox.ready" {
		t.Errorf("events of the sandbox and its hub credential %v; want create, ready and token.bind, ready last", events)
	}
	spec, held := h.fake.Spec("parley", id)
	if !held || spec.Env["PARLEY_WORKSPACE"] != "demo--t1" || spec.Env["NPM_TOKEN"] != npmValue {
		t.Errorf("the runtime holds %s (%t) with PARLEY_WORKSPACE %q and NPM_TOKEN's value: %t; want demo--t1 and the secret's value",
			id, held, spec.Env["PARLEY_WORKSPACE"], spec.Env["NPM_TOKEN"] == npmValue)
	}
	if ran := h.ran(id); len(ran) == 0 || !slices.Equal(ran[len(ran)-1], []string{"make", "test"}) {
		t.Errorf("the sandbox ran %v; want make test last, as its main process", ran)
	}
	var mains int
	for _, c := range h.fake.Calls() {
		if c.Op == runtime.OpRunMain && c.Target == id {
			mains++
		}
	}
	if mains != 1 {
		t.Errorf("the runtime started %d main processes in %s; want 1", mains, id)
	}

	list := h.parley(t, "sandbox", "list", "demo", "--include-forks")
	items, _ := list.json(t)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != id {
		t.Errorf("parley sandbox list demo --include-forks = %d %s; want the sandbox %s alone", list.code, list.stdout, id)
	}
	if got := h.parley(t, "sandbox", "get", "demo--t1", id); got.code != 0 || got.json(t)["id"] != id {
		t.Errorf("parley sandbox get demo--t1 %s = %d %s; want the sandbox", id, got.code, got.stdout)
	}
}

func TestForkAndSandboxInOneCall(t *testing.T) {
	parallel(t)
	// The profile needs lists the credential model, whose secret MODEL_KEY
	// is set nowhere.
	h := startSandboxCreateHub(t, "[[sandbox.profiles]]\nname = \"agent\"\ndefault = true\nimage = \"img:1\"\nttl = \"4h\"\nhub_access = \"fork\"\n"+
		"[[sandbox.profiles]]\nname = \"needs\"\nttl = \"4h\"\ncredentials = [\"model\"]\n")

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
