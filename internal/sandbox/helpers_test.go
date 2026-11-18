package sandbox_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
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
// ("" for none) and body, as hubtest's Call does, and reads the error
// envelope of the answer, if it has one.
func (h *probeHub) call(t *testing.T, method, path, cred string, body any) answer {
	t.Helper()
	var a answer
	a.status, a.body = h.Call(t, method, "/api/v1"+path, cred, body)
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

// recordStart is where the clock of a record hub starts: a whole second,
// since the store keeps whole seconds.
var recordStart = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

// recordSession is the session of the sandboxes a record hub seeds.
const recordSession = "0b4e7c1a-5d3f-4a2b-9c8d-7e6f5a4b3c2d"

// recordHub is a hub with sandboxes on over a fake runtime, on a clock the
// test advances, whose sandbox records the test writes and moves through the
// service directly.
type recordHub struct {
	*hubtest.Hub
	svc   *sandbox.Service
	clock *hubtest.JobClock
	rec   *recordingRecorder
}

// startRecordHub starts a record hub, its clock at recordStart.
func startRecordHub(t *testing.T) *recordHub {
	t.Helper()
	h := &recordHub{clock: hubtest.NewJobClock(func() time.Time { return recordStart }), rec: &recordingRecorder{}}
	fake := runtimetest.New(t)
	cfg := sandboxesOn
	cfg.Now = h.clock.Now
	cfg.Compose = func(c *hub.Composition) {
		h.svc = c.Sandbox()
		c.SetSandboxRecorder(h.rec)
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			return fake.Client(s.OpenShellWorkspace, deps.Recorder), nil
		})
	}
	h.Hub = hubtest.StartWith(t, cfg)
	return h
}

// seed stores a sandbox of the workspace ws in state, created an hour before
// recordStart in the session recordSession, and returns its id. A sandbox
// seeded in an ended state ended then too.
func (h *recordHub) seed(t *testing.T, state string) string {
	t.Helper()
	before := recordStart.Add(-time.Hour)
	session := recordSession
	rec := sandbox.Record{
		ID: sandbox.NewID(), Workspace: "ws", SessionID: &session, Profile: "agent", State: state,
		ExpiresAt: recordStart.Add(3 * time.Hour), LastActivityAt: before,
		CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: "u1", Username: "owner"},
		CreatedAt: before, UpdatedAt: before, UserID: "u1", FamilyRoot: "ws",
	}
	switch state {
	case sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed, sandbox.StateDeleted:
		rec.EndedAt = &before
	}
	err := h.Store.Tx(context.Background(), func(tx *sql.Tx) error { return sandbox.Insert(context.Background(), tx, rec) })
	if err != nil {
		t.Fatalf("insert a sandbox in %s: %v", state, err)
	}
	return rec.ID
}

