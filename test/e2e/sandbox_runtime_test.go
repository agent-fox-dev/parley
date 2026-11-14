package e2e

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell/openshelltest"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// gatewayRelease is the release the gateway of these tests reports, inside
// the supported range.
const gatewayRelease = "0.1.8"

// sandboxConfig turns sandboxes on: [server].external_url and a [sandbox]
// section reaching the gateway with the token in the file gateway-token, the
// credential model of the provider profile anthropic and the profile agent,
// which attaches it and sets one env value.
var sandboxConfig = hubtest.Config{
	Server: "external_url = \"https://hub.test\"\n",
	TOML: "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" +
		"[[sandbox.credentials]]\nname = \"model\"\nprovider_profile = \"anthropic\"\nsecret = \"MODEL_KEY\"\n" +
		"[[sandbox.profiles]]\nname = \"agent\"\nttl = \"4h\"\ncredentials = [\"model\"]\nenv = { A = \"va\" }\n",
	Files: map[string]string{"gateway-token": "gw-tok-SECRET\n"},
}

// newGateway returns the SDK's in-memory gateway of release gatewayRelease,
// holding the provider profile anthropic, whose credential goes in
// ANTHROPIC_API_KEY, in the workspace parley.
func newGateway() *openshelltest.Gateway {
	gw := openshelltest.New(fake.WithGatewayInfo(&v1.GatewayInfo{Version: gatewayRelease}))
	gw.AddProfile("parley", v1.ProviderProfile{ID: "anthropic", Credentials: []v1.ProfileCredential{{Name: "api_key", EnvVars: []string{"ANTHROPIC_API_KEY"}}}})
	return gw
}

// startSandboxHub starts a hub with cfg whose runtime client is the
// OpenShell implementation over gw; cfg.Compose, when set, runs after the
// runtime is set.
func startSandboxHub(t *testing.T, cfg hubtest.Config, gw v1.ClientInterface) *hubtest.Hub {
	t.Helper()
	compose := cfg.Compose
	cfg.Compose = func(c *hub.Composition) {
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			return openshell.New(s, openshell.Deps{
				Scrubber: deps.Scrubber,
				Recorder: deps.Recorder,
				Dial:     func(v1.Config) (v1.ClientInterface, error) { return gw, nil },
			})
		})
		if compose != nil {
			compose(c)
		}
	}
	return hubtest.StartWith(t, cfg)
}

func TestSandboxRuntime_AnOperatorEnablesSandboxesAndADeveloperListsTheProfiles(t *testing.T) {
	gw := newGateway()
	h := startSandboxHub(t, sandboxConfig, gw)
	key := h.IssueKey(t, h.CreateUser(t, "dev"))

	if _, err := gw.Fake().Workspaces().Get(context.Background(), "parley"); err != nil {
		t.Errorf("the gateway's workspace parley: %v", err)
	}
	if _, ok := gw.Profile("parley", "parley-hub"); !ok {
		t.Error("the gateway holds no provider profile parley-hub in the workspace parley")
	}

	clientEnv(t, t.TempDir())
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"sandbox", "profiles", "--endpoint-url", h.URL(""), "--api-key", key}, &stdout, &stderr)
	r := ran{code, stdout.String(), stderr.String()}
	if r.code != 0 {
		t.Fatalf("parley sandbox profiles exited %d; stderr = %q", r.code, r.stderr)
	}
	items, _ := r.json(t)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("parley sandbox profiles printed %d items, want 1: %s", len(items), r.stdout)
	}
	agent, _ := items[0].(map[string]any)
	if agent["name"] != "agent" || !reflect.DeepEqual(agent["env_keys"], []any{"A"}) {
		t.Errorf("the item = %v, want agent with env_keys [A]", agent)
	}
	if _, ok := agent["env"]; ok {
		t.Errorf("the item = %v, want no member env", agent)
	}
	if strings.Contains(r.stdout, `"va"`) {
		t.Errorf("stdout holds the env value: %s", r.stdout)
	}

	status, doc := getAuthorized(t, h.URL("/version"), "")
	runtimeDoc, _ := doc["sandbox_runtime"].(map[string]any)
	if status != http.StatusOK || runtimeDoc["connected"] != gatewayRelease {
		t.Errorf("GET /version = %d %v, want sandbox_runtime.connected %q", status, doc, gatewayRelease)
	}
}

