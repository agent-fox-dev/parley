package sandbox_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
)

// The content of the files every configuration dir of loadSandbox holds. The
// values are what a failing load must never repeat.
const (
	tokenContent  = "tok-SECRET-1"
	secretContent = "oidc-SECRET-2"
	// policyContent is the content of policy.yaml.
	policyContent = "network_policies: {}\n"
)

// sandboxFile describes a config.toml around a valid [sandbox] section:
// gateway_url https://gw.test:8443, token_file "token", [server].external_url
// set and one profile {name 'agent', ttl '4h'}.
type sandboxFile struct {
	// set changes the section's keys: each value is a TOML literal written
	// as it is, and "" removes the key.
	set map[string]string
	// profile is TOML appended inside the profile's table.
	profile string
	// noProfile leaves the profile 'agent' out, profile with it, so that the
	// section holds only the profiles of tail.
	noProfile bool
	// tail is TOML appended after the profile, such as credential tables.
	tail string
	// noExternalURL leaves [server].external_url out.
	noExternalURL bool
	// noSection leaves the whole [sandbox] section out.
	noSection bool
}

func (f sandboxFile) String() string {
	var b strings.Builder
	if !f.noExternalURL {
		b.WriteString("[server]\nexternal_url = \"https://hub.test\"\n")
	}
	if f.noSection {
		return b.String()
	}
	keys := map[string]string{
		"gateway_url": `"https://gw.test:8443"`,
		"token_file":  `"token"`,
	}
	for k, v := range f.set {
		if v == "" {
			delete(keys, k)
			continue
		}
		keys[k] = v
	}
	b.WriteString("[sandbox]\n")
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		b.WriteString(k + " = " + keys[k] + "\n")
	}
	if !f.noProfile {
		b.WriteString("[[sandbox.profiles]]\nname = \"agent\"\nttl = \"4h\"\n")
		b.WriteString(f.profile)
	}
	b.WriteString(f.tail)
	return b.String()
}

// loadSandbox writes f as config.toml into a new config dir, next to the
// files token (tokenContent), secret (secretContent), cert, key, ca and
// policy.yaml (each with some content) and empty (white space only), loads it
// with the sandbox
// section registered and the environment env, and returns the sandbox
// settings or the error of the load or of the section's validation.
func loadSandbox(t *testing.T, f sandboxFile, env map[string]string) (*sandbox.Settings, error) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"config.toml": f.String(),
		"token":       tokenContent + "\n",
		"secret":      secretContent,
		"cert":        "cert",
		"key":         "key",
		"ca":          "ca",
		"policy.yaml": policyContent,
		"empty":       " \n\t",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := config.NewRegistry()
	if err := sandbox.Register(reg); err != nil {
		t.Fatal(err)
	}
	cfg, err := reg.Load(config.MapEnv(env), dir)
	if err != nil {
		return nil, err
	}
	return sandbox.FromConfig(cfg)
}

// profileTable is a [[sandbox.profiles]] table named name, with body as its
// other keys.
func profileTable(name, body string) string {
	return "[[sandbox.profiles]]\nname = \"" + name + "\"\n" + body
}

// credentialTable is a [[sandbox.credentials]] table named name for the
// provider profile anthropic and the secret MODEL_KEY.
func credentialTable(name string) string {
	return "[[sandbox.credentials]]\nname = \"" + name + "\"\nprovider_profile = \"anthropic\"\nsecret = \"MODEL_KEY\"\n"
}

// wantErrorNaming fails the test unless err is not nil and its text holds
// every name.
func wantErrorNaming(t *testing.T, s *sandbox.Settings, err error, names ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("load = %+v, nil; want an error naming %q", s, names)
	}
	if s != nil {
		t.Errorf("load returned settings %+v with its error", s)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %q", err, name)
		}
	}
}

// checkFile is the configuration of the start-up check's tests: the
// credential model for the provider profile anthropic, the profile agent with
// the policy file policy.yaml and the credential model, and the profile
// plain on the built-in policy.
func checkFile() sandboxFile {
	return sandboxFile{
		profile: "policy = \"policy.yaml\"\ncredentials = [\"model\"]\n",
		tail:    profileTable("plain", "ttl = \"4h\"\n") + credentialTable("model"),
	}
}

// newRuntime returns a runtime that reports a healthy gateway of a release
// inside the supported range and holds, in the workspace parley, the
// provider profile anthropic, which declares the variable ANTHROPIC_API_KEY.
func newRuntime(t *testing.T) *runtimetest.Fake {
	t.Helper()
	rt := runtimetest.New(t)
	rt.SetRelease(runtime.Release{Healthy: true, Version: sandbox.LowestSupportedRelease})
	rt.AddProviderProfile("parley", runtime.ProviderProfile{ID: "anthropic", Variables: []string{"ANTHROPIC_API_KEY"}})
	return rt
}

// startCheck loads f (see loadSandbox), builds a client of rt in the
// section's workspace and runs the start-up check through it with opts,
// whose ExternalURL is that of f when it is empty.
func startCheck(t *testing.T, f sandboxFile, rt *runtimetest.Fake, opts sandbox.CheckOptions) (*sandbox.Settings, runtime.Client, error) {
	t.Helper()
	s, err := loadSandbox(t, f, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if opts.ExternalURL == "" {
		opts.ExternalURL = "https://hub.test"
	}
	client := rt.Client(s.OpenShellWorkspace, nil)
	_, err = sandbox.RunStartCheck(context.Background(), s, client, opts)
	return s, client, err
}

// ops returns the operations of calls, in order.
func ops(calls []runtimetest.Call) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Op
	}
	return out
}
