package sandbox_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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
	// header is set by the helpers that read the answer's headers.
	header http.Header
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

// storedRecord reads the stored sandbox id of the hub h.
func storedRecord(t *testing.T, h *hubtest.Hub, id string) sandbox.Record {
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

// recordingRecorder keeps the observations it is given, the sum of what is
// added to each series and the last value set in each.
type recordingRecorder struct {
	mu  sync.Mutex
	obs map[string][]float64
	// series maps a metric's name to its series, each under its labels
	// written as k=v pairs in key order, joined by commas.
	series map[string]map[string]float64
}

func (r *recordingRecorder) Add(name string, labels map[string]string, value float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seriesOf(name)[labelKey(labels)] += value
	return nil
}

func (r *recordingRecorder) Set(name string, labels map[string]string, value float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seriesOf(name)[labelKey(labels)] = value
	return nil
}

// seriesOf returns the series of the metric name. The caller holds r.mu.
func (r *recordingRecorder) seriesOf(name string) map[string]float64 {
	if r.series == nil {
		r.series = map[string]map[string]float64{}
	}
	if r.series[name] == nil {
		r.series[name] = map[string]float64{}
	}
	return r.series[name]
}

// labelKey writes labels as k=v pairs in key order, joined by commas.
func labelKey(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

// values returns the series of the metric name: the value under each label
// key (labelKey).
func (r *recordingRecorder) values(name string) map[string]float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.series[name])
}

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
	// forkLinker is the fork create's session linker, which accepts every
	// session id.
	forkLinker *stubLinker
	// clock is the hub's clock, at recordStart until the test advances it;
	// the provisioner's pass and bound count on it.
	clock *hubtest.JobClock
	// rec is the hub's metrics recorder.
	rec *recordingRecorder
	// hooks steer the runtime client the hub is given, in front of rt's.
	hooks *runtimeHooks
	// timers are the timers of the hub's clock the stored sandboxes asked
	// for.
	timers *timerLog
	// limits are the sandbox limits its creates are held to.
	limits *quotaLimits
	// dana is the user id of dana, key her API key.
	dana, key, org string
	// base is the commit main of demo points at in a create world.
	base string
}

// startCreateHub starts a create hub whose [sandbox] section holds the
// profiles given as TOML, with the dependencies of its stored sandboxes
// changed by adjust, in order. Its provisioner is held: the sandboxes it
// creates stay provisioning, with their marker, and the runtime is not
// called for them.
func startCreateHub(t *testing.T, profiles string, adjust ...func(*sandbox.Deps)) *createHub {
	t.Helper()
	return newCreateHub(t, profiles, false, adjust)
}

// startProvisionHub is startCreateHub with the provisioner running: it
// brings every sandbox the hub creates to ready or failed, through the
// hooks, on the hub's clock.
func startProvisionHub(t *testing.T, profiles string, adjust ...func(*sandbox.Deps)) *createHub {
	t.Helper()
	return newCreateHub(t, profiles, true, adjust)
}

func newCreateHub(t *testing.T, profiles string, provision bool, adjust []func(*sandbox.Deps)) *createHub {
	t.Helper()
	h := &createHub{
		probeHub: &probeHub{rt: runtimetest.New(t)}, linker: &stubLinker{}, forkLinker: &stubLinker{}, resolver: &stubResolver{}, minter: &spyMinter{},
		clock: hubtest.NewJobClock(func() time.Time { return recordStart }), rec: &recordingRecorder{}, hooks: &runtimeHooks{head: defaultHead},
		timers: &timerLog{}, limits: &quotaLimits{},
	}
	h.rt.SetExec(h.hooks.script)
	h.rt.AddProviderProfile("parley", runtime.ProviderProfile{ID: "anthropic", Variables: []string{"ANTHROPIC_API_KEY"}})
	h.rt.AddProviderProfile("parley", runtime.ProviderProfile{ID: "mirror", Variables: []string{"MIRROR_TOKEN"}})
	cfg := hubtest.Config{
		Server: sandboxesOn.Server,
		TOML:   "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" + profiles,
		Files:  map[string]string{"gateway-token": gatewayToken + "\n", "policy.yaml": policyContent},
		Env:    map[string]string{secret.EnvCurrentKey: secretsKey},
		Now:    h.clock.Now,
	}
	cfg.Compose = func(c *hub.Composition) {
		svc := c.Sandbox()
		h.svc, h.workspaces, h.secrets = svc, c.Workspaces(), c.Secrets()
		h.svc.RegisterSessionLinker(h.linker)
		c.Forks().RegisterSessionLinker(h.forkLinker)
		c.SetSandboxRecorder(h.rec)
		c.AdjustSandbox(func(d *sandbox.Deps) {
			h.resolver.next, d.ResolveSecrets = d.ResolveSecrets, h.resolver.resolve
			h.minter.next, d.Mint = d.Mint, h.minter.mint
			d.NewTimer = func(d time.Duration) sandbox.Timer { return h.timers.track(d, h.clock.NewTimer(d)) }
			d.HoldProvisioning = !provision
			d.Limits = func() sandbox.Limits { return h.limits.get(svc.Settings()) }
			for _, fn := range adjust {
				fn(d)
			}
		})
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			return &hookedClient{Client: h.rt.Client(s.OpenShellWorkspace, deps.Recorder), hooks: h.hooks}, nil
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

// fork adds the ready fork slug of parent, of dana, based on main,
// expiring at expires, in session (nil for none). It shares the parent's
// repository when the parent has one, and tracks the parent's branches, as
// a fork create does, or main when the parent has no repository.
func (h *createHub) fork(t *testing.T, slug, parent string, expires time.Time, session *string) {
	t.Helper()
	h.forkBy(t, slug, parent, h.dana, expires, session)
}

// forkBy is fork with the fork owned by the user owner.
func (h *createHub) forkBy(t *testing.T, slug, parent, owner string, expires time.Time, session *string) {
	t.Helper()
	f := h.forkRecord(t, slug, parent, owner, expires, session)
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return fork.Insert(ctx, tx, f) })
}

