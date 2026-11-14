package hub_test

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
)

// gatewayToken is the content of the token_file of sandboxSection.
const gatewayToken = "gw-tok-SECRET"

// sandboxSection is a valid [sandbox] section: a gateway reached with the
// token in the file gateway-token of the config dir, the credential model of
// the provider profile anthropic, and the profile agent with the policy
// file policy.yaml and that credential.
const sandboxSection = "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" +
	"[[sandbox.profiles]]\nname = \"agent\"\nttl = \"4h\"\npolicy = \"policy.yaml\"\ncredentials = [\"model\"]\n" +
	"[[sandbox.credentials]]\nname = \"model\"\nprovider_profile = \"anthropic\"\nsecret = \"MODEL_KEY\"\n"

// sandboxFiles are the files sandboxSection names.
var sandboxFiles = map[string]string{"gateway-token": gatewayToken + "\n", "policy.yaml": "network_policies: {}\n"}

// externalURL is the [server] line sandboxes need.
const externalURL = "external_url = \"https://hub.test\"\n"

// newRuntime returns a runtime that reports a release inside the supported
// range and holds, in the workspace parley, the provider profile anthropic.
func newRuntime(t *testing.T) *runtimetest.Fake {
	t.Helper()
	rt := runtimetest.New(t)
	rt.SetRelease(runtime.Release{Healthy: true, Version: "0.1.4"})
	rt.AddProviderProfile("parley", runtime.ProviderProfile{ID: "anthropic", Variables: []string{"ANTHROPIC_API_KEY"}})
	return rt
}

// runtimeOf is a runtime factory that counts its calls and builds a client
// of rt with the deps it is given.
func runtimeOf(rt *runtimetest.Fake, calls *atomic.Int32, wrap func(runtime.Client) runtime.Client) sandbox.RuntimeFactory {
	return func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
		calls.Add(1)
		c := rt.Client(s.OpenShellWorkspace, deps.Recorder)
		if wrap != nil {
			c = wrap(c)
		}
		return c, nil
	}
}

func TestRun_StartsWithoutSandboxesWithoutBuildingARuntimeClient(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	rt := newRuntime(t)
	h := hubtest.Start(t, func(c *hub.Composition) {
		c.SetSandboxRuntime(runtimeOf(rt, &calls, nil))
	})

	var doc map[string]any
	if status := getJSON(t, h.URL("/version"), &doc); status != 200 {
		t.Fatalf("GET /version = %d, want 200", status)
	}
	if _, ok := doc["sandbox_runtime"]; ok {
		t.Errorf("GET /version = %v, want no sandbox_runtime without sandboxes", doc)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the runtime factory was called %d times, want never", n)
	}
	if got := rt.Calls(); len(got) != 0 {
		t.Errorf("the runtime received %+v, want no call", got)
	}
}

// lastCallClock records when the start-up check's last call, the policy
// validation, returned; it holds that call a moment so that a listening
// line written meanwhile would show.
type lastCallClock struct {
	runtime.Client
	mu       sync.Mutex
	returned time.Time
}

func (c *lastCallClock) ValidatePolicy(ctx context.Context, p runtime.Policy) (runtime.PolicyValidation, error) {
	time.Sleep(50 * time.Millisecond)
	v, err := c.Client.ValidatePolicy(ctx, p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.returned = time.Now()
	return v, err
}

func TestRun_ChecksTheRuntimeOnceBeforeListening(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	rt := newRuntime(t)
	metrics := &runtimetest.Metrics{}
	last := &lastCallClock{}
	var svc *sandbox.Service
	h := hubtest.StartWith(t, hubtest.Config{Server: externalURL, TOML: sandboxSection, Files: sandboxFiles, Compose: func(c *hub.Composition) {
		svc = c.Sandbox()
		c.SetSandboxRecorder(metrics)
		c.SetSandboxRuntime(runtimeOf(rt, &calls, func(c runtime.Client) runtime.Client {
			last.Client = c
			return last
		}))
	}})

	if n := calls.Load(); n != 1 {
		t.Errorf("the runtime factory was called %d times, want once", n)
	}
	want := []string{runtime.OpRelease, runtime.OpEnsureWorkspace, runtime.OpInstallProviderProfile, runtime.OpGetProviderProfile, runtime.OpValidatePolicy}
	var got []string
	for _, c := range rt.Calls() {
		got = append(got, c.Op)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
	listened, err := time.Parse(time.RFC3339Nano, h.Listening["time"].(string))
	if err != nil {
		t.Fatalf("listening line's time: %v", err)
	}
	last.mu.Lock()
	returned := last.returned
	last.mu.Unlock()
	if returned.IsZero() || listened.Before(returned) {
		t.Errorf("the hub listened at %v, before the check's last call returned at %v", listened, returned)
	}
	if svc.Client() == nil || !svc.Client().Reachable() {
		t.Fatal("the composition's sandbox service has no reachable runtime client after the start")
	}
	if got, want := metrics.Calls(), []runtimetest.Metric{{Method: "set", Name: runtime.MetricUp, Value: 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("metrics after the start = %+v, want %+v", got, want)
	}
	// The recorder is the client's too: losing the gateway reports 0.
	rt.SetUnreachable(true)
	_, _ = svc.Client().Release(context.Background())
	if got := metrics.Calls(); len(got) != 2 || got[1].Value != 0 {
		t.Errorf("metrics after losing the gateway = %+v, want %s set to 0", got, runtime.MetricUp)
	}
}

func TestRun_ReportsTheConnectedRuntimeInTheVersionDocument(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	rt := newRuntime(t)
	h := hubtest.StartWith(t, hubtest.Config{Server: externalURL, TOML: sandboxSection, Files: sandboxFiles, Compose: func(c *hub.Composition) {
		c.SetSandboxRuntime(runtimeOf(rt, &calls, nil))
	}})
	want := map[string]any{"supported": ">=0.1.0 <0.2.0", "connected": "0.1.4"}

	var doc map[string]any
	if status := getJSON(t, h.URL("/version"), &doc); status != 200 {
		t.Fatalf("GET /version = %d, want 200", status)
	}
	if !reflect.DeepEqual(doc["sandbox_runtime"], want) {
		t.Errorf("sandbox_runtime = %v, want %v", doc["sandbox_runtime"], want)
	}
	if v, _ := doc["version"].(string); v == "" {
		t.Errorf("GET /version = %v, want the build's version beside sandbox_runtime", doc)
	}

	rt.SetUnreachable(true)
	doc = nil
	getJSON(t, h.URL("/version"), &doc)
	if !reflect.DeepEqual(doc["sandbox_runtime"], want) {
		t.Errorf("sandbox_runtime while the runtime is unreachable = %v, want %v", doc["sandbox_runtime"], want)
	}
}