func TestSandboxRuntime_AnOperatorStartsTheHubAgainstAGatewayThatIsNotThere(t *testing.T) {
	gatewayPort, hubPort := closedPort(t), closedPort(t)
	dir := t.TempDir()
	writeConfig(t, dir, "[server]\nbind = \"127.0.0.1\"\nport = "+strconv.Itoa(hubPort)+"\nexternal_url = \"https://hub.test\"\n"+
		"[sandbox]\ngateway_url = \"http://127.0.0.1:"+strconv.Itoa(gatewayPort)+"\"\ntoken_file = \"gateway-token\"\n"+
		"[[sandbox.profiles]]\nname = \"agent\"\nttl = \"4h\"\n")
	if err := os.WriteFile(filepath.Join(dir, "gateway-token"), []byte("gw-tok-SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The default runtime factory: the OpenShell client dialling the
	// closed port.
	var stdout, stderr hubtest.LogBuffer
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	began := time.Now()
	code := hub.Run(ctx, hub.Options{
		Env:     config.MapEnv(map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir}),
		WorkDir: dir, Stdout: &stdout, Stderr: &stderr,
	})
	took := time.Since(began)

	if code != 1 {
		t.Errorf("parleyd returned %d, want 1", code)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "sandbox.gateway_url") {
		t.Errorf("stderr = %q, want one line naming sandbox.gateway_url", stderr.String())
	}
	for _, out := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(out, "gw-tok-SECRET") || strings.Contains(out, "listening") {
			t.Errorf("an output holds the gateway token or a listening line: %q", out)
		}
	}
	if conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hubPort))); err == nil {
		conn.Close()
		t.Errorf("the hub's port %d accepts connections", hubPort)
	}
	// A refused connection fails at once; only a gateway that does not
	// answer at all waits for the start-up check's 30 s.
	if took > 10*time.Second {
		t.Errorf("parleyd took %v to give up on a refused connection, want well under the 30 s budget", took)
	}
}

func TestSandboxRuntime_AnAgentAsksAHubWithoutSandboxesForTheProfiles(t *testing.T) {
	h := hubtest.StartWith(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.SetSandboxRuntime(func(*sandbox.Settings, sandbox.RuntimeDeps) (runtime.Client, error) {
			t.Error("the hub built a runtime client without a [sandbox] section")
			return nil, errors.New("no runtime here")
		})
	}})
	pat := h.IssuePAT(t, identity.PATSpec{UserID: h.CreateUser(t, "agent"), Permissions: []string{"sandboxes:read"}})

	status, body := getAuthorized(t, h.URL("/api/v1/sandbox-profiles"), "Bearer "+pat)
	errDoc, _ := body["error"].(map[string]any)
	if status != http.StatusNotFound || errDoc["error_type"] != "sandboxes_disabled" {
		t.Errorf("GET /api/v1/sandbox-profiles = %d %v, want 404 sandboxes_disabled", status, body)
	}
	if _, doc := getAuthorized(t, h.URL("/version"), ""); doc["sandbox_runtime"] != nil {
		t.Errorf("GET /version = %v, want no sandbox_runtime member", doc)
	}
}