// get reads the stored sandbox id.
func (h *recordHub) get(t *testing.T, id string) sandbox.Record {
	t.Helper()
	rec, err := sandbox.Get(context.Background(), h.Store.Main(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return rec
}

// storedEvent is what a test checks of a hub event about a sandbox.
type storedEvent struct {
	typ, resourceType string
	workspace         *string
	session           *string
}

// events returns the hub events whose resource is the sandbox id, oldest
// first.
func (h *recordHub) events(t *testing.T, id string) []storedEvent {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT type, resource_type, workspace, session_id FROM hub_events WHERE resource_id = ? ORDER BY seq`, id)
	if err != nil {
		t.Fatalf("read the events of %s: %v", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []storedEvent
	for rows.Next() {
		var e storedEvent
		if err := rows.Scan(&e.typ, &e.resourceType, &e.workspace, &e.session); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// recordingRecorder keeps the observations it is given; it ignores Add and
// Set.
type recordingRecorder struct {
	mu  sync.Mutex
	obs map[string][]float64
}

func (r *recordingRecorder) Add(string, map[string]string, float64) error { return nil }
func (r *recordingRecorder) Set(string, map[string]string, float64) error { return nil }

func (r *recordingRecorder) Observe(name string, _ map[string]string, value float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.obs == nil {
		r.obs = map[string][]float64{}
	}
	r.obs[name] = append(r.obs[name], value)
	return nil
}

// observations returns the values observed in the metric name, in order.
func (r *recordingRecorder) observations(name string) []float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.obs[name])
}

func TestMain(m *testing.M) {
	hubtest.LimitParallel(hubtest.HubParallel)
	os.Exit(m.Run())
}

// agentProfile is the profile agent of the create tests, marked default.
const agentProfile = "[[sandbox.profiles]]\nname = \"agent\"\ndefault = true\nimage = \"img:1\"\ncpu = \"2\"\nmemory = \"4Gi\"\n" +
	"ttl = \"4h\"\nidle_timeout = \"30m\"\nhub_access = \"fork\"\n"

// plainProfile is a profile without hub access that exposes the secret
// NPM_TOKEN.
const plainProfile = "[[sandbox.profiles]]\nname = \"plain\"\nttl = \"4h\"\nhub_access = \"none\"\nexpose_secrets = [\"NPM_TOKEN\"]\n"

// t1Session is the session of the fork demo--t1 of a create world.
const t1Session = "6f1c2f0e-0000-4000-8000-0000000000aa"

// credentialTables are the credentials model (provider profile anthropic,
// secret MODEL_KEY) and mirror (provider profile mirror, secret MIRROR_KEY).
const credentialTables = "[[sandbox.credentials]]\nname = \"model\"\nprovider_profile = \"anthropic\"\nsecret = \"MODEL_KEY\"\n" +
	"[[sandbox.credentials]]\nname = \"mirror\"\nprovider_profile = \"mirror\"\nsecret = \"MIRROR_KEY\"\n"

// secretsKey is the encryption key of a create hub's secret store, as the
// operator configures it.
var secretsKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

// createHub is a hub with sandboxes on over a fake runtime, on a clock
// fixed at recordStart, with the users dana, who owns every workspace the
// test adds, in the org acme, a session linker the test steers, a secret
// store with an encryption key, a resolver in front of the hub's secret
// resolution that the test can make fail, and a minter in front of the
// hub's minting component that keeps the values it minted and that the test
// can make fail. The runtime holds the provider profiles anthropic and
// mirror of credentialTables.
type createHub struct {
	*probeHub
	svc        *sandbox.Service
	workspaces *workspace.Service
	secrets    *secret.Component
	resolver   *stubResolver
	minter     *spyMinter
	linker     *stubLinker
	// dana is the user id of dana, key her API key.
	dana, key, org string
	// base is the commit main of demo points at in a create world.
	base string
}

// startCreateHub starts a create hub whose [sandbox] section holds the
// profiles given as TOML, with the dependencies of its stored sandboxes
// changed by adjust, in order.
func startCreateHub(t *testing.T, profiles string, adjust ...func(*sandbox.Deps)) *createHub {
	t.Helper()
	h := &createHub{probeHub: &probeHub{rt: runtimetest.New(t)}, linker: &stubLinker{}, resolver: &stubResolver{}, minter: &spyMinter{}}
	h.rt.AddProviderProfile("parley", runtime.ProviderProfile{ID: "anthropic", Variables: []string{"ANTHROPIC_API_KEY"}})
	h.rt.AddProviderProfile("parley", runtime.ProviderProfile{ID: "mirror", Variables: []string{"MIRROR_TOKEN"}})
	cfg := hubtest.Config{
		Server: sandboxesOn.Server,
		TOML:   "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" + profiles,
		Files:  sandboxesOn.Files,
		Env:    map[string]string{secret.EnvCurrentKey: secretsKey},
		Now:    func() time.Time { return recordStart },
	}
	cfg.Compose = func(c *hub.Composition) {
		h.svc, h.workspaces, h.secrets = c.Sandbox(), c.Workspaces(), c.Secrets()
		h.svc.RegisterSessionLinker(h.linker)
		c.AdjustSandbox(func(d *sandbox.Deps) {
			h.resolver.next, d.ResolveSecrets = d.ResolveSecrets, h.resolver.resolve
			h.minter.next, d.Mint = d.Mint, h.minter.mint
			for _, fn := range adjust {
				fn(d)
			}
		})
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			return h.rt.Client(s.OpenShellWorkspace, deps.Recorder), nil
		})
	}
	h.Hub = hubtest.StartWith(t, cfg)
	h.dana = h.CreateUser(t, "dana")
	h.key = h.IssueKey(t, h.dana)
	h.org = h.CreateOrg(t, "acme")
	return h
}

// world adds the ready repository demo, whose main has one commit, its
// fork demo--t1 (name t1, base main, expiring in 7 days, in the session
// t1Session) and its fork demo--t2 (expiring in an hour, in no session).
func (h *createHub) world(t *testing.T) {
	t.Helper()
	h.base = h.repository(t, "demo", workspace.StatusReady)
	h.fork(t, "demo--t1", "demo", recordStart.Add(7*24*time.Hour), new(t1Session))
	h.fork(t, "demo--t2", "demo", recordStart.Add(time.Hour), nil)
}

// repository adds the repository workspace slug of dana in acme, with the
// default branch main, in status. A ready one gets a repository whose main
// points at a commit, whose id it returns.
func (h *createHub) repository(t *testing.T, slug string, status workspace.Status) string {
	t.Helper()
	commit := ""
	if status == workspace.StatusReady {
		dir := h.workspaces.RepoPath(slug)
		gitrepotest.InitBare(t, dir, "main")
		commit = gitrepotest.Commit(t, dir, "first", map[string]string{"README": "demo\n"})
		gitrepotest.SetRef(t, dir, "refs/heads/main", commit)
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		return workspace.Insert(ctx, tx, h.record(slug, access.KindRepository, status))
	})
	return commit
}

// fork adds the ready fork slug of parent, of dana, based on and tracking
// main, expiring at expires, in session (nil for none). It shares the
// parent's repository when the parent has one.
func (h *createHub) fork(t *testing.T, slug, parent string, expires time.Time, session *string) {
	t.Helper()
	h.forkBy(t, slug, parent, h.dana, expires, session)
}

// forkBy is fork with the fork owned by the user owner.
func (h *createHub) forkBy(t *testing.T, slug, parent, owner string, expires time.Time, session *string) {
	t.Helper()
	w := h.record(slug, access.KindFork, workspace.StatusReady)
	w.ParentSlug, w.ExpiresAt, w.SessionID, w.OwnerID = &parent, &expires, session, owner
	if _, err := os.Stat(h.workspaces.RepoPath(parent)); err == nil {
		if err := (gitrepo.Git{}).CreateFork(context.Background(), h.workspaces.RepoPath(parent), h.workspaces.RepoPath(slug)); err != nil {
			t.Fatal(err)
		}
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		return fork.Insert(ctx, tx, fork.Fork{Workspace: w, TrackingBranches: []string{"main"}})
	})
}

// record is the workspace record slug of kind of dana in acme, with the
// default branch main, in status.
func (h *createHub) record(slug, kind string, status workspace.Status) workspace.Workspace {
	return workspace.Workspace{
		Slug: slug, Kind: kind, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
		OwnerID: h.dana, OrgID: h.org, Status: status, DisplayName: slug,
		CreatedAt: recordStart, UpdatedAt: recordStart, InstanceID: workspace.NewInstanceID(),
	}
}

// insert runs fn in a store transaction.
func (h *createHub) insert(t *testing.T, fn func(ctx context.Context, tx *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
}

// seedIn stores a ready sandbox of the workspace ws with the checkpoint cp
// (nil for none) and returns its id.
func (h *createHub) seedIn(t *testing.T, ws string, cp json.RawMessage) string {
	t.Helper()
	rec := sandbox.Record{
		ID: sandbox.NewID(), Workspace: ws, Profile: "agent", State: sandbox.StateReady, Checkpoint: cp,
		ExpiresAt: recordStart.Add(time.Hour), LastActivityAt: recordStart,
		CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"},
		CreatedAt: recordStart, UpdatedAt: recordStart, UserID: h.dana, FamilyRoot: "demo",
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.Insert(ctx, tx, rec) })
	return rec.ID
}

// post sends POST /workspaces/slug/sandboxes as dana with body.
func (h *createHub) post(t *testing.T, slug string, body any) answer {
	t.Helper()
	return h.call(t, http.MethodPost, "/workspaces/"+slug+"/sandboxes", h.key, body)
}

// footprint is what a refused create must leave as it was: the number of
// sandbox records, tokens and hub events, and of runtime calls.
type footprint struct{ sandboxes, tokens, events, calls int }

// footprint counts what a create could add.
func (h *createHub) footprint(t *testing.T) footprint {
	t.Helper()
	return footprintOf(t, h.Hub, h.rt)
}

// footprintOf counts what a create could add to h, whose runtime is rt (nil
// for none).
func footprintOf(t *testing.T, h *hubtest.Hub, rt *runtimetest.Fake) footprint {
	t.Helper()
	var f footprint
	db := h.Store.Main()
	for _, c := range []struct {
		table string
		n     *int
	}{{"sandboxes", &f.sandboxes}, {"pats", &f.tokens}, {"hub_events", &f.events}} {
		if err := db.QueryRow(`SELECT count(*) FROM ` + c.table).Scan(c.n); err != nil {
			t.Fatal(err)
		}
	}
	if rt != nil {
		f.calls = len(rt.Calls())
	}
	return f
}

// sandboxObject decodes the sandbox object of a 201 answer.
func sandboxObject(t *testing.T, a answer) map[string]any {
	t.Helper()
	if a.status != http.StatusCreated {
		t.Fatalf("create = %d %s; want 201", a.status, a.body)
	}
	var o map[string]any
	if err := json.Unmarshal(a.body, &o); err != nil {
		t.Fatalf("the answer %s: %v", a.body, err)
	}
	return o
}

// stubLinker is a session linker that accepts every session id, in lower
// case, unless refuse is set, and keeps what it was asked. Its record fails
// with fail when fail is set.
type stubLinker struct {
	mu       sync.Mutex
	refuse   bool
	fail     error
	checks   []string
	recorded []string
}

func (l *stubLinker) setRefuse(v bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refuse = v
}

func (l *stubLinker) Check(_ context.Context, caller identity.Principal, id, ws string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	user := ""
	if caller.User != nil {
		user = caller.User.ID
	}
	l.checks = append(l.checks, id+" "+user+" "+ws)
	if l.refuse {
		return "", sandbox.ErrUnknownSession
	}
	return strings.ToLower(id), nil
}

func (l *stubLinker) Record(_ context.Context, _ *sql.Tx, id, ws, sandboxID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recorded = append(l.recorded, id+" "+ws+" "+sandboxID)
	return l.fail
}

func (l *stubLinker) setFail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail = err
}

// stubResolver stands in front of the hub's secret resolution: it answers
// err while one is set and asks the hub's resolution otherwise.
type stubResolver struct {
	mu   sync.Mutex
	err  error
	next func(ctx context.Context, slug string, names []string) (secret.SecretResolution, error)
}

func (r *stubResolver) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *stubResolver) resolve(ctx context.Context, slug string, names []string) (secret.SecretResolution, error) {
	r.mu.Lock()
	err, next := r.err, r.next
	r.mu.Unlock()
	if err != nil {
		return secret.SecretResolution{}, err
	}
	return next(ctx, slug, names)
}

// putSecret stores the secret name with value in the scope scope of the
// owner id: an org id, a workspace slug or a user id.
func (h *createHub) putSecret(t *testing.T, scope secret.Scope, id, name, value string) {
	t.Helper()
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return h.secrets.Put(ctx, tx, scope, id, name, value) })
}

// last returns the last check and the last record, "" for none.
func (l *stubLinker) last() (check, record string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := len(l.checks); n > 0 {
		check = l.checks[n-1]
	}
	if n := len(l.recorded); n > 0 {
		record = l.recorded[n-1]
	}
	return check, record
}

// spyMinter stands in front of the hub's minting component: it answers err
// while one is set and mints through the hub's component otherwise, keeping
// the value of every token it minted.
type spyMinter struct {
	mu     sync.Mutex
	err    error
	values []string
	next   func(ctx context.Context, tx *sql.Tx, in access.MintInput) (identity.PAT, string, error)
}

func (m *spyMinter) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// minted returns the values of the tokens minted so far, in order.
func (m *spyMinter) minted() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.values)
}

func (m *spyMinter) mint(ctx context.Context, tx *sql.Tx, in access.MintInput) (identity.PAT, string, error) {
	m.mu.Lock()
	err, next := m.err, m.next
	m.mu.Unlock()
	if err != nil {
		return identity.PAT{}, "", err
	}
	pat, value, err := next(ctx, tx, in)
	if err == nil {
		m.mu.Lock()
		m.values = append(m.values, value)
		m.mu.Unlock()
	}
	return pat, value, err
}

// tokenIDs returns the ids of every PAT the hub holds.
func (h *createHub) tokenIDs(t *testing.T) map[string]bool {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT token_id FROM pats`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// newTokens returns the PATs the hub holds whose ids are not in before.
func (h *createHub) newTokens(t *testing.T, before map[string]bool) []identity.PAT {
	t.Helper()
	var out []identity.PAT
	for id := range h.tokenIDs(t) {
		if before[id] {
			continue
		}
		pat, err := identity.PATByID(context.Background(), h.Store.Main(), id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, pat)
	}
	return out
}

// count returns the single number the query with args reads.
func (h *createHub) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.Store.Main().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