// forkRecord is the record of the fork forkBy adds, whose repository it
// makes, as a fork create does before it stores the record.
func (h *createHub) forkRecord(t *testing.T, slug, parent, owner string, expires time.Time, session *string) fork.Fork {
	t.Helper()
	w := h.record(slug, access.KindFork, workspace.StatusReady)
	w.ParentSlug, w.ExpiresAt, w.SessionID, w.OwnerID = &parent, &expires, session, owner
	tracking := []string{"main"}
	if _, err := os.Stat(h.workspaces.RepoPath(parent)); err == nil {
		ctx := context.Background()
		if err := (gitrepo.Git{}).CreateFork(ctx, h.workspaces.RepoPath(parent), h.workspaces.RepoPath(slug)); err != nil {
			t.Fatal(err)
		}
		repo, err := (gitrepo.Git{}).Open(ctx, h.workspaces.RepoPath(parent))
		if err != nil {
			t.Fatal(err)
		}
		branches, err := repo.Branches(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tracking = slices.Sorted(maps.Keys(branches))
	}
	return fork.Fork{Workspace: w, TrackingBranches: tracking}
}

// principal is dana as her API key authenticates her.
func (h *createHub) principal(t *testing.T) identity.Principal {
	t.Helper()
	u, err := identity.UserByID(context.Background(), h.Store.Main(), h.dana)
	if err != nil {
		t.Fatal(err)
	}
	return identity.Principal{Type: identity.TypeAPIKey, CredentialID: "key", User: &u}
}

// pass advances the hub's clock by d once the provisioner waits for its
// next pass, and waits until that pass has run.
func (h *createHub) pass(t *testing.T, d time.Duration) {
	t.Helper()
	eventually(t, "the provisioner to wait", func() bool { return h.timers.live(sandbox.ProvisionInterval) > 0 })
	before := h.timers.created(sandbox.ProvisionInterval)
	h.clock.Advance(d)
	eventually(t, "the provisioner's pass", func() bool { return h.timers.created(sandbox.ProvisionInterval) > before })
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

// seedRecord stores a sandbox of the workspace ws in state, created by dana
// at recordStart, as edit changes it when edit is not nil, and returns its
// id. One seeded in an ended state ended at its creation.
func (h *createHub) seedRecord(t *testing.T, ws, state string, edit func(*sandbox.Record)) string {
	t.Helper()
	rec := sandbox.Record{
		ID: sandbox.NewID(), Workspace: ws, Profile: "agent", State: state, Image: "img:1", CPU: "2", Memory: "4Gi",
		ExpiresAt: recordStart.Add(time.Hour), LastActivityAt: recordStart,
		CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"},
		CreatedAt: recordStart, UpdatedAt: recordStart, UserID: h.dana, FamilyRoot: "demo",
	}
	if edit != nil {
		edit(&rec)
	}
	switch rec.State {
	case sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed, sandbox.StateDeleted:
		ended := rec.CreatedAt
		rec.EndedAt = &ended
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.Insert(ctx, tx, rec) })
	return rec.ID
}

// getting sends GET path, under /api/v1, with cred as the bearer
// credential, under ctx, and delivers the answer on the channel it returns:
// with status 0 when the request got none, as when ctx ended first.
func (h *createHub) getting(ctx context.Context, path, cred string) <-chan answer {
	out := make(chan answer, 1)
	go func() {
		var a answer
		defer func() { out <- a }()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL("/api/v1"+path), nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+cred)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return
		}
		a.status, a.body = resp.StatusCode, body
	}()
	return out
}

// decoded decodes the JSON object of an answer that must have status.
func decoded(t *testing.T, a answer, status int) map[string]any {
	t.Helper()
	if a.status != status {
		t.Fatalf("answer = %d %s; want %d", a.status, a.body, status)
	}
	var o map[string]any
	if err := json.Unmarshal(a.body, &o); err != nil {
		t.Fatalf("the answer %s: %v", a.body, err)
	}
	return o
}

// timerLog keeps every timer of the hub's clock it tracks, with its
// duration and whether it was stopped.
type timerLog struct {
	mu     sync.Mutex
	timers []*trackedTimer
}

// trackedTimer is a timer of the hub's clock that a timerLog keeps.
type trackedTimer struct {
	sandbox.Timer
	d       time.Duration
	stopped atomic.Bool
}

func (t *trackedTimer) Stop() bool {
	t.stopped.Store(true)
	return t.Timer.Stop()
}

// track keeps the timer tm of d and returns it.
func (l *timerLog) track(d time.Duration, tm sandbox.Timer) sandbox.Timer {
	tt := &trackedTimer{Timer: tm, d: d}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.timers = append(l.timers, tt)
	return tt
}

// created is the number of timers of d asked for so far.
func (l *timerLog) created(d time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, tm := range l.timers {
		if tm.d == d {
			n++
		}
	}
	return n
}

// live is the number of timers of d not stopped.
func (l *timerLog) live(d time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, tm := range l.timers {
		if tm.d == d && !tm.stopped.Load() {
			n++
		}
	}
	return n
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
// case, unless refuse is set, and keeps what it was asked. Its record calls
// onRecord, when set, with the sandbox id, and fails with fail when fail is
// set.
type stubLinker struct {
	mu       sync.Mutex
	refuse   bool
	fail     error
	onRecord func(sandboxID string)
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
	if l.onRecord != nil {
		l.onRecord(sandboxID)
	}
	return l.fail
}

// setOnRecord makes fn the function a record calls; nil removes it.
func (l *stubLinker) setOnRecord(fn func(sandboxID string)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onRecord = fn
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
	next func(ctx context.Context, q access.Querier, slug string, names []string) (secret.SecretResolution, error)
}

func (r *stubResolver) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *stubResolver) resolve(ctx context.Context, q access.Querier, slug string, names []string) (secret.SecretResolution, error) {
	r.mu.Lock()
	err, next := r.err, r.next
	r.mu.Unlock()
	if err != nil {
		return secret.SecretResolution{}, err
	}
	return next(ctx, q, slug, names)
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

// defaultHead is the commit a create hub's runtime answers git rev-parse
// HEAD with until the test sets another.
const defaultHead = "0123456789abcdef0123456789abcdef01234567"

// runtimeHooks steer the provisioning's runtime calls of a create hub: hook,
// when set, is called before each EnsureProvider, CreateSandbox, WaitReady
// and RunMain with the operation and the provider's or sandbox's name, and
// an error it returns is the call's. exec, when set, is asked before each
// Exec: an error it returns is the call's, and a non-zero code ends the
// command with that code and stderr. They keep the command of each RunMain
// and, in one log, every command run in a sandbox, every main-process start
// and every restore, in the order they happened.
type runtimeHooks struct {
	mu    sync.Mutex
	hook  func(ctx context.Context, op, name string) error
	exec  func(argv []string) (code int, stderr string, err error)
	head  string
	mains map[string][]string
	log   []runCall
}

// runCall is an entry of the hooks' log: a command run in the sandbox
// (exec), with its working directory, a main-process start (main) with its
// command, or a restore (restore) with the new and the source sandbox's ids.
type runCall struct {
	sandbox, kind string
	argv          []string
	dir           string
}

// setExec makes fn the exec hook; nil removes it.
func (r *runtimeHooks) setExec(fn func(argv []string) (code int, stderr string, err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exec = fn
}

// setHead makes id the commit git rev-parse HEAD answers.
func (r *runtimeHooks) setHead(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head = id
}

// record adds c to the log.
func (r *runtimeHooks) record(c runCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, c)
}

// runs returns the log's entries about the sandbox id, in order.
func (r *runtimeHooks) runs(id string) []runCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []runCall
	for _, c := range r.log {
		if c.sandbox == id {
			out = append(out, c)
		}
	}
	return out
}