func TestSandboxRuntime_TheGatewayGoesAwayWhileTheHubRuns(t *testing.T) {
	gw := &countingGateway{ClientInterface: newGateway()}
	src := oneSandbox{sandbox.Item{
		CreatedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), ID: "sb-1",
		Object: map[string]any{"id": "sb-1", "workspace": "dev-ws", "state": "ready"},
	}}
	var svc *sandbox.Service
	cfg := sandboxConfig
	cfg.Compose = func(c *hub.Composition) {
		svc = c.Sandbox()
		svc.RegisterSource(src)
		// A route that needs the runtime: it answers the error of a get.
		c.AddRoute(svc.WorkspaceRoute(sandbox.KindRead, server.Route{
			Method: http.MethodGet, Pattern: "/workspaces/:slug/sandboxes/probe",
			Handler: func(w http.ResponseWriter, r *http.Request) error {
				if _, err := svc.Client().GetSandbox(r.Context(), "probe"); err != nil {
					return err
				}
				server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
				return nil
			},
		}))
	}
	h := startSandboxHub(t, cfg, gw)
	dev := h.CreateUser(t, "dev")
	h.AddWorkspace(t, access.Record{Slug: "dev-ws", OwnerID: dev, OrgID: h.CreateOrg(t, "devs"), Kind: access.KindRepository, Status: access.StatusReady})
	key := h.IssueKey(t, dev)

	gw.ClientInterface.(*openshelltest.Gateway).SetUnreachable(true)
	got, body := getAuthorized(t, h.URL("/api/v1/workspaces/dev-ws/sandboxes/probe"), "Bearer "+key)
	errDoc, _ := body["error"].(map[string]any)
	if got != http.StatusBadGateway || errDoc["error_type"] != "sandbox_runtime_error" {
		t.Errorf("the route that needs the runtime = %d %v, want 502 sandbox_runtime_error", got, body)
	}
	if svc.Client().Reachable() {
		t.Error("the runtime client reports the gateway reachable")
	}

	before := gw.calls.Load()
	if before == 0 {
		t.Fatal("the gateway counted no call, not even the start-up check's and the route's")
	}
	got, body = getAuthorized(t, h.URL("/api/v1/sandboxes"), "Bearer "+h.Token)
	if items, _ := body["items"].([]any); got != http.StatusOK || len(items) != 1 || !reflect.DeepEqual(items[0], src[0].Object) {
		t.Errorf("GET /api/v1/sandboxes = %d %v, want 200 with the stored sandbox", got, body)
	}
	if got, body := getAuthorized(t, h.URL("/api/v1/sandbox-profiles"), "Bearer "+key); got != http.StatusOK {
		t.Errorf("GET /api/v1/sandbox-profiles = %d %v, want 200", got, body)
	}
	if n := gw.calls.Load() - before; n != 0 {
		t.Errorf("the two lists called the gateway %d times, want 0", n)
	}
	if got := status(t, h.URL("/healthz")); got != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", got)
	}
}

// closedPort returns a loopback TCP port that was just closed, which
// nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// oneSandbox is a sandbox source holding its items, which it gives on every
// call, whatever the filter.
type oneSandbox []sandbox.Item

func (s oneSandbox) List(context.Context, sandbox.Filter, *sandbox.Position, int) ([]sandbox.Item, error) {
	return s, nil
}

// countingGateway counts every reach of the SDK client it wraps for a
// sub-client: no call to the gateway goes past it uncounted.
type countingGateway struct {
	v1.ClientInterface
	calls atomic.Int64
}

func (g *countingGateway) count() v1.ClientInterface { g.calls.Add(1); return g.ClientInterface }

func (g *countingGateway) Sandboxes() v1.SandboxInterface { return g.count().Sandboxes() }
func (g *countingGateway) SandboxTemplates() v1.SandboxTemplateInterface {
	return g.count().SandboxTemplates()
}
func (g *countingGateway) CreateSandboxFromTemplate(ctx context.Context, workspace, name, template string, spec *v1.SandboxSpec, opts ...v1.CreateOption) (*v1.Sandbox, error) {
	return g.count().CreateSandboxFromTemplate(ctx, workspace, name, template, spec, opts...)
}
func (g *countingGateway) SandboxSuspendResume() v1.SandboxSuspendResumeInterface {
	return g.count().SandboxSuspendResume()
}
func (g *countingGateway) Providers() v1.ProviderInterface   { return g.count().Providers() }
func (g *countingGateway) Services() v1.ServiceInterface     { return g.count().Services() }
func (g *countingGateway) Exec() v1.ExecInterface            { return g.count().Exec() }
func (g *countingGateway) Files() v1.FileInterface           { return g.count().Files() }
func (g *countingGateway) Health() v1.HealthInterface        { return g.count().Health() }
func (g *countingGateway) SSH() v1.SSHInterface              { return g.count().SSH() }
func (g *countingGateway) TCP() v1.TCPInterface              { return g.count().TCP() }
func (g *countingGateway) Config() v1.ConfigInterface        { return g.count().Config() }
func (g *countingGateway) Policy() v1.PolicyInterface        { return g.count().Policy() }
func (g *countingGateway) Workspaces() v1.WorkspaceInterface { return g.count().Workspaces() }
