package sandbox_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell/openshelltest"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
)

func TestRunStartCheck_CallsTheRuntimeOnceEachInOrderAndMarksItReachable(t *testing.T) {
	t.Parallel()
	rt := newRuntime(t)
	rt.SetRelease(runtime.Release{Healthy: true, Version: "0.1.8"})
	rt.SetPolicyValidation([]byte(policyContent), runtime.PolicyValidation{Hosts: []string{"pypi.org", "api.anthropic.com", "pypi.org"}})
	f := checkFile()
	// Two credentials of one provider profile: the profile is read once.
	f.tail += credentialTable("model2")
	s, err := loadSandbox(t, f, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	client := rt.Client(s.OpenShellWorkspace, nil)
	rec := &runtimetest.Metrics{}

	release, err := sandbox.RunStartCheck(context.Background(), s, client, sandbox.CheckOptions{ExternalURL: "https://hub.test", Recorder: rec})
	if err != nil {
		t.Fatalf("RunStartCheck: %v", err)
	}

	want := []runtimetest.Call{
		{Op: runtime.OpRelease, Workspace: "parley"},
		{Op: runtime.OpEnsureWorkspace, Workspace: "parley"},
		{Op: runtime.OpInstallProviderProfile, Workspace: "parley", Target: "parley-hub"},
		{Op: runtime.OpGetProviderProfile, Workspace: "parley", Target: "anthropic"},
		{Op: runtime.OpValidatePolicy, Workspace: "parley"},
	}
	if got := rt.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %+v, want %+v", got, want)
	}
	if release.Version != "0.1.8" {
		t.Errorf("release = %+v, want version 0.1.8", release)
	}
	if !client.Reachable() {
		t.Error("Reachable() = false after the check succeeded")
	}
	if got, want := rec.Calls(), []runtimetest.Metric{{Method: "set", Name: runtime.MetricUp, Value: 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("metrics = %+v, want %+v", got, want)
	}
	agent, _ := s.Profile("agent")
	if want := []string{"api.anthropic.com", "pypi.org"}; !slices.Equal(agent.PolicyHosts, want) {
		t.Errorf("PolicyHosts of agent = %q, want %q", agent.PolicyHosts, want)
	}
	if plain, _ := s.Profile("plain"); plain.PolicyHosts != nil {
		t.Errorf("PolicyHosts of plain = %q, want none for the built-in policy", plain.PolicyHosts)
	}
	hubProfile, err := client.GetProviderProfile(context.Background(), "parley-hub")
	if err != nil {
		t.Fatalf("read parley-hub: %v", err)
	}
	wantProfile := runtime.ProviderProfile{
		ID:        "parley-hub",
		Variables: []string{"PARLEY_API_KEY"},
		Endpoints: []runtime.Endpoint{{Scheme: "https", Host: "hub.test", Port: 443, Paths: []string{"/api/v1/", "/git/"}}},
	}
	if !reflect.DeepEqual(hubProfile, wantProfile) {
		t.Errorf("parley-hub = %+v, want %+v", hubProfile, wantProfile)
	}
}

func TestRunStartCheck_AuthorizesTheHubCredentialForTheExternalURLsSchemeHostAndPort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		externalURL string
		want        runtime.Endpoint
	}{
		{"https://hub.test", runtime.Endpoint{Scheme: "https", Host: "hub.test", Port: 443}},
		{"http://localhost:8080/parley", runtime.Endpoint{Scheme: "http", Host: "localhost", Port: 8080}},
		{"http://hub.test", runtime.Endpoint{Scheme: "http", Host: "hub.test", Port: 80}},
		{"https://[::1]:9443/", runtime.Endpoint{Scheme: "https", Host: "::1", Port: 9443}},
	} {
		t.Run(tc.externalURL, func(t *testing.T) {
			t.Parallel()
			rt := newRuntime(t)
			_, client, err := startCheck(t, sandboxFile{}, rt, sandbox.CheckOptions{ExternalURL: tc.externalURL})
			if err != nil {
				t.Fatalf("RunStartCheck: %v", err)
			}
			p, err := client.GetProviderProfile(context.Background(), "parley-hub")
			if err != nil {
				t.Fatalf("read parley-hub: %v", err)
			}
			tc.want.Paths = []string{"/api/v1/", "/git/"}
			if !reflect.DeepEqual(p.Endpoints, []runtime.Endpoint{tc.want}) {
				t.Errorf("endpoints = %+v, want [%+v]", p.Endpoints, tc.want)
			}
		})
	}
}