// script runs the commands of the hub's fake runtime: git rev-parse HEAD
// prints the head the test set, every other git command exits 0 with no
// output, and any other command, a main process, runs until it is ended.
func (r *runtimeHooks) script(ctx context.Context, _ string, req runtime.ExecRequest, stdout, _ io.Writer) int {
	if len(req.Command) == 0 || req.Command[0] != "git" {
		<-ctx.Done()
		return 0
	}
	if slices.Equal(req.Command, []string{"git", "rev-parse", "HEAD"}) {
		r.mu.Lock()
		head := r.head
		r.mu.Unlock()
		_, _ = io.WriteString(stdout, head+"\n")
	}
	return 0
}

// set makes fn the hook; nil removes it.
func (r *runtimeHooks) set(fn func(ctx context.Context, op, name string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hook = fn
}

// main returns the command the main process of the sandbox id was started
// with; ok is false when none was started.
func (r *runtimeHooks) main(id string) (command []string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	command, ok = r.mains[id]
	return command, ok
}

func (r *runtimeHooks) before(ctx context.Context, op, name string) error {
	r.mu.Lock()
	fn := r.hook
	r.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, op, name)
}

// hookedClient is a runtime client in front of a fake's, steered by hooks.
type hookedClient struct {
	runtime.Client
	hooks *runtimeHooks
}

func (c *hookedClient) EnsureProvider(ctx context.Context, p runtime.Provider) error {
	if err := c.hooks.before(ctx, runtime.OpEnsureProvider, p.Name); err != nil {
		return err
	}
	return c.Client.EnsureProvider(ctx, p)
}

func (c *hookedClient) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (runtime.SandboxStatus, error) {
	if err := c.hooks.before(ctx, runtime.OpCreateSandbox, spec.Name); err != nil {
		return runtime.SandboxStatus{}, err
	}
	return c.Client.CreateSandbox(ctx, spec)
}

func (c *hookedClient) WaitReady(ctx context.Context, name string) (runtime.SandboxStatus, error) {
	if err := c.hooks.before(ctx, runtime.OpWaitReady, name); err != nil {
		return runtime.SandboxStatus{}, err
	}
	return c.Client.WaitReady(ctx, name)
}

func (c *hookedClient) RunMain(ctx context.Context, name string, command []string) error {
	if err := c.hooks.before(ctx, runtime.OpRunMain, name); err != nil {
		return err
	}
	if err := c.Client.RunMain(ctx, name, command); err != nil {
		return err
	}
	c.hooks.mu.Lock()
	defer c.hooks.mu.Unlock()
	if c.hooks.mains == nil {
		c.hooks.mains = map[string][]string{}
	}
	c.hooks.mains[name] = slices.Clone(command)
	c.hooks.log = append(c.hooks.log, runCall{sandbox: name, kind: "main", argv: slices.Clone(command)})
	return nil
}

func (c *hookedClient) Exec(ctx context.Context, name string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	c.hooks.record(runCall{sandbox: name, kind: "exec", argv: slices.Clone(req.Command), dir: req.WorkDir})
	c.hooks.mu.Lock()
	fn := c.hooks.exec
	c.hooks.mu.Unlock()
	if fn != nil {
		code, stderr, err := fn(req.Command)
		if err != nil {
			return runtime.ExecResult{}, err
		}
		if code != 0 {
			return runtime.ExecResult{ExitCode: code, Stderr: []byte(stderr)}, nil
		}
	}
	return c.Client.Exec(ctx, name, req)
}

// stubRestorer is a restorer that logs each restore in the hooks' log and
// answers err while one is set.
type stubRestorer struct {
	hooks *runtimeHooks
	mu    sync.Mutex
	err   error
}

func (r *stubRestorer) Restore(_ context.Context, rec, source sandbox.Record) error {
	r.hooks.record(runCall{sandbox: rec.ID, kind: "restore", argv: []string{rec.ID, source.ID}})
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// pause holds the calls that wait on it until it is released or their
// context ends; entered is closed when the first call waits.
type pause struct {
	entered, release chan struct{}
	once             sync.Once
}

func newPause() *pause { return &pause{entered: make(chan struct{}), release: make(chan struct{})} }

// wait holds the caller until the pause is released (nil) or ctx ends (an
// error of kind unavailable, as the runtime client answers then).
func (p *pause) wait(ctx context.Context, op string) error {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return runtime.NewError(runtime.ErrUnavailable, op, ctx.Err().Error())
	}
}

// eventually waits up to 5 s, polling, for cond to hold, and fails the
// test then.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5 s for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// settled waits until the sandbox id is no longer provisioning and returns
// its record.
func (h *createHub) settled(t *testing.T, id string) sandbox.Record {
	t.Helper()
	var rec sandbox.Record
	eventually(t, "sandbox "+id+" to leave provisioning", func() bool {
		var err error
		rec, err = sandbox.Get(context.Background(), h.Store.Main(), id)
		if err != nil {
			t.Fatal(err)
		}
		return rec.State != sandbox.StateProvisioning
	})
	return rec
}

// settledObject waits until a get of the sandbox id of the workspace slug
// shows it no longer provisioning and returns that object.
func (h *createHub) settledObject(t *testing.T, slug, id string) map[string]any {
	t.Helper()
	var o map[string]any
	eventually(t, "sandbox "+id+" to leave provisioning", func() bool {
		o = decoded(t, h.call(t, http.MethodGet, "/workspaces/"+slug+"/sandboxes/"+id, h.key, nil), http.StatusOK)
		return o["state"] != sandbox.StateProvisioning
	})
	return o
}

// hubEvent is what a test checks of a stored hub event.
type hubEvent struct {
	actorType string
	metadata  map[string]any
}

