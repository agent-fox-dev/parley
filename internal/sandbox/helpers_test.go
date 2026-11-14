package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
	"github.com/agent-fox-dev/parley/internal/server"
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

// gatewayToken is the content of the token file of a hub with sandboxes on.
const gatewayToken = "gw-tok-SECRET"

// sandboxesOn is what startProbeHub adds to a hub's configuration to turn
// sandboxes on: [server].external_url and a [sandbox] section with the
// token file gateway-token and the profile agent.
var sandboxesOn = hubtest.Config{
	Server: "external_url = \"https://hub.test\"\n",
	TOML: "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" +
		"[[sandbox.profiles]]\nname = \"agent\"\nttl = \"4h\"\n",
	Files: map[string]string{"gateway-token": gatewayToken + "\n"},
}

// probeBodyLimit is the body limit of the probe write route.
const probeBodyLimit = 1024

// probeHub is a real hub with the sandbox probe routes, each declared with
// the sandbox service's declarations, and the workspace ws owned by owner in
// the org team, of which member is a member and outsider is not. A probe
// answers 200 and counts its calls.
type probeHub struct {
	*hubtest.Hub
	// rt is the runtime of a hub with sandboxes on; nil with sandboxes off.
	rt    *runtimetest.Fake
	calls atomic.Int64
	// owner, member and outsider are user ids.
	owner, member, outsider string
}