func TestRunStartCheck_LeavesOneHubProfileAndOneWorkspaceWhenRunAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gw := openshelltest.New(fake.WithGatewayInfo(&v1.GatewayInfo{Version: "0.1.8"}))
	gw.AddProfile("parley", v1.ProviderProfile{ID: "anthropic", Credentials: []v1.ProfileCredential{{Name: "api_key", EnvVars: []string{"ANTHROPIC_API_KEY"}}}})
	// The SDK has no call that validates a policy file, so every profile
	// here is on the built-in policy.
	s, err := loadSandbox(t, sandboxFile{tail: credentialTable("model")}, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	for run := 1; run <= 2; run++ {
		client := openshell.NewWithClient(gw, s.OpenShellWorkspace, openshell.Deps{})
		if _, err := sandbox.RunStartCheck(ctx, s, client, sandbox.CheckOptions{ExternalURL: "https://hub.test"}); err != nil {
			t.Fatalf("run %d: RunStartCheck: %v", run, err)
		}
	}

	workspaces, err := gw.Fake().Workspaces().ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 1 || workspaces[0].Name != "parley" {
		t.Errorf("workspaces = %+v, want parley alone", workspaces)
	}
	profiles, err := gw.Providers().Profiles().ListAll(ctx, "parley")
	if err != nil {
		t.Fatal(err)
	}
	hub := 0
	for _, p := range profiles {
		if p.ID == "parley-hub" {
			hub++
		}
	}
	if hub != 1 {
		t.Errorf("the workspace holds %d provider profiles parley-hub, want 1", hub)
	}
}

func TestRunStartCheck_StopsAtAGatewayItCannotUseNamingTheGatewayURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		arrange   func(rt *runtimetest.Fake)
		wantCalls []string
	}{
		{"gateway that cannot be reached", func(rt *runtimetest.Fake) { rt.SetUnreachable(true) }, []string{runtime.OpRelease}},
		{"gateway refusing the credential", func(rt *runtimetest.Fake) {
			rt.FailOperation(runtime.OpRelease, runtime.ErrUnavailable, "unauthenticated: bad token "+tokenContent)
		}, []string{runtime.OpRelease}},
		{"gateway that is not healthy", func(rt *runtimetest.Fake) {
			rt.SetRelease(runtime.Release{Healthy: false, Version: "0.1.8"})
		}, []string{runtime.OpRelease}},
		{"gateway lost after the release", func(rt *runtimetest.Fake) {
			rt.FailOperation(runtime.OpEnsureWorkspace, runtime.ErrUnavailable, "connection reset")
		}, []string{runtime.OpRelease, runtime.OpEnsureWorkspace}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRuntime(t)
			tc.arrange(rt)
			_, _, err := startCheck(t, checkFile(), rt, sandbox.CheckOptions{})
			if err == nil {
				t.Fatal("RunStartCheck succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "sandbox.gateway_url") {
				t.Errorf("error %q does not name sandbox.gateway_url", err)
			}
			if strings.Contains(err.Error(), tokenContent) {
				t.Errorf("error %q holds the gateway token", err)
			}
			if got := ops(rt.Calls()); !slices.Equal(got, tc.wantCalls) {
				t.Errorf("calls = %q, want %q", got, tc.wantCalls)
			}
		})
	}
}

func TestRunStartCheck_RefusesAReleaseOutsideTheSupportedRange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		release string
		accept  bool
	}{
		{"1.1.9", false},
		{"1.2.0", true},
		{"1.9.3", true},
		{"2.0.0", false},
		{"v1.4.0", true},
		{"nightly", false},
		{"", false},
	} {
		t.Run("release "+tc.release, func(t *testing.T) {
			t.Parallel()
			rt := newRuntime(t)
			rt.SetRelease(runtime.Release{Healthy: true, Version: tc.release})
			_, _, err := startCheck(t, checkFile(), rt, sandbox.CheckOptions{
				Releases: sandbox.ReleaseRange{Lowest: "1.2.0", FirstUnsupported: "2.0.0"},
			})
			if tc.accept {
				if err != nil {
					t.Fatalf("RunStartCheck: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RunStartCheck succeeded, want an error")
			}
			for _, want := range []string{tc.release, ">=1.2.0 <2.0.0"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not hold %q", err, want)
				}
			}
			if got := ops(rt.Calls()); !slices.Equal(got, []string{runtime.OpRelease}) {
				t.Errorf("calls = %q, want the release read alone", got)
			}
		})
	}
}