// sandboxEvents returns the hub events of type typ about the sandbox id,
// oldest first.
func (h *createHub) sandboxEvents(t *testing.T, typ, id string) []hubEvent {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT actor_type, metadata FROM hub_events WHERE type = ? AND resource_id = ? ORDER BY seq`, typ, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []hubEvent
	for rows.Next() {
		var (
			e    hubEvent
			meta string
		)
		if err := rows.Scan(&e.actorType, &meta); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(meta), &e.metadata); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// runtimeLeft returns the names of the sandboxes and providers the runtime
// holds for the sandbox id, under the names the hub gives them.
func (h *createHub) runtimeLeft(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	if _, ok := h.rt.Spec("parley", id); ok {
		out = append(out, id)
	}
	for _, name := range []string{"hub", "model", "mirror"} {
		if _, ok := h.rt.Provider("parley", id+"-"+name); ok {
			out = append(out, id+"-"+name)
		}
	}
	return out
}

// stamp is how a sandbox object writes t.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// createEvent is a parley.sandbox.create event as the event log holds it.
type createEvent struct {
	resourceType, workspace, actorType, actorID string
	session                                     *string
	metadata                                    map[string]any
}

// createEvents returns the parley.sandbox.create events of the sandbox id.
func (h *createHub) createEvents(t *testing.T, id string) []createEvent {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT resource_type, workspace, actor_type, actor_id, session_id, metadata
		FROM hub_events WHERE type = 'parley.sandbox.create' AND resource_id = ? ORDER BY seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []createEvent
	for rows.Next() {
		var (
			e    createEvent
			meta string
		)
		if err := rows.Scan(&e.resourceType, &e.workspace, &e.actorType, &e.actorID, &e.session, &meta); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(meta), &e.metadata); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// strs returns n strings "a0", "a1", ….
func strs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "a" + strconv.Itoa(i)
	}
	return out
}

// entries returns a map of n entries "K0": "v", "K1": "v", ….
func entries(n int) map[string]string {
	out := make(map[string]string, n)
	for i := range n {
		out["K"+strconv.Itoa(i)] = "v"
	}
	return out
}

// wantRefused fails the test unless a is status with error type typ and
// the hub's footprint is still before.
func (h *createHub) wantRefused(t *testing.T, a answer, status int, typ string, before footprint) {
	t.Helper()
	if a.status != status || a.err.ErrorType != typ {
		t.Errorf("create = %d %s; want %d %s", a.status, a.body, status, typ)
	}
	if after := h.footprint(t); after != before {
		t.Errorf("a refused create left %+v; want %+v", after, before)
	}
}

// limits are the four sandbox limits of a hub, in the order they are
// checked.
type limits struct{ sandboxes, perOrg, perWorkspace, perUser int }

// only is limits with the limit name at n and the others at 100.
func only(name string, n int) limits {
	l := limits{100, 100, 100, 100}
	l.set(name, n)
	return l
}

// set sets the limit name to n.
func (l *limits) set(name string, n int) {
	switch name {
	case "max_sandboxes":
		l.sandboxes = n
	case "max_per_org":
		l.perOrg = n
	case "max_per_workspace":
		l.perWorkspace = n
	case "max_per_user":
		l.perUser = n
	}
}

// setLimits makes l the limits of the hub's next creates.
func (h *createHub) setLimits(l limits) {
	h.limits.set(sandbox.Limits{MaxSandboxes: l.sandboxes, MaxPerOrg: l.perOrg, MaxPerWorkspace: l.perWorkspace, MaxPerUser: l.perUser})
}

// quotaLimits are the sandbox limits of a create hub: those of its
// settings until the test sets others, which the requests then read.
type quotaLimits struct {
	mu    sync.Mutex
	given *sandbox.Limits
}

// set makes l the limits from now on.
func (q *quotaLimits) set(l sandbox.Limits) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.given = &l
}

// get returns the limits the test set, or those of settings.
func (q *quotaLimits) get(settings *sandbox.Settings) sandbox.Limits {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.given != nil {
		return *q.given
	}
	return sandbox.Limits{MaxSandboxes: settings.MaxSandboxes, MaxPerOrg: settings.MaxPerOrg, MaxPerWorkspace: settings.MaxPerWorkspace, MaxPerUser: settings.MaxPerUser}
}

// quotaHub is a create hub with a create world, the user omar, who creates
// no sandbox through the hub, and the org other.
type quotaHub struct {
	*createHub
	omar, other string
}

func startQuotaHub(t *testing.T) *quotaHub {
	t.Helper()
	h := &quotaHub{createHub: startCreateHub(t, agentProfile)}
	h.world(t)
	h.omar, h.other = h.CreateUser(t, "omar"), h.CreateOrg(t, "other")
	return h
}

// stored describes a sandbox a quota test stores directly: its workspace,
// the root of the workspace's family, the org (none for ""), the user who
// created it and its state.
type stored struct{ ws, family, org, user, state string }

// store stores s and returns its id.
func (h *createHub) store(t *testing.T, s stored) string {
	t.Helper()
	rec := sandbox.Record{
		ID: sandbox.NewID(), Workspace: s.ws, Profile: "agent", State: s.state,
		ExpiresAt: recordStart.Add(time.Hour), LastActivityAt: recordStart,
		CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: s.user, Username: "someone"},
		CreatedAt: recordStart, UpdatedAt: recordStart, UserID: s.user, OrgID: s.org, FamilyRoot: s.family,
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.Insert(ctx, tx, rec) })
	return rec.ID
}

// exec runs the statement query with args in a store transaction.
func (h *createHub) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
}

// capacity is the error object of a sandbox_capacity answer.
type capacity struct {
	ErrorType string `json:"error_type"`
	Message   string `json:"message"`
	Limit     string `json:"limit"`
	Maximum   int    `json:"maximum"`
	Current   int    `json:"current"`
}

// wantCapacity fails the test unless a is 429 sandbox_capacity naming the
// limit at maximum with the count current.
func wantCapacity(t *testing.T, a answer, limit string, maximum, current int) {
	t.Helper()
	var env struct{ Error capacity }
	_ = json.Unmarshal(a.body, &env)
	want := capacity{"sandbox_capacity", "sandbox limit reached: " + limit, limit, maximum, current}
	if a.status != http.StatusTooManyRequests || env.Error != want {
		t.Errorf("create = %d %s; want 429 with %+v", a.status, a.body, want)
	}
}

// nameList is a JSON list of names as a decoded answer holds it; never nil.
func nameList(n ...string) []any {
	out := make([]any, len(n))
	for i, v := range n {
		out[i] = v
	}
	return out
}

// errorObject decodes the error object of a, with its extra members.
func errorObject(t *testing.T, a answer) map[string]any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(a.body, &env); err != nil || env.Error == nil {
		t.Fatalf("the answer %d %s has no error object", a.status, a.body)
	}
	return env.Error
}

// wantMembers fails the test unless o has each member of want, compared as
// decoded JSON, so that [] and null differ.
func wantMembers(t *testing.T, o map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if !reflect.DeepEqual(o[k], v) {
			t.Errorf("%s = %#v; want %#v", k, o[k], v)
		}
	}
}

// dump is every row of the table, as text.
func dump(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM ` + table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			fmt.Fprintf(&b, "%s\t", v)
		}
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The values of the secrets of a provisioning world.
const (
	modelSecret = "sk-model-0123456789"
	npmSecret   = "npm_SECRETVALUE42"
)

