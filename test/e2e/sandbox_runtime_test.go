package e2e

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell/openshelltest"
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
// OpenShell implementation over gw.
func startSandboxHub(t *testing.T, cfg hubtest.Config, gw *openshelltest.Gateway) *hubtest.Hub {
	t.Helper()
	cfg.Compose = func(c *hub.Composition) {
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			return openshell.New(s, openshell.Deps{
				Scrubber: deps.Scrubber,
				Recorder: deps.Recorder,
				Dial:     func(v1.Config) (v1.ClientInterface, error) { return gw, nil },
			})
		})
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