// startProbeHub starts a probe hub, with sandboxes on over a fake runtime
// when on is true. Its probe routes, beside the hub's own GET
// /sandbox-profiles and GET /sandboxes, are:
//
//   - GET /workspaces/:slug/sandboxes/probe-read, of kind read;
//   - POST /workspaces/:slug/sandboxes/probe-write, of kind write, with the
//     body limit probeBodyLimit;
//   - POST /workspaces/:slug/sandboxes/probe-create, of kind write and
//     needing a user, which answers 400 to a body without a name;
//   - GET /workspaces/:slug/sandboxes/probe-runtime, of kind read, which
//     returns the error of GetSandbox;
//   - POST /workspaces/:slug/sandboxes/probe-exec, of kind write, which
//     stands for a sandbox whose hub credential has the scope git:write and
//     no paths limit: it asks whether the caller covers it, then counts,
//     answers 400 to a body without a command and lists the sandboxes.
func startProbeHub(t *testing.T, on bool) *probeHub {
	t.Helper()
	h := &probeHub{}
	var cfg hubtest.Config
	if on {
		cfg = sandboxesOn
		h.rt = runtimetest.New(t)
	}
	cfg.Compose = func(c *hub.Composition) {
		svc := c.Sandbox()
		if on {
			c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
				return h.rt.Client(s.OpenShellWorkspace, deps.Recorder), nil
			})
		}
		c.AddRoute(svc.WorkspaceRoute(sandbox.KindRead, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/sandboxes/probe-read", Handler: h.probe}))
		c.AddRoute(svc.WorkspaceRoute(sandbox.KindWrite, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/sandboxes/probe-write", MaxBodySize: probeBodyLimit, Handler: h.probe}))
		c.AddRoute(svc.WorkspaceRoute(sandbox.KindWrite, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/sandboxes/probe-create", Handler: func(w http.ResponseWriter, r *http.Request) error {
			h.calls.Add(1)
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
				return server.InvalidRequest("name is required")
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
			return nil
		}}, sandbox.NeedsUser()))
		c.AddRoute(svc.WorkspaceRoute(sandbox.KindRead, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/sandboxes/probe-runtime", Handler: func(w http.ResponseWriter, r *http.Request) error {
			h.calls.Add(1)
			if _, err := svc.Client().GetSandbox(r.Context(), "probe"); err != nil {
				return err
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
			return nil
		}}))
		c.AddRoute(svc.WorkspaceRoute(sandbox.KindWrite, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/sandboxes/probe-exec", Handler: func(w http.ResponseWriter, r *http.Request) error {
			if err := sandbox.RequireCovers(r, &sandbox.HubAccess{Scopes: []string{"git:write"}}); err != nil {
				return err
			}
			h.calls.Add(1)
			var body struct {
				Command []string `json:"command"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Command) == 0 {
				return server.InvalidRequest("command is required")
			}
			if _, err := svc.Client().ListSandboxes(r.Context()); err != nil {
				return err
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
			return nil
		}}))
	}
	h.Hub = hubtest.StartWith(t, cfg)
	h.owner, h.member, h.outsider = h.CreateUser(t, "owner"), h.CreateUser(t, "member"), h.CreateUser(t, "outsider")
	org := h.CreateOrg(t, "team")
	h.AddMember(t, org, h.member)
	h.AddWorkspace(t, access.Record{Slug: "ws", OwnerID: h.owner, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady})
	return h
}

func (h *probeHub) probe(w http.ResponseWriter, _ *http.Request) error {
	h.calls.Add(1)
	server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

// pat stores a PAT of the user userID with permissions and returns it.
func (h *probeHub) pat(t *testing.T, userID string, permissions ...string) string {
	t.Helper()
	return h.IssuePAT(t, identity.PATSpec{UserID: userID, Permissions: permissions})
}

// answer is the answer to a request: its status, its body and the members
// of its error envelope, if it has one.
type answer struct {
	status int
	body   []byte
	err    struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		ErrorType string `json:"error_type"`
	}
}

// call sends method path, under /api/v1, with cred as the bearer credential
// ("" for none) and body: nil for none, []byte as it is, anything else as
// JSON; a body is sent as application/json.
func (h *probeHub) call(t *testing.T, method, path, cred string, body any) answer {
	t.Helper()
	var data io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		data = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.URL("/api/v1"+path), data)
	if err != nil {
		t.Fatal(err)
	}
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	a := answer{status: res.StatusCode}
	if a.body, err = io.ReadAll(res.Body); err != nil {
		t.Fatalf("%s %s: read the body: %v", method, path, err)
	}
	var env struct {
		Error *json.RawMessage `json:"error"`
	}
	if json.Unmarshal(a.body, &env) == nil && env.Error != nil {
		_ = json.Unmarshal(*env.Error, &a.err)
	}
	return a
}

// profilesFile is the configuration of the profile list's tests: the
// profile plain, on the built-in policy with nothing set but a ttl of 24h
// and the idle stop turned off, and
// the profile agent, with every key the list shows or hides set and the
// idle_timeout left to the section's default (30m), written in that order.
var profilesFile = hubtest.Config{
	Server: "external_url = \"https://hub.test\"\n",
	TOML: "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" +
		"[[sandbox.profiles]]\nname = \"plain\"\nttl = \"24h\"\nidle_timeout = \"0s\"\n" +
		"[[sandbox.profiles]]\nname = \"agent\"\ndefault = true\ndescription = \"an agent\"\nimage = \"agent:1\"\ncpu = \"2\"\nmemory = \"4Gi\"\ngpu = 1\n" +
		"ttl = \"4h\"\npolicy = \"policy.yaml\"\ncredentials = [\"model\"]\noptional_credentials = [\"search\"]\n" +
		"expose_secrets = [\"Z_TOKEN\"]\noptional_expose_secrets = [\"A_TOKEN\"]\nhub_access = \"none\"\ncheckpoint = false\nallow_proposals = false\n" +
		"env = { B = \"vb\", A = \"va\" }\n" +
		credentialTable("model") + credentialTable("search"),
	Files: map[string]string{"gateway-token": gatewayToken + "\n", "policy.yaml": policyContent},
}

// startProfilesHub starts a hub with sandboxes on over a fake runtime with
// profilesFile, the runtime reporting the hosts b.test and a.test for the
// policy file, and returns it with the absolute path of agent's policy file.
func startProfilesHub(t *testing.T) (*probeHub, string) {
	t.Helper()
	h := &probeHub{rt: newRuntime(t)}
	h.rt.SetPolicyValidation([]byte(policyContent), runtime.PolicyValidation{Hosts: []string{"b.test", "a.test"}})
	var svc *sandbox.Service
	cfg := profilesFile
	cfg.Compose = func(c *hub.Composition) {
		svc = c.Sandbox()
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			return h.rt.Client(s.OpenShellWorkspace, deps.Recorder), nil
		})
	}
	h.Hub = hubtest.StartWith(t, cfg)
	agent, ok := svc.Settings().Profile("agent")
	if !ok || agent.PolicyPath == "" {
		t.Fatalf("the hub has no profile agent with a policy file: %+v", agent)
	}
	return h, agent.PolicyPath
}

// storedSandbox is a sandbox a memorySource holds.
type storedSandbox struct {
	id, workspace, state, user string
	createdAt                  time.Time
}

// object is the sandbox object the source gives for s.
func (s storedSandbox) object() map[string]any {
	return map[string]any{
		"id": s.id, "workspace": s.workspace, "state": s.state, "created_by": s.user,
		"created_at": s.createdAt.Format(time.RFC3339), "profile": map[string]any{"name": "agent"},
	}
}

// memorySource is a sandbox source over a fixed list of sandboxes that
// counts its calls and fails each with err when err is set.
type memorySource struct {
	sandboxes []storedSandbox
	err       error
	calls     atomic.Int64
}

func (m *memorySource) List(_ context.Context, f sandbox.Filter, after *sandbox.Position, limit int) ([]sandbox.Item, error) {
	m.calls.Add(1)
	if m.err != nil {
		return nil, m.err
	}
	all := slices.Clone(m.sandboxes)
	slices.SortFunc(all, func(a, b storedSandbox) int {
		if c := b.createdAt.Compare(a.createdAt); c != 0 {
			return c
		}
		return strings.Compare(b.id, a.id)
	})
	var out []sandbox.Item
	for _, s := range all {
		if (f.State != "" && s.state != f.State) || (f.User != "" && s.user != f.User) {
			continue
		}
		if after != nil && !(s.createdAt.Before(after.CreatedAt) || (s.createdAt.Equal(after.CreatedAt) && s.id < after.ID)) {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, sandbox.Item{CreatedAt: s.createdAt, ID: s.id, Object: s.object()})
	}
	return out, nil
}

// startListHub starts a probe hub with sandboxes on over a fake runtime,
// with src registered as the sandbox source when it is not nil.
func startListHub(t *testing.T, src sandbox.Source) *probeHub {
	t.Helper()
	h := &probeHub{rt: runtimetest.New(t)}
	cfg := sandboxesOn
	cfg.Compose = func(c *hub.Composition) {
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			return h.rt.Client(s.OpenShellWorkspace, deps.Recorder), nil
		})
		if src != nil {
			c.Sandbox().RegisterSource(src)
		}
	}
	h.Hub = hubtest.StartWith(t, cfg)
	return h
}