// provisionProfile is the profile full of the provisioning tests: image
// img:1 with 2 CPUs and 4Gi of memory, the policy file policy.yaml, the
// credential model, the exposed secret NPM_TOKEN and the variable TIER.
const provisionProfile = "[[sandbox.profiles]]\nname = \"full\"\nimage = \"img:1\"\ncpu = \"2\"\nmemory = \"4Gi\"\nttl = \"4h\"\n" +
	"policy = \"policy.yaml\"\ncredentials = [\"model\"]\nexpose_secrets = [\"NPM_TOKEN\"]\nenv = { TIER = \"profile\" }\n"

// startProvisionWorld starts a provision hub with the profiles agent, full
// and plain and the credentials model and mirror, adds the create world,
// makes dana a member of acme and sets MODEL_KEY and NPM_TOKEN on acme.
func startProvisionWorld(t *testing.T, adjust ...func(*sandbox.Deps)) *createHub {
	t.Helper()
	h := startProvisionHub(t, agentProfile+provisionProfile+plainProfile+credentialTables, adjust...)
	h.world(t)
	h.AddMember(t, h.org, h.dana)
	h.putSecret(t, secret.ScopeOrg, h.org, "MODEL_KEY", modelSecret)
	h.putSecret(t, secret.ScopeOrg, h.org, "NPM_TOKEN", npmSecret)
	return h
}

// objectMap is the sandbox object of rec as a decoded answer holds it.
func objectMap(t *testing.T, rec sandbox.Record) map[string]any {
	t.Helper()
	b, err := json.Marshal(sandbox.ObjectOf(rec, false))
	if err != nil {
		t.Fatal(err)
	}
	var o map[string]any
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	return o
}

// decodedRules are rules as names and decoded bodies, for comparison.
func decodedRules(t *testing.T, rules []runtime.Rule) []any {
	t.Helper()
	out := []any{}
	for _, r := range rules {
		var body any
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("rule %s: %v", r.Name, err)
		}
		out = append(out, map[string]any{"name": r.Name, "body": body})
	}
	return out
}

// hubRule is the protected rule of the hub credential of the sandbox id of
// the workspace slug in acme, decoded: its provider, on the scheme, host
// and port of https://hub.test, for the API and git routes of the workspace
// only.
func hubRule(id, slug string) any {
	return map[string]any{"name": "parley-hub", "body": map[string]any{
		"providers": []any{id + "-hub"},
		"endpoints": []any{map[string]any{
			"scheme": "https", "host": "hub.test", "port": float64(443),
			"paths": []any{
				"/api/v1/workspaces/" + slug, "/api/v1/workspaces/" + slug + "/**",
				"/git/acme/" + slug + ".git", "/git/acme/" + slug + ".git/**",
			},
		}},
	}}
}

// credentialRule is the protected rule of the credential name of the
// sandbox id, decoded: it names the credential's provider.
func credentialRule(id, name string) any {
	return map[string]any{"name": "credential-" + name, "body": map[string]any{"providers": []any{id + "-" + name}}}
}

// sandboxOps are the operations of calls that name the sandbox id or one of
// its providers, in order, each run of commands executed in the sandbox
// counted as one exec.
func sandboxOps(calls []runtimetest.Call, id string) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasPrefix(c.Target, id) || (c.Op == runtime.OpExec && len(out) > 0 && out[len(out)-1] == runtime.OpExec) {
			continue
		}
		out = append(out, c.Op)
	}
	return out
}

// providerNames are the provider names of a sandbox spec without the
// sandbox id they start with, sorted.
func providerNames(spec runtime.SandboxSpec) []string {
	out := []string{}
	for _, p := range spec.Providers {
		out = append(out, strings.TrimPrefix(p, spec.Name+"-"))
	}
	slices.Sort(out)
	return out
}

// stubComposer is a policy composer whose Compose answers policy or err and
// whose Explain prefixes the refusal; it keeps the workspace rules it read.
type stubComposer struct {
	mu     sync.Mutex
	policy runtime.Policy
	err    error
	read   [][]sandbox.WorkspaceRule
}

func (c *stubComposer) Compose(_ context.Context, rec sandbox.Record) (runtime.Policy, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read = append(c.read, rec.WorkspaceRules)
	return c.policy, c.err
}

func (c *stubComposer) Explain(_ sandbox.Record, refusal error) string {
	return "the composer explains: " + refusal.Error()
}

func (c *stubComposer) rulesRead() [][]sandbox.WorkspaceRule {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.read)
}

// hubtestWait waits for ch to be closed.
func hubtestWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("waited 5 s for %s", what)
	}
}

// stubRules is a workspace rule provider that answers rules and keeps the
// workspaces it was asked for.
type stubRules struct {
	mu    sync.Mutex
	rules []sandbox.WorkspaceRule
	calls []string
}

func (r *stubRules) WorkspaceRules(_ context.Context, tx *sql.Tx, workspace string) ([]sandbox.WorkspaceRule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tx == nil {
		return nil, errors.New("asked outside the creating transaction")
	}
	r.calls = append(r.calls, workspace)
	return slices.Clone(r.rules), nil
}

func (r *stubRules) set(rules []sandbox.WorkspaceRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = rules
}

func (r *stubRules) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// sandboxPath is the path of the sandbox id of the workspace slug.
func sandboxPath(slug, id string) string { return "/workspaces/" + slug + "/sandboxes/" + id }