func TestRunStartCheck_RefusesACredentialWhoseProviderProfileTheRuntimeLacks(t *testing.T) {
	t.Parallel()
	rt := newRuntime(t)
	f := checkFile()
	f.tail = profileTable("plain", "ttl = \"4h\"\n") +
		"[[sandbox.credentials]]\nname = \"model\"\nprovider_profile = \"missing\"\nsecret = \"MODEL_KEY\"\n"

	_, _, err := startCheck(t, f, rt, sandbox.CheckOptions{})

	if err == nil {
		t.Fatal("RunStartCheck succeeded, want an error")
	}
	for _, want := range []string{"sandbox.credentials[model].provider_profile", `"missing"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if slices.Contains(ops(rt.Calls()), runtime.OpValidatePolicy) {
		t.Errorf("calls = %q: a policy file was sent after the failure", ops(rt.Calls()))
	}
}

func TestRunStartCheck_RefusesAPolicyFileTheRuntimeDoesNotAccept(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		file       sandboxFile
		arrange    func(rt *runtimetest.Fake)
		wantErr    bool
		wantReason string
	}{
		{"rejected policy", checkFile(), func(rt *runtimetest.Fake) {
			rt.RejectPolicies(func(runtime.Policy) bool { return true }, "unknown field egress (token "+tokenContent+")")
		}, true, "unknown field egress"},
		{"runtime that cannot validate a policy", checkFile(), func(rt *runtimetest.Fake) {
			rt.FailOperation(runtime.OpValidatePolicy, runtime.ErrUnsupported, "")
		}, true, ""},
		{"built-in policies on a runtime that cannot validate one", sandboxFile{tail: profileTable("plain", "ttl = \"4h\"\n")}, func(rt *runtimetest.Fake) {
			rt.FailOperation(runtime.OpValidatePolicy, runtime.ErrUnsupported, "")
		}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRuntime(t)
			tc.arrange(rt)
			_, _, err := startCheck(t, tc.file, rt, sandbox.CheckOptions{})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("RunStartCheck: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RunStartCheck succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "sandbox.profiles[agent].policy") {
				t.Errorf("error %q does not name sandbox.profiles[agent].policy", err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("error %q does not carry the runtime's reason %q", err, tc.wantReason)
			}
			if strings.Contains(err.Error(), tokenContent) {
				t.Errorf("error %q holds the gateway token", err)
			}
		})
	}
}

func TestRunStartCheck_RefusesAnExposedSecretThatIsAnAttachedCredentialsVariable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		file    sandboxFile
		wantKey string // "" for a configuration the check accepts
	}{
		{"exposed and attached", sandboxFile{
			profile: "credentials = [\"model\"]\nexpose_secrets = [\"ANTHROPIC_API_KEY\"]\n",
			tail:    credentialTable("model"),
		}, "sandbox.profiles[agent].expose_secrets"},
		{"optionally exposed and optionally attached", sandboxFile{
			profile: "optional_credentials = [\"model\"]\noptional_expose_secrets = [\"ANTHROPIC_API_KEY\"]\n",
			tail:    credentialTable("model"),
		}, "sandbox.profiles[agent].optional_expose_secrets"},
		{"optionally exposed and attached", sandboxFile{
			profile: "credentials = [\"model\"]\noptional_expose_secrets = [\"ANTHROPIC_API_KEY\"]\n",
			tail:    credentialTable("model"),
		}, "sandbox.profiles[agent].optional_expose_secrets"},
		{"exposed by a profile without the credential", sandboxFile{
			profile: "credentials = [\"model\"]\n",
			tail:    profileTable("other", "ttl = \"4h\"\nexpose_secrets = [\"ANTHROPIC_API_KEY\"]\n") + credentialTable("model"),
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRuntime(t)
			_, _, err := startCheck(t, tc.file, rt, sandbox.CheckOptions{})
			if tc.wantKey == "" {
				if err != nil {
					t.Fatalf("RunStartCheck: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RunStartCheck succeeded, want an error")
			}
			for _, want := range []string{tc.wantKey, "ANTHROPIC_API_KEY"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not hold %q", err, want)
				}
			}
		})
	}
}

// deadlineClient records the deadline of every call of the start-up check,
// and with block makes EnsureWorkspace wait until its context ends.
type deadlineClient struct {
	runtime.Client
	block bool

	mu        sync.Mutex
	deadlines map[string]time.Time
}

func (c *deadlineClient) note(ctx context.Context, op string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := ctx.Deadline()
	if !ok {
		d = time.Time{}
	}
	c.deadlines[op] = d
}

func (c *deadlineClient) Release(ctx context.Context) (runtime.Release, error) {
	c.note(ctx, runtime.OpRelease)
	return c.Client.Release(ctx)
}

func (c *deadlineClient) EnsureWorkspace(ctx context.Context) error {
	c.note(ctx, runtime.OpEnsureWorkspace)
	if c.block {
		<-ctx.Done()
		return runtime.NewError(runtime.ErrUnavailable, runtime.OpEnsureWorkspace, ctx.Err().Error())
	}
	return c.Client.EnsureWorkspace(ctx)
}

func (c *deadlineClient) InstallProviderProfile(ctx context.Context, p runtime.ProviderProfile) error {
	c.note(ctx, runtime.OpInstallProviderProfile)
	return c.Client.InstallProviderProfile(ctx, p)
}

func (c *deadlineClient) GetProviderProfile(ctx context.Context, id string) (runtime.ProviderProfile, error) {
	c.note(ctx, runtime.OpGetProviderProfile)
	return c.Client.GetProviderProfile(ctx, id)
}

func (c *deadlineClient) ValidatePolicy(ctx context.Context, p runtime.Policy) (runtime.PolicyValidation, error) {
	c.note(ctx, runtime.OpValidatePolicy)
	return c.Client.ValidatePolicy(ctx, p)
}

func TestRunStartCheck_GivesEveryCallADeadlineWithinThirtySecondsOfItsStart(t *testing.T) {
	t.Parallel()
	rt := newRuntime(t)
	s, err := loadSandbox(t, checkFile(), nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	client := &deadlineClient{Client: rt.Client(s.OpenShellWorkspace, nil), deadlines: map[string]time.Time{}}
	began := time.Now()

	if _, err := sandbox.RunStartCheck(context.Background(), s, client, sandbox.CheckOptions{
		ExternalURL: "https://hub.test", Now: func() time.Time { return began },
	}); err != nil {
		t.Fatalf("RunStartCheck: %v", err)
	}

	for _, op := range []string{runtime.OpRelease, runtime.OpEnsureWorkspace, runtime.OpInstallProviderProfile, runtime.OpGetProviderProfile, runtime.OpValidatePolicy} {
		d, called := client.deadlines[op]
		switch {
		case !called:
			t.Errorf("%s was not called", op)
		case d.IsZero():
			t.Errorf("%s was called without a deadline", op)
		case d.Sub(began) > 30*time.Second || !d.After(began):
			t.Errorf("%s was called with a deadline %v after the check began, want at most 30s", op, d.Sub(began))
		}
	}
}

func TestRunStartCheck_GivesUpOnARuntimeThatDoesNotAnswerInTime(t *testing.T) {
	t.Parallel()
	rt := newRuntime(t)
	s, err := loadSandbox(t, checkFile(), nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	client := &deadlineClient{Client: rt.Client(s.OpenShellWorkspace, nil), block: true, deadlines: map[string]time.Time{}}
	began := time.Now()

	_, err = sandbox.RunStartCheck(context.Background(), s, client, sandbox.CheckOptions{
		ExternalURL: "https://hub.test", Budget: 50 * time.Millisecond,
	})

	if err == nil {
		t.Fatal("RunStartCheck succeeded, want an error")
	}
	for _, want := range []string{"sandbox.gateway_url", "did not answer in time"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Errorf("the check took %v with a budget of 50ms", elapsed)
	}
}