// listIDs returns the ids of the items of a 200 list answer, in order, and
// its next_cursor ("" for null).
func listIDs(t *testing.T, a answer) (ids []string, next string) {
	t.Helper()
	o := decoded(t, a, http.StatusOK)
	items, ok := o["items"].([]any)
	if !ok {
		t.Fatalf("the list %s has no items list", a.body)
	}
	ids = []string{}
	for _, it := range items {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	next, _ = o["next_cursor"].(string)
	return ids, next
}

// newestFirst returns ids sorted by id descending, the order of sandboxes
// created at the same instant.
func newestFirst(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	slices.Reverse(out)
	return out
}

// checkoutHead is the commit the runtime of a checkout world reports as
// HEAD in every sandbox.
const checkoutHead = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// startCheckoutWorld starts a provision hub with the profiles agent and
// plain, the ready repository demo of dana in acme, whose main has one
// commit and whose release has another on top of it, and the fork demo--t1
// of it (name t1, base main, tracking both, expiring in 7 days, in the
// session t1Session). NPM_TOKEN is set on acme, and the runtime reports
// checkoutHead as HEAD. It returns the hub and the commit of release.
func startCheckoutWorld(t *testing.T) (*createHub, string) {
	t.Helper()
	h := startProvisionHub(t, agentProfile+plainProfile)
	h.base = h.repository(t, "demo", workspace.StatusReady)
	dir := h.workspaces.RepoPath("demo")
	release := gitrepotest.Commit(t, dir, "release", map[string]string{"NOTES": "release\n"}, h.base)
	gitrepotest.SetRef(t, dir, "refs/heads/release", release)
	h.fork(t, "demo--t1", "demo", recordStart.Add(7*24*time.Hour), new(t1Session))
	h.AddMember(t, h.org, h.dana)
	h.putSecret(t, secret.ScopeOrg, h.org, "NPM_TOKEN", npmSecret)
	h.hooks.setHead(checkoutHead)
	return h, release
}

// provisioned creates a sandbox on slug with body, with the credential cred
// (dana's key when ""), waits until it leaves provisioning and returns the
// 201 answer's object and the record then.
func (h *createHub) provisioned(t *testing.T, slug, cred string, body map[string]any) (map[string]any, sandbox.Record) {
	t.Helper()
	if cred == "" {
		cred = h.key
	}
	o := sandboxObject(t, h.call(t, "POST", "/workspaces/"+slug+"/sandboxes", cred, body))
	return o, h.settled(t, o["id"].(string))
}

// gitRuns are the commands of runs that run git, each with its index in
// runs.
func gitRuns(runs []runCall) map[int][]string {
	out := map[int][]string{}
	for i, r := range runs {
		if r.kind == "exec" && len(r.argv) > 0 && r.argv[0] == "git" {
			out[i] = r.argv
		}
	}
	return out
}

// firstRun is the index of the first of runs whose command matches, -1 for
// none.
func firstRun(runs []runCall, match func(argv []string) bool) int {
	return slices.IndexFunc(runs, func(r runCall) bool { return r.kind == "exec" && match(r.argv) })
}

// isClone reports whether argv is a git clone.
func isClone(argv []string) bool { return len(argv) > 1 && argv[0] == "git" && argv[1] == "clone" }

// isCheckout reports whether argv is a git checkout.
func isCheckout(argv []string) bool {
	return len(argv) > 1 && argv[0] == "git" && argv[1] == "checkout"
}

// createsBranch reports whether argv creates a branch.
func createsBranch(argv []string) bool {
	if len(argv) < 2 || argv[0] != "git" {
		return false
	}
	switch argv[1] {
	case "branch":
		return true
	case "checkout":
		return slices.Contains(argv, "-b") || slices.Contains(argv, "-B")
	case "switch":
		return slices.Contains(argv, "-c") || slices.Contains(argv, "-C")
	}
	return false
}

// checkCloneAndOrder checks that the commands run in the sandbox id all ran
// in the working directory, that they hold exactly one clone of remote
// into it as a partial clone without blobs, and that every git command ran
// before the main process started, when one did.
func checkCloneAndOrder(t *testing.T, runs []runCall, remote string) {
	t.Helper()
	clones := 0
	for _, r := range runs {
		if r.kind == "exec" && r.dir != sandbox.WorkDir {
			t.Errorf("%v ran in %q; want %s", r.argv, r.dir, sandbox.WorkDir)
		}
		if r.kind == "exec" && isClone(r.argv) {
			clones++
			if !slices.Contains(r.argv, "--filter=blob:none") || !slices.Contains(r.argv, remote) || !slices.Contains(r.argv, sandbox.WorkDir) {
				t.Errorf("clone %v; want a partial clone with --filter=blob:none of %s into %s", r.argv, remote, sandbox.WorkDir)
			}
		}
	}
	if clones != 1 {
		t.Errorf("%d clones in %v; want one", clones, runs)
	}
	if main := slices.IndexFunc(runs, func(r runCall) bool { return r.kind == "main" }); main >= 0 {
		for i, argv := range gitRuns(runs) {
			if i > main {
				t.Errorf("%v ran after the main process started", argv)
			}
		}
	}
}

// errRollback ends a test's transaction, which the store then rolls back.
var errRollback = errors.New("roll back")

// postFork sends POST /workspaces/demo/forks with cred and body.
func (h *createHub) postFork(t *testing.T, cred string, body map[string]any) answer {
	t.Helper()
	return h.call(t, http.MethodPost, "/workspaces/demo/forks", cred, body)
}

// timeMember parses the RFC 3339 time of the member name of o.
func timeMember(t *testing.T, o map[string]any, name string) time.Time {
	t.Helper()
	s, _ := o[name].(string)
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("%s %v: %v", name, o[name], err)
	}
	return at
}

// createInFork creates in tx, through the fork create's sandbox component,
// a sandbox with an empty body on the fork f, which tx stores first, for
// dana and with no defaults, and returns its object.
func (h *createHub) createInFork(t *testing.T, ctx context.Context, tx *sql.Tx, f fork.Fork) map[string]any {
	t.Helper()
	if err := fork.Insert(ctx, tx, f); err != nil {
		t.Fatal(err)
	}
	raw, err := h.svc.CreateSandbox(ctx, tx, fork.SandboxRequest{Caller: h.principal(t), Fork: f, Sandbox: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("CreateSandbox = %v", err)
	}
	var o map[string]any
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("the sandbox object %s: %v", raw, err)
	}
	return o
}

// allHubScopes are the six scopes a hub credential can carry, sorted.
var allHubScopes = []string{"audit:write", "changes:read", "changes:write", "git:read", "git:write", "sessions:write"}

// jsonList is list as a decoded answer holds it: nil for nil.
func jsonList(list []string) any {
	if list == nil {
		return nil
	}
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}

// bindEvents counts the parley.token.bind events of the token id, or of
// every token for "".
func (h *createHub) bindEvents(t *testing.T, id string) int {
	t.Helper()
	return h.count(t, `SELECT count(*) FROM hub_events WHERE type = 'parley.token.bind' AND (? = '' OR resource_id = ?)`, id, id)
}

// useRoute is one of the routes that use a sandbox: the name the test hooks
// know it by, its method, the last segment of its path, and whether it is
// of kind write.
type useRoute struct {
	name, method, segment string
	write                 bool
}

// useRoutes are the five routes that use a sandbox.
var useRoutes = []useRoute{
	{"exec", http.MethodPost, "exec", true},
	{"upload", http.MethodPut, "files", true},
	{"download", http.MethodGet, "files", false},
	{"logs", http.MethodGet, "logs", false},
	{"attach", http.MethodGet, "attach", true},
}

func (u useRoute) String() string { return u.method + " " + u.segment }

// path is the route's path for the sandbox id of the workspace slug.
func (u useRoute) path(slug, id string) string { return sandboxPath(slug, id) + "/" + u.segment }

// body is a body the probe validations of a use hub accept: a command for
// exec, none for the others.
func (u useRoute) body() any {
	if u.name == "exec" {
		return map[string]any{"command": []string{"true"}}
	}
	return nil
}

// useHub is a probe hub with sandboxes on whose routes that use a sandbox
// run probes in place of their validations and operations, and whose owner
// has a second workspace, ws2.
type useHub struct {
	*probeHub
	svc *sandbox.Service
	// key is the owner's API key.
	key string
}

// startUseHub starts a use hub. Its operations count their calls in calls
// and answer 200, or return the error of fail when fail is not nil and
// returns one; exec's validation refuses an empty body 400 invalid_request,
// and the other routes validate nothing.
func startUseHub(t *testing.T, fail func() error) *useHub {
	t.Helper()
	h := &useHub{probeHub: &probeHub{rt: runtimetest.New(t)}}
	cfg := sandboxesOn
	cfg.Compose = func(c *hub.Composition) {
		h.svc = c.Sandbox()
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			return h.rt.Client(s.OpenShellWorkspace, deps.Recorder), nil
		})
		for _, u := range useRoutes {
			sandbox.SetUseOperation(h.svc, u.name, func(w http.ResponseWriter, r *http.Request) error {
				if fail != nil {
					if err := fail(); err != nil {
						h.calls.Add(1)
						return err
					}
				}
				return h.probe(w, r)
			})
			sandbox.SetUseValidation(h.svc, u.name, func(*http.Request) error { return nil })
		}
		sandbox.SetUseValidation(h.svc, "exec", func(r *http.Request) error {
			if b, _ := io.ReadAll(r.Body); len(b) == 0 {
				return server.InvalidRequest("request body is empty; want a JSON object")
			}
			return nil
		})
	}
	h.Hub = hubtest.StartWith(t, cfg)
	h.owner, h.member, h.outsider = h.CreateUser(t, "owner"), h.CreateUser(t, "member"), h.CreateUser(t, "outsider")
	org := h.CreateOrg(t, "team")
	h.AddMember(t, org, h.member)
	h.AddWorkspace(t, access.Record{Slug: "ws", OwnerID: h.owner, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady})
	h.AddWorkspace(t, access.Record{Slug: "ws2", OwnerID: h.owner, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady})
	h.key = h.IssueKey(t, h.owner)
	return h
}

// seed stores a sandbox of the workspace ws, created by the owner's API
// key, in state, with the hub credential ha (nil for none), and returns its
// id.
func (h *useHub) seed(t *testing.T, ws, state string, ha *sandbox.HubAccess) string {
	t.Helper()
	before := recordStart.Add(-time.Hour)
	rec := sandbox.Record{
		ID: sandbox.NewID(), Workspace: ws, Profile: "agent", State: state, HubAccess: ha,
		ExpiresAt: recordStart.Add(3 * time.Hour), LastActivityAt: before,
		CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: h.owner, Username: "owner"},
		CreatedAt: before, UpdatedAt: before, UserID: h.owner, FamilyRoot: ws,
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

// useCall sends a request to the use route u for the sandbox id of the
// workspace slug with cred and body, and checks its answer: status and
// errorType, and, for a status other than 200, that neither the operation
// nor the runtime was reached; for 200, that the operation was.
func (h *useHub) useCall(t *testing.T, u useRoute, slug, id, cred string, body any, status int, errorType string) answer {
	t.Helper()
	calls, runtimeCalls := h.calls.Load(), len(h.rt.Calls())
	a := h.call(t, u.method, u.path(slug, id), cred, body)
	if a.status != status || a.err.ErrorType != errorType {
		t.Errorf("%s %s = %d %s, want %d %s", u.method, u.path(slug, id), a.status, a.body, status, errorType)
	}
	reached := h.calls.Load() - calls
	switch {
	case status == http.StatusOK && reached != 1:
		t.Errorf("the operation ran %d times, want once", reached)
	case status != http.StatusOK && reached != 0:
		t.Errorf("the operation ran %d times, want never", reached)
	}
	if status != http.StatusOK {
		if n := len(h.rt.Calls()) - runtimeCalls; n != 0 {
			t.Errorf("the runtime got %d calls, want none", n)
		}
	}
	return a
}

// execHub is a hub with sandboxes on over a fake runtime whose commands
// cmds runs, on a clock the test advances, with the repository ws of owner,
// whose API key is key.
type execHub struct {
	*probeHub
	clock *hubtest.JobClock
	cmds  *commands
	key   string
}

// startExecHub starts an exec hub that runs at most maxExecs commands at
// once in a sandbox, its clock at recordStart; [sandbox].max_exec_timeout
// is its default, 30m.
func startExecHub(t *testing.T, maxExecs int) *execHub {
	t.Helper()
	h := &execHub{
		probeHub: &probeHub{rt: runtimetest.New(t)}, clock: hubtest.NewJobClock(func() time.Time { return recordStart }),
		cmds: &commands{scripts: map[string]runtimetest.ExecFunc{}},
	}
	h.rt.SetExec(h.cmds.run)
	cfg := sandboxesOn
	cfg.TOML = "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" +
		"max_concurrent_execs = " + strconv.Itoa(maxExecs) + "\n[[sandbox.profiles]]\nname = \"agent\"\nttl = \"4h\"\n"
	cfg.Now = h.clock.Now
	cfg.Compose = func(c *hub.Composition) {
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			return h.rt.Client(s.OpenShellWorkspace, deps.Recorder), nil
		})
		c.AdjustSandbox(func(d *sandbox.Deps) {
			d.Now = h.clock.Now
			d.NewTimer = func(x time.Duration) sandbox.Timer { return h.clock.NewTimer(x) }
		})
	}
	h.Hub = hubtest.StartWith(t, cfg)
	h.owner = h.CreateUser(t, "owner")
	h.AddWorkspace(t, access.Record{Slug: "ws", OwnerID: h.owner, OrgID: h.CreateOrg(t, "team"), Kind: access.KindRepository, Status: access.StatusReady})
	h.key = h.IssueKey(t, h.owner)
	return h
}

// sandbox stores a ready sandbox of ws created by the owner's API key, as
// edit changes it when edit is not nil, creates it in the runtime with the
// working directory /workspace and the environment PARLEY_SANDBOX_ID and
// MODE=profile, and returns its id.
func (h *execHub) sandbox(t *testing.T, edit func(*sandbox.Record)) string {
	t.Helper()
	before := recordStart.Add(-time.Hour)
	rec := sandbox.Record{
		ID: sandbox.NewID(), Workspace: "ws", Profile: "agent", State: sandbox.StateReady,
		ExpiresAt: recordStart.Add(3 * time.Hour), LastActivityAt: before,
		CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: h.owner, Username: "owner"},
		CreatedAt: before, UpdatedAt: before, UserID: h.owner, FamilyRoot: "ws",
	}
	if edit != nil {
		edit(&rec)
	}
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return sandbox.Insert(ctx, tx, rec) }); err != nil {
		t.Fatalf("insert a sandbox: %v", err)
	}
	spec := runtime.SandboxSpec{Name: rec.ID, WorkDir: sandbox.WorkDir, Env: map[string]string{"PARLEY_SANDBOX_ID": rec.ID, "MODE": "profile"}}
	if _, err := h.rt.Client("parley", nil).CreateSandbox(ctx, spec); err != nil {
		t.Fatalf("create the sandbox in the runtime: %v", err)
	}
	return rec.ID
}

// startMain starts the main process of the sandbox id, which runs until it
// is stopped.
func (h *execHub) startMain(t *testing.T, id string) {
	t.Helper()
	if err := h.rt.Client("parley", nil).RunMain(context.Background(), id, []string{"main"}); err != nil {
		t.Fatalf("start the main process of %s: %v", id, err)
	}
}

// mainRunning reports whether the main process of the sandbox id runs.
func (h *execHub) mainRunning(t *testing.T, id string) bool {
	t.Helper()
	st, err := h.rt.Client("parley", nil).GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s from the runtime: %v", id, err)
	}
	return !st.MainExited
}

// exec sends POST …/exec for the sandbox id of ws as the owner with body
// and the headers of header, and reads the answer with its headers and
// its error envelope, if it has one.
func (h *execHub) exec(t *testing.T, id string, body any, header http.Header) answer {
	t.Helper()
	var a answer
	a.status, a.header, a.body = h.Exchange(t, http.MethodPost, "/api/v1"+sandboxPath("ws", id)+"/exec", h.key, body, header)
	var env struct {
		Error *json.RawMessage `json:"error"`
	}
	if json.Unmarshal(a.body, &env) == nil && env.Error != nil {
		_ = json.Unmarshal(*env.Error, &a.err)
	}
	return a
}

// execing sends what exec sends under ctx, from another goroutine, and
// delivers the answer on the channel it returns: with status 0 when the
// request got none, as when ctx ended first or the hub dropped the
// connection.
func (h *execHub) execing(ctx context.Context, id string, body any, header http.Header) <-chan answer {
	out := make(chan answer, 1)
	raw, _ := json.Marshal(body)
	go func() {
		var a answer
		defer func() { out <- a }()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL("/api/v1"+sandboxPath("ws", id)+"/exec"), bytes.NewReader(raw))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+h.key)
		req.Header.Set("Content-Type", "application/json")
		for k, vs := range header {
			req.Header[k] = vs
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return
		}
		a.status, a.header, a.body = resp.StatusCode, resp.Header, b
	}()
	return out
}

// commands run the commands of an exec hub's runtime. main, a main process,
// runs until it is stopped; any other command runs the script set for its
// first argument, or exits 0 with no output when none is set. They keep the
// request of every command but main, in order, and count the commands
// running, those waiting in a blocking script and those killed: ended by
// their context before they returned.
type commands struct {
	mu                      sync.Mutex
	scripts                 map[string]runtimetest.ExecFunc
	ran                     []runtime.ExecRequest
	running, waiting, kills int
}

// set makes fn the script of the commands named name.
func (c *commands) set(name string, fn runtimetest.ExecFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scripts[name] = fn
}

// blocking is a script that writes stdout and stderr and then waits until
// it is killed, counted as waiting meanwhile.
func (c *commands) blocking(stdout, stderr string) runtimetest.ExecFunc {
	return func(ctx context.Context, _ string, _ runtime.ExecRequest, out, errOut io.Writer) int {
		_, _ = io.WriteString(out, stdout)
		_, _ = io.WriteString(errOut, stderr)
		c.mu.Lock()
		c.waiting++
		c.mu.Unlock()
		<-ctx.Done()
		c.mu.Lock()
		c.waiting--
		c.mu.Unlock()
		return 137
	}
}

func (c *commands) run(ctx context.Context, name string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
	if req.Command[0] == "main" {
		<-ctx.Done()
		return 0
	}
	c.mu.Lock()
	c.ran = append(c.ran, req)
	c.running++
	fn := c.scripts[req.Command[0]]
	c.mu.Unlock()
	code := 0
	if fn != nil {
		code = fn(ctx, name, req, stdout, stderr)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running--
	if ctx.Err() != nil {
		c.kills++
	}
	return code
}

// counts are the number of commands run so far, of those running, of
// those waiting in a blocking script and of those killed.
func (c *commands) counts() (ran, running, waiting, kills int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ran), c.running, c.waiting, c.kills
}

// last is the request of the last command run.
func (c *commands) last(t *testing.T) runtime.ExecRequest {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ran) == 0 {
		t.Fatal("no command ran")
	}
	return c.ran[len(c.ran)-1]
}
