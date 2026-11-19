package sandbox_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"golang.org/x/net/websocket"

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
// answers 200 and counts its calls. The probe hubs, the profiles hub, the
// list hub and the use hub run no policy collector: its listing of the
// runtime's proposals would stand beside the calls their tests count.
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
			c.SetSandboxPolicyCollector(sandbox.PolicyCollectorOptions{Hold: true})
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
		c.SetSandboxPolicyCollector(sandbox.PolicyCollectorOptions{Hold: true})
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
		c.SetSandboxPolicyCollector(sandbox.PolicyCollectorOptions{Hold: true})
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
// mirror of credentialTables. The hub runs no policy collector, whose calls
// would stand beside those its tests check.
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
	// proposalSeeds counts the proposals seedProposal stored.
	proposalSeeds atomic.Int64
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
	return newCreateHub(t, runtimetest.New(t), profiles, false, adjust)
}

// startProvisionHub is startCreateHub with the provisioner running: it
// brings every sandbox the hub creates to ready or failed, through the
// hooks, on the hub's clock.
func startProvisionHub(t *testing.T, profiles string, adjust ...func(*sandbox.Deps)) *createHub {
	t.Helper()
	return newCreateHub(t, runtimetest.New(t), profiles, true, adjust)
}

// newCreateHub starts a create hub over the fake runtime rt, whose
// commands the hooks' script runs.
func newCreateHub(t *testing.T, rt *runtimetest.Fake, profiles string, provision bool, adjust []func(*sandbox.Deps)) *createHub {
	t.Helper()
	h := &createHub{
		probeHub: &probeHub{rt: rt}, linker: &stubLinker{}, forkLinker: &stubLinker{}, resolver: &stubResolver{}, minter: &spyMinter{},
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
		c.SetSandboxPolicyCollector(sandbox.PolicyCollectorOptions{Hold: true})
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
	return h.sending(ctx, http.MethodGet, path, cred)
}

// sending is getting for a request of any method, with no body.
func (h *createHub) sending(ctx context.Context, method, path, cred string) <-chan answer {
	out := make(chan answer, 1)
	go func() {
		var a answer
		defer func() { out <- a }()
		req, err := http.NewRequestWithContext(ctx, method, h.URL("/api/v1"+path), nil)
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
// when set, is called before each EnsureProvider, CreateSandbox,
// StartSandbox, WaitReady and RunMain with the operation and the provider's
// or sandbox's name, and an error it returns is the call's; it is called
// after each StopSandbox and AddPolicyRule the runtime carried out, while
// its caller still waits for the answer. exec, when set, is asked before each
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
	// downloadErr, when set, ends every download after half its bytes.
	downloadErr error
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

// setDownloadErr makes every later download deliver half of the file's
// bytes and then fail with err; nil ends that.
func (r *runtimeHooks) setDownloadErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.downloadErr = err
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

func (c *hookedClient) StartSandbox(ctx context.Context, name string, spec runtime.StartSpec) error {
	if err := c.hooks.before(ctx, runtime.OpStartSandbox, name); err != nil {
		return err
	}
	return c.Client.StartSandbox(ctx, name, spec)
}

func (c *hookedClient) AddPolicyRule(ctx context.Context, name string, rule runtime.Rule) error {
	if err := c.Client.AddPolicyRule(ctx, name, rule); err != nil {
		return err
	}
	return c.hooks.before(ctx, runtime.OpAddPolicyRule, name)
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

func (c *hookedClient) StopSandbox(ctx context.Context, name string) error {
	if err := c.Client.StopSandbox(ctx, name); err != nil {
		return err
	}
	return c.hooks.before(ctx, runtime.OpStopSandbox, name)
}

func (c *hookedClient) Exec(ctx context.Context, name string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	c.hooks.record(runCall{sandbox: name, kind: "exec", argv: slices.Clone(req.Command), dir: req.WorkDir})
	if err := c.hooks.before(ctx, runtime.OpExec, name); err != nil {
		return runtime.ExecResult{}, err
	}
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

func (c *hookedClient) Download(ctx context.Context, name, p string) (io.ReadCloser, error) {
	if err := c.hooks.before(ctx, runtime.OpDownload, name); err != nil {
		return nil, err
	}
	rc, err := c.Client.Download(ctx, name, p)
	c.hooks.mu.Lock()
	cut := c.hooks.downloadErr
	c.hooks.mu.Unlock()
	if err != nil || cut == nil {
		return rc, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(io.MultiReader(bytes.NewReader(data[:len(data)/2]), iotest.ErrReader(cut))), nil
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
	// actorID is nil for an actor that is no user.
	actorID            *string
	metadata           map[string]any
	resourceType       string
	workspace, session *string
}

// sandboxEvents returns the hub events of type typ about the sandbox id,
// oldest first.
func (h *probeHub) sandboxEvents(t *testing.T, typ, id string) []hubEvent {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT actor_type, actor_id, metadata, resource_type, workspace, session_id FROM hub_events WHERE type = ? AND resource_id = ? ORDER BY seq`, typ, id)
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
		if err := rows.Scan(&e.actorType, &e.actorID, &meta, &e.resourceType, &e.workspace, &e.session); err != nil {
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

// leakyProfile has the policy file of full, without credentials; nohubProfile
// has no policy file and no hub access, and the credential model.
const (
	leakyProfile = "[[sandbox.profiles]]\nname = \"leaky\"\nttl = \"4h\"\npolicy = \"policy.yaml\"\n"
	nohubProfile = "[[sandbox.profiles]]\nname = \"nohub\"\nttl = \"4h\"\nhub_access = \"none\"\ncredentials = [\"model\"]\n"
)

// startPolicyWorld is startProvisionWorld with the [sandbox] settings given
// as TOML, the profiles leaky and nohub besides agent, full and plain, and
// no sandbox limit a test reaches.
func startPolicyWorld(t *testing.T, settings string) *createHub {
	t.Helper()
	h := startProvisionHub(t, settings+agentProfile+provisionProfile+plainProfile+leakyProfile+nohubProfile+credentialTables)
	h.world(t)
	h.AddMember(t, h.org, h.dana)
	h.putSecret(t, secret.ScopeOrg, h.org, "MODEL_KEY", modelSecret)
	h.putSecret(t, secret.ScopeOrg, h.org, "NPM_TOKEN", npmSecret)
	h.setLimits(limits{100, 100, 100, 100})
	return h
}

// saveRule stores the workspace sandbox rule body on the workspace ws, with
// the id id (a new one when ""), saved by dana at at, and returns it.
func (h *createHub) saveRule(t *testing.T, ws, id, body string, at time.Time) sandbox.SandboxRule {
	t.Helper()
	if id == "" {
		id = sandbox.NewRuleID()
	}
	r := sandbox.SandboxRule{
		ID: id, Workspace: ws, Rule: json.RawMessage(body), Summary: "allow " + body,
		Findings:  []runtime.Finding{{Severity: "low", Message: "a finding"}},
		CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"}, CreatedAt: at,
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.SaveRule(ctx, tx, r) })
	return r
}

// fillRules stores n workspace sandbox rules on the workspace ws in one
// transaction, each with its own rule {"host": "fill-<i>.<ws>.test"}, saved
// by dana an hour before recordStart.
func (h *createHub) fillRules(t *testing.T, ws string, n int) {
	t.Helper()
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		for i := range n {
			r := sandbox.SandboxRule{
				ID: sandbox.NewRuleID(), Workspace: ws, Rule: json.RawMessage(fmt.Sprintf(`{"host":"fill-%d.%s.test"}`, i, ws)),
				CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"}, CreatedAt: recordStart.Add(-time.Hour),
			}
			if err := sandbox.SaveRule(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

// ruleCount is how many workspace sandbox rules the workspace ws holds.
func (h *createHub) ruleCount(t *testing.T, ws string) int {
	t.Helper()
	n, err := sandbox.CountRules(context.Background(), h.Store.Main(), ws)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// ruleMap is the workspace sandbox rule object of r as a decoded answer
// holds it: its seven members, findings [] without findings, from_proposal
// null for a rule promoted from no proposal and created_at in RFC 3339,
// UTC, whole seconds.
func ruleMap(t *testing.T, r sandbox.SandboxRule) map[string]any {
	t.Helper()
	var rule any
	if err := json.Unmarshal(r.Rule, &rule); err != nil {
		t.Fatal(err)
	}
	findings := []any{}
	for _, f := range r.Findings {
		findings = append(findings, map[string]any{"severity": f.Severity, "message": f.Message})
	}
	var from any
	if o := r.FromProposal; o != nil {
		from = map[string]any{"sandbox_id": o.SandboxID, "proposal_id": o.ProposalID, "workspace": o.Workspace}
	}
	by := r.CreatedBy
	return map[string]any{
		"id": r.ID, "rule": rule, "summary": r.Summary, "findings": findings, "from_proposal": from,
		"created_by": map[string]any{"actor_type": by.ActorType, "actor_id": by.ActorID, "username": by.Username},
		"created_at": stamp(r.CreatedAt),
	}
}

// readyIn creates a sandbox on slug with body, waits until it is ready and
// returns its id.
func (h *createHub) readyIn(t *testing.T, slug string, body map[string]any) string {
	t.Helper()
	_, rec := h.provisioned(t, slug, "", body)
	if rec.State != sandbox.StateReady {
		t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
	}
	return rec.ID
}

// stopIn stops the ready sandbox id through the service, by the API.
func (h *createHub) stopIn(t *testing.T, id string) {
	t.Helper()
	if _, err := h.svc.Stop(context.Background(), storedRecord(t, h.Hub, id), sandbox.StopAPI, sandbox.SystemActor); err != nil {
		t.Fatal(err)
	}
}

// startAgain starts the sandbox id of the workspace slug with POST start,
// waits until it leaves provisioning and returns its record then.
func (h *createHub) startAgain(t *testing.T, slug, id string) sandbox.Record {
	t.Helper()
	if a := h.call(t, http.MethodPost, sandboxPath(slug, id)+"/start", h.key, nil); a.status != http.StatusOK {
		t.Fatalf("POST start = %d %s; want 200", a.status, a.body)
	}
	return h.settled(t, id)
}

// composed is the policy value the hub composes for the sandbox id as
// stored.
func (h *createHub) composed(t *testing.T, id string) runtime.Policy {
	t.Helper()
	p, err := h.svc.ComposePolicy(context.Background(), storedRecord(t, h.Hub, id))
	if err != nil {
		t.Fatalf("ComposePolicy: %v", err)
	}
	return p
}

// provisionFailure checks that the sandbox rec failed provision_failed with
// one fail event, and returns the event's error text.
func (h *createHub) provisionFailure(t *testing.T, rec sandbox.Record) string {
	t.Helper()
	if rec.State != sandbox.StateFailed || rec.StateReason != sandbox.ReasonProvisionFailed {
		t.Fatalf("sandbox = %s %q; want failed %s", rec.State, rec.StateReason, sandbox.ReasonProvisionFailed)
	}
	ev := h.sandboxEvents(t, sandbox.EventFail, rec.ID)
	if len(ev) != 1 {
		t.Fatalf("fail events %+v; want one", ev)
	}
	text, _ := ev[0].metadata["error"].(string)
	return text
}

// copyOf is the added rule of a recorded copy of the workspace sandbox rule
// r: the rule under its id.
func copyOf(r sandbox.SandboxRule) runtime.Rule { return runtime.Rule{Name: r.ID, Body: r.Rule} }

// approvedOf is the added rule of the approved rule a: the rule under its
// proposal's id.
func approvedOf(a sandbox.ApprovedRule) runtime.Rule {
	return runtime.Rule{Name: a.ProposalID, Body: a.Rule}
}

// saveApproved stores body as a rule approved for the sandbox id at at, by a
// proposal of a new id, and returns it.
func (h *createHub) saveApproved(t *testing.T, id, body string, at time.Time) sandbox.ApprovedRule {
	t.Helper()
	a := sandbox.ApprovedRule{SandboxID: id, ProposalID: "sbp_" + strings.TrimPrefix(sandbox.NewID(), "sbx_"), Rule: json.RawMessage(body), DecidedAt: at}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.SaveApprovedRule(ctx, tx, a) })
	return a
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
		c.SetSandboxPolicyCollector(sandbox.PolicyCollectorOptions{Hold: true})
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
// id. It expires hours after the real clock, the use hub's, so that the
// lifecycle loop does not delete it while the test runs.
func (h *useHub) seed(t *testing.T, ws, state string, ha *sandbox.HubAccess) string {
	t.Helper()
	before := recordStart.Add(-time.Hour)
	rec := sandbox.Record{
		ID: sandbox.NewID(), Workspace: ws, Profile: "agent", State: state, HubAccess: ha,
		ExpiresAt: time.Now().Add(3 * time.Hour), LastActivityAt: before,
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
	calls, runtimeCalls := h.calls.Load(), requestCalls(h.rt)
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
		if n := requestCalls(h.rt) - runtimeCalls; n != 0 {
			t.Errorf("the runtime got %d calls, want none", n)
		}
	}
	return a
}

// requestCalls is the number of calls the runtime rt received but the log
// collector's: it reads the logs of every ready sandbox in the background,
// which no request asks for.
func requestCalls(rt *runtimetest.Fake) int {
	n := 0
	for _, c := range rt.Calls() {
		if c.Op != runtime.OpLogs {
			n++
		}
	}
	return n
}

// execHub is a hub with sandboxes on over a fake runtime whose commands
// cmds runs, on a clock the test advances, with the repository ws of owner,
// whose API key is key.
type execHub struct {
	*probeHub
	svc   *sandbox.Service
	clock *hubtest.JobClock
	// timers keeps the timers the sandbox service asked the clock for.
	timers *timerLog
	cmds   *commands
	// terms records the terminals the hub opens.
	terms *terminals
	// quirks makes the hub's runtime client answer as a runtime other
	// than the fake may.
	quirks *runtimeQuirks
	key    string
}

// startExecHub starts an exec hub that runs at most maxExecs commands at
// once in a sandbox, its clock at recordStart; [sandbox].max_exec_timeout
// is its default, 30m.
func startExecHub(t *testing.T, maxExecs int) *execHub {
	t.Helper()
	return startUseSandboxHub(t, "max_concurrent_execs = "+strconv.Itoa(maxExecs)+"\n")
}

// startUseSandboxHub starts an exec hub whose [sandbox] section also holds
// the lines settings.
func startUseSandboxHub(t *testing.T, settings string) *execHub {
	t.Helper()
	return newExecHub(t, settings, nil)
}

// newExecHub starts an exec hub whose [sandbox] section also holds the lines
// settings, with its configuration as edit changes it when edit is not nil.
// The fake runtime stamps log entries with the hub's clock, and the hub's
// runtime client records the terminals it opens in terms. The hub runs no
// policy collector, whose streams would stand beside those its tests count.
func newExecHub(t *testing.T, settings string, edit func(*hubtest.Config)) *execHub {
	t.Helper()
	clock := hubtest.NewJobClock(func() time.Time { return recordStart })
	h := &execHub{
		probeHub: &probeHub{rt: runtimetest.New(t, runtimetest.WithClock(clock.Now))}, clock: clock, timers: &timerLog{},
		cmds: &commands{scripts: map[string]runtimetest.ExecFunc{}}, terms: &terminals{}, quirks: &runtimeQuirks{},
	}
	h.rt.SetExec(h.cmds.run)
	cfg := sandboxesOn
	cfg.TOML = "[sandbox]\ngateway_url = \"https://gw.test:8443\"\ntoken_file = \"gateway-token\"\n" +
		settings + "[[sandbox.profiles]]\nname = \"agent\"\nttl = \"4h\"\n"
	cfg.Now = h.clock.Now
	cfg.Compose = func(c *hub.Composition) {
		h.svc = c.Sandbox()
		c.SetSandboxPolicyCollector(sandbox.PolicyCollectorOptions{Hold: true})
		c.SetSandboxRuntime(func(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
			terms := &terminalClient{Client: h.rt.Client(s.OpenShellWorkspace, deps.Recorder), terms: h.terms}
			return &quirkClient{Client: terms, quirks: h.quirks}, nil
		})
		c.AdjustSandbox(func(d *sandbox.Deps) {
			d.Now = h.clock.Now
			d.NewTimer = func(x time.Duration) sandbox.Timer { return h.timers.track(x, h.clock.NewTimer(x)) }
		})
	}
	if edit != nil {
		edit(&cfg)
	}
	h.Hub = hubtest.StartWith(t, cfg)
	h.owner = h.CreateUser(t, "owner")
	h.AddWorkspace(t, access.Record{Slug: "ws", OwnerID: h.owner, OrgID: h.CreateOrg(t, "team"), Kind: access.KindRepository, Status: access.StatusReady})
	h.key = h.IssueKey(t, h.owner)
	return h
}

// sandbox creates a sandbox in the runtime with the working directory
// /workspace and the environment PARLEY_SANDBOX_ID and MODE=profile, then
// stores it as a ready sandbox of ws created by the owner's API key, as
// edit changes it when edit is not nil, and returns its id.
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
	// The runtime holds the sandbox before its record is stored, as it does
	// when the hub creates one: a reconciliation pass that read a ready
	// record the runtime does not hold yet would mark it failed.
	ctx := context.Background()
	spec := runtime.SandboxSpec{Name: rec.ID, WorkDir: sandbox.WorkDir, Env: map[string]string{"PARLEY_SANDBOX_ID": rec.ID, "MODE": "profile"}}
	if _, err := h.rt.Client("parley", nil).CreateSandbox(ctx, spec); err != nil {
		t.Fatalf("create the sandbox in the runtime: %v", err)
	}
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return sandbox.Insert(ctx, tx, rec) }); err != nil {
		t.Fatalf("insert a sandbox: %v", err)
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
	return h.execAs(t, h.key, id, body, header)
}

// execAs sends what exec sends with cred as the bearer credential.
func (h *execHub) execAs(t *testing.T, cred, id string, body any, header http.Header) answer {
	t.Helper()
	var a answer
	a.status, a.header, a.body = h.Exchange(t, http.MethodPost, "/api/v1"+sandboxPath("ws", id)+"/exec", cred, body, header)
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

// frame is one server-sent event of a stream: its name and its data.
type frame struct {
	event string
	data  map[string]any
}

// output is the bytes of output a stdout or stderr frame carries: its data
// as text, or its data_base64 decoded.
func (f frame) output(t *testing.T) []byte {
	t.Helper()
	if encoded, ok := f.data["data_base64"].(string); ok {
		b, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("the %s frame's data_base64 is not base64: %v", f.event, err)
		}
		return b
	}
	text, ok := f.data["data"].(string)
	if !ok {
		t.Fatalf("the %s frame %v has neither data nor data_base64", f.event, f.data)
	}
	return []byte(text)
}

// frameReader reads the server-sent events of an answer one frame at a
// time.
type frameReader struct {
	resp  *http.Response
	lines *bufio.Scanner
}

// streaming sends POST …/exec for the sandbox id of ws as the owner with
// body and Accept: text/event-stream, under ctx and at most 10 s, and
// returns the answer's stream once its headers arrived; the test closes it.
func (h *execHub) streaming(t *testing.T, ctx context.Context, id string, body any) *frameReader {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL("/api/v1"+sandboxPath("ws", id)+"/exec"), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return h.eventStream(t, req, "exec")
}

// eventStream sends req as the owner and returns the answer's stream once
// its headers arrived, failing unless it is 200 and an event stream; what
// names the request in a failure.
func (h *execHub) eventStream(t *testing.T, req *http.Request, what string) *frameReader {
	t.Helper()
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s = %d %.300s (Content-Type %q), want 200 and an event stream", what, resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(nil, 1<<20)
	return &frameReader{resp: resp, lines: lines}
}

// next reads the next frame, and reports false once the stream ended.
func (s *frameReader) next(t *testing.T) (frame, bool) {
	t.Helper()
	var f frame
	for s.lines.Scan() {
		line := s.lines.Text()
		switch {
		case line == "":
			if f.event != "" {
				return f, true
			}
		case strings.HasPrefix(line, "event: "):
			f.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f.data); err != nil {
				t.Fatalf("the data of a %s frame is not a JSON object: %v", f.event, err)
			}
		}
	}
	if err := s.lines.Err(); err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if f.event != "" {
		t.Fatalf("the stream ended inside a %s frame", f.event)
	}
	return frame{}, false
}

// rest reads the frames up to the end of the stream.
func (s *frameReader) rest(t *testing.T) []frame {
	t.Helper()
	var out []frame
	for f, ok := s.next(t); ok; f, ok = s.next(t) {
		out = append(out, f)
	}
	return out
}

// startFilesHub starts an exec hub whose [sandbox].max_transfer_size is
// 1 MiB, with a ready sandbox, whose id it returns.
func startFilesHub(t *testing.T) (*execHub, string) {
	t.Helper()
	h := startUseSandboxHub(t, "max_transfer_size = \"1mb\"\n")
	return h, h.sandbox(t, nil)
}

// files sends method …/files?query for the sandbox id of ws as the owner,
// with contentType ("" for none) and body (nil for none), and reads the
// error envelope of the answer, if it has one. A body that is not a
// *bytes.Reader, *bytes.Buffer or *strings.Reader is sent without a
// declared length.
func (h *execHub) files(t *testing.T, method, id, query, contentType string, body io.Reader) answer {
	t.Helper()
	return h.filesWith(t, context.Background(), method, id, query, contentType, body)
}

// filesWith is files under ctx.
func (h *execHub) filesWith(t *testing.T, ctx context.Context, method, id, query, contentType string, body io.Reader) answer {
	t.Helper()
	a, err := h.exchangeFiles(t, ctx, method, id, query, contentType, body)
	if err != nil {
		t.Fatalf("%s files?%s: read the answer: %v", method, query, err)
	}
	return a
}

// download sends GET …/files?query for the sandbox id of ws as the owner
// and returns the answer with what of its body arrived, and the error that
// ended the read of the body, nil for a clean end.
func (h *execHub) download(t *testing.T, id, query string) (answer, error) {
	t.Helper()
	return h.exchangeFiles(t, context.Background(), http.MethodGet, id, query, "", nil)
}

// mustDownload is download for an answer whose body must end cleanly.
func (h *execHub) mustDownload(t *testing.T, id, query string) answer {
	t.Helper()
	return h.files(t, http.MethodGet, id, query, "", nil)
}

// exchangeFiles is filesWith, which returns the error that ended the read
// of the answer's body instead of failing on it.
func (h *execHub) exchangeFiles(t *testing.T, ctx context.Context, method, id, query, contentType string, body io.Reader) (answer, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, h.URL("/api/v1"+sandboxPath("ws", id)+"/files?"+query), body)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s files?%s: %v", method, query, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var a answer
	a.status, a.header = resp.StatusCode, resp.Header
	a.body, err = io.ReadAll(resp.Body)
	var env struct {
		Error *json.RawMessage `json:"error"`
	}
	if json.Unmarshal(a.body, &env) == nil && env.Error != nil {
		_ = json.Unmarshal(*env.Error, &a.err)
	}
	return a, err
}

// put writes entries into the sandbox id through the runtime, each name an
// absolute path: a directory when it ends in a slash, else a file holding
// its body with its mode, 0644 when it has none.
func (h *execHub) put(t *testing.T, id string, entries ...tarEntry) {
	t.Helper()
	ctx, client := context.Background(), h.rt.Client("parley", nil)
	for _, e := range entries {
		mode := fs.FileMode(e.mode)
		var err error
		if strings.HasSuffix(e.name, "/") {
			if mode == 0 {
				mode = 0o755
			}
			err = client.MakeDir(ctx, id, strings.TrimSuffix(e.name, "/"), mode)
		} else {
			if mode == 0 {
				mode = 0o644
			}
			err = client.Upload(ctx, id, e.name, mode, strings.NewReader(e.body))
		}
		if err != nil {
			t.Fatalf("put %s into the sandbox: %v", e.name, err)
		}
	}
}

// fileCalls counts the file operations the runtime received.
func (h *execHub) fileCalls() int {
	n := 0
	for _, c := range h.rt.Calls() {
		switch c.Op {
		case runtime.OpUpload, runtime.OpMakeDir, runtime.OpStat, runtime.OpDownload, runtime.OpWalk:
			n++
		}
	}
	return n
}

// file reads the file at p of the sandbox id from the runtime, and reports
// false when there is none; it fails on a directory.
func (h *execHub) file(t *testing.T, id, p string) (data []byte, mode fs.FileMode, ok bool) {
	t.Helper()
	ctx, client := context.Background(), h.rt.Client("parley", nil)
	fi, err := client.Stat(ctx, id, p)
	if errors.Is(err, runtime.ErrNotFound) {
		return nil, 0, false
	}
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if fi.Dir {
		t.Fatalf("%s is a directory, want a file", p)
	}
	rc, err := client.Download(ctx, id, p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	defer func() { _ = rc.Close() }()
	if data, err = io.ReadAll(rc); err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return data, fi.Mode, true
}

// tarEntry is an entry of an archive tarOf writes: a directory when name
// ends in a slash, a symbolic link to link, a hard link to hardLink, else a
// regular file holding body.
type tarEntry struct {
	name           string
	mode           int64
	body           string
	link, hardLink string
}

// tarOf is the tar stream of entries, in order.
func tarOf(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: tar.TypeReg, Size: int64(len(e.body))}
		switch {
		case strings.HasSuffix(e.name, "/"):
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		case e.link != "":
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, e.link, 0
		case e.hardLink != "":
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, e.hardLink, 0
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatalf("write the tar header of %s: %v", e.name, err)
		}
		if _, err := io.WriteString(w, e.body); err != nil && hdr.Size > 0 {
			t.Fatalf("write the tar body of %s: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close the tar stream: %v", err)
	}
	return b.Bytes()
}

// gatedBody is a body sent without a declared length that gives the bytes
// of r up to limit of them, then waits until ctx ends and fails: a hub that
// answers before ctx ends has read at most limit bytes.
type gatedBody struct {
	ctx   context.Context
	r     io.Reader
	limit int64
	taken atomic.Int64
}

func (b *gatedBody) Read(p []byte) (int, error) {
	left := b.limit - b.taken.Load()
	if left <= 0 {
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err := b.r.Read(p)
	b.taken.Add(int64(n))
	return n, err
}

// logHub is an exec hub whose test has the log collector run its rounds on
// the hub's clock and reads the stored log lines; passes counts the
// maintenance passes that have ended.
type logHub struct {
	*execHub
	passes atomic.Int64
}

// startLogHub starts a log hub. With passInterval above zero the hub runs a
// maintenance pass at that interval, on the real clock.
func startLogHub(t *testing.T, passInterval time.Duration) *logHub {
	t.Helper()
	h := &logHub{}
	h.execHub = newExecHub(t, "", func(cfg *hubtest.Config) {
		cfg.MaintenanceInterval = passInterval
		compose := cfg.Compose
		cfg.Compose = func(c *hub.Composition) {
			compose(c)
			// A task under a name the maintenance order does not know runs
			// after those it knows.
			c.AddMaintenanceTask("log_test_pass", func(context.Context, *hub.Runtime) error {
				h.passes.Add(1)
				return nil
			})
		}
	})
	return h
}

// round has the log collector run a round: it waits until the collector
// waits for one, then moves the hub's clock on to it. The collector asks
// for the timer of its next round before it runs one, so a test that saw
// what a round did may ask for the next.
func (h *logHub) round(t *testing.T) {
	t.Helper()
	hubtest.WaitClosed(t, h.clock.Armed(sandbox.LogCollectInterval), "the log collector to wait for its next round")
	h.clock.Advance(sandbox.LogCollectInterval)
}

// follow has the log collector run a round and waits until it reads both
// logs of the sandbox id.
func (h *logHub) follow(t *testing.T, id string) {
	t.Helper()
	h.round(t)
	eventually(t, "the collector to read both logs of "+id, func() bool { return h.logStreams(id) == 2 })
}

// logStreams is the number of open log streams of the sandbox id.
func (h *execHub) logStreams(id string) int {
	n := 0
	for _, s := range h.rt.Streams() {
		if s.Op == runtime.OpLogs && s.Sandbox == id {
			n++
		}
	}
	return n
}

// breakLog breaks the first open log stream of the sandbox id.
func (h *execHub) breakLog(t *testing.T, id string) {
	t.Helper()
	for _, s := range h.rt.Streams() {
		if s.Op == runtime.OpLogs && s.Sandbox == id {
			h.rt.BreakStream(s.ID)
			return
		}
	}
	t.Fatalf("no open log stream of %s", id)
}

// lines are the stored log lines of the sandbox id of the source ("" for
// both), in the order they were stored.
func (h *logHub) lines(t *testing.T, id, source string) []sandbox.LogLine {
	t.Helper()
	lines, err := sandbox.LogLines(context.Background(), h.Store.Main(), id, sandbox.LogSelection{Source: source})
	if err != nil {
		t.Fatalf("read the log lines of %s: %v", id, err)
	}
	return lines
}

// texts are the line members of the stored log lines of the sandbox id.
func (h *logHub) texts(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, l := range h.lines(t, id, "") {
		out = append(out, l.Line)
	}
	return out
}

// emit has the main process of the sandbox id write text to stream.
func (h *logHub) emit(id string, stream runtime.OutputStream, text string) {
	h.rt.EmitOutput("parley", id, stream, text)
}

// pass waits for a maintenance pass that started after the call.
func (h *logHub) pass(t *testing.T) {
	t.Helper()
	from := h.passes.Load()
	eventually(t, "a maintenance pass", func() bool { return h.passes.Load() >= from+2 })
}

// logsAs sends GET …/logs?query for the sandbox id of ws with cred as the
// bearer credential, and reads the answer with its headers and its error
// envelope, if it has one.
func (h *execHub) logsAs(t *testing.T, cred, id, query string) answer {
	t.Helper()
	var a answer
	a.status, a.header, a.body = h.Exchange(t, http.MethodGet, "/api/v1"+sandboxPath("ws", id)+"/logs?"+query, cred, nil, nil)
	var env struct {
		Error *json.RawMessage `json:"error"`
	}
	if json.Unmarshal(a.body, &env) == nil && env.Error != nil {
		_ = json.Unmarshal(*env.Error, &a.err)
	}
	return a
}

// logPage reads the page of log lines the answer a holds, which must be
// 200 with an items array: the line member of each item, in order, and
// next_cursor, "" for null.
func logPage(t *testing.T, a answer) (lines []string, next string) {
	t.Helper()
	var page struct {
		Items      *[]struct{ Line string } `json:"items"`
		NextCursor *string                  `json:"next_cursor"`
	}
	if a.status != http.StatusOK || json.Unmarshal(a.body, &page) != nil || page.Items == nil {
		t.Fatalf("logs = %d %.300s, want 200 with an items array", a.status, a.body)
	}
	for _, item := range *page.Items {
		lines = append(lines, item.Line)
	}
	if page.NextCursor != nil {
		next = *page.NextCursor
	}
	return lines, next
}

// followLogs sends GET …/logs?query for the sandbox id of ws as the owner,
// under ctx and at most 10 s, and returns the answer's event stream once
// its headers arrived; the test closes it.
func (h *execHub) followLogs(t *testing.T, ctx context.Context, id, query string) *frameReader {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL("/api/v1"+sandboxPath("ws", id)+"/logs?"+query), nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	return h.eventStream(t, req, "logs")
}

// storeLines stores lines as log lines of the sandbox id, as the collector
// does, but without telling the followed streams.
func (h *execHub) storeLines(t *testing.T, id string, lines ...sandbox.LogLine) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return sandbox.InsertLogLines(ctx, tx, id, lines) }); err != nil {
		t.Fatalf("store log lines of %s: %v", id, err)
	}
}

// processLine is a line of the main process's stream at the second sec
// after recordStart.
func processLine(sec int, stream, line string) sandbox.LogLine {
	return sandbox.LogLine{Time: recordStart.Add(time.Duration(sec) * time.Second), Source: sandbox.LogSourceProcess, Stream: stream, Line: line}
}

// policyLine is a policy entry that denies a connection to example.com:443
// at the second sec after recordStart.
func policyLine(sec int, line string) sandbox.LogLine {
	return sandbox.LogLine{
		Time: recordStart.Add(time.Duration(sec) * time.Second), Source: sandbox.LogSourcePolicy, Line: line,
		Action: "deny", Program: "/usr/bin/curl", Host: "example.com", Port: 443,
	}
}

// seedLines stores the lines p1 (stdout), d1 (a policy entry), p2 (stderr)
// and p3 (stdout) of the sandbox id, in this order, a second apart from
// recordStart.
func (h *execHub) seedLines(t *testing.T, id string) {
	t.Helper()
	h.storeLines(t, id, processLine(0, "stdout", "p1"), policyLine(1, "d1"), processLine(2, "stderr", "p2"), processLine(3, "stdout", "p3"))
}

// terminals keeps the terminals a hub's runtime client opened, in order.
type terminals struct {
	mu   sync.Mutex
	list []*openedTerminal
}

// of is the terminals opened in the sandbox id, in order.
func (ts *terminals) of(id string) []*openedTerminal {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []*openedTerminal
	for _, o := range ts.list {
		if o.sandbox == id {
			out = append(out, o)
		}
	}
	return out
}

// count is the number of terminals opened in any sandbox.
func (ts *terminals) count() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.list)
}

// last is the terminal opened last in the sandbox id.
func (ts *terminals) last(t *testing.T, id string) *openedTerminal {
	t.Helper()
	list := ts.of(id)
	if len(list) == 0 {
		t.Fatalf("no terminal was opened in %s", id)
	}
	return list[len(list)-1]
}

// openedTerminal is a terminal of the fake runtime, which echoes its input,
// with what the hub asked for when it opened it, the resizes and the close
// the hub asked for, and the controls a test steers it with: write, exit and
// fail.
type openedTerminal struct {
	runtime.Terminal
	sandbox string
	req     runtime.ExecRequest
	size    runtime.TerminalSize

	mu      sync.Mutex
	resizes []runtime.TerminalSize
	closed  bool
	failure error
}

func (o *openedTerminal) Resize(size runtime.TerminalSize) error {
	o.mu.Lock()
	o.resizes = append(o.resizes, size)
	o.mu.Unlock()
	return o.Terminal.Resize(size)
}

func (o *openedTerminal) Close() error {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	return o.Terminal.Close()
}

// Read is the fake's, except that a terminal fail ended reports its
// failure after what it had written.
func (o *openedTerminal) Read(b []byte) (int, error) {
	n, err := o.Terminal.Read(b)
	if errors.Is(err, io.EOF) {
		o.mu.Lock()
		if o.failure != nil {
			err = o.failure
		}
		o.mu.Unlock()
	}
	return n, err
}

// write makes the terminal write text, as the shell's output: the fake's
// terminal gives back what it is written.
func (o *openedTerminal) write(t *testing.T, text string) {
	t.Helper()
	if _, err := o.Terminal.Write([]byte(text)); err != nil {
		t.Fatalf("write %q as the terminal's output: %v", text, err)
	}
}

// exit ends the shell, as when it exits.
func (o *openedTerminal) exit() { _ = o.Terminal.Close() }

// fail breaks the terminal: its output ends with a runtime failure.
func (o *openedTerminal) fail() {
	o.mu.Lock()
	o.failure = runtime.NewError(runtime.ErrUnavailable, runtime.OpExecInteractive, "the terminal broke")
	o.mu.Unlock()
	_ = o.Terminal.Close()
}

// resized is the sizes the hub resized the terminal to, in order.
func (o *openedTerminal) resized() []runtime.TerminalSize {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.resizes)
}

// closedByHub reports whether the hub closed the terminal.
func (o *openedTerminal) closedByHub() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}

// terminalClient is a runtime client in front of a fake's that records the
// terminals it opens in terms.
type terminalClient struct {
	runtime.Client
	terms *terminals
}

func (c *terminalClient) ExecInteractive(ctx context.Context, name string, req runtime.ExecRequest, size runtime.TerminalSize) (runtime.Terminal, error) {
	term, err := c.Client.ExecInteractive(ctx, name, req, size)
	if err != nil {
		return nil, err
	}
	o := &openedTerminal{Terminal: term, sandbox: name, req: req, size: size}
	c.terms.mu.Lock()
	c.terms.list = append(c.terms.list, o)
	c.terms.mu.Unlock()
	return o, nil
}

// runtimeQuirks are answers a runtime may give that the fake does not.
type runtimeQuirks struct {
	// killedCode reports the exit code of a killed command as 137 without
	// an error.
	killedCode atomic.Bool
	// canonical has Stat report a path under /srv/sandbox, and Walk add
	// directories outside the directory it was asked for.
	canonical atomic.Bool
}

// quirkClient is a runtime client that answers with the quirks set.
type quirkClient struct {
	runtime.Client
	quirks *runtimeQuirks
}

func (c *quirkClient) ExecStream(ctx context.Context, name string, req runtime.ExecRequest) (runtime.ExecStream, error) {
	s, err := c.Client.ExecStream(ctx, name, req)
	if err != nil || !c.quirks.killedCode.Load() {
		return s, err
	}
	return &killedCodeStream{ExecStream: s, ctx: ctx}, nil
}

// killedCodeStream is an exec stream whose command, once killed through
// ctx, reports exit code 137 without an error.
type killedCodeStream struct {
	runtime.ExecStream
	ctx context.Context
}

func (s *killedCodeStream) ExitCode() (int, error) {
	code, err := s.ExecStream.ExitCode()
	if err != nil && s.ctx.Err() != nil {
		return 137, nil
	}
	return code, err
}

func (c *quirkClient) Stat(ctx context.Context, name, p string) (runtime.FileInfo, error) {
	fi, err := c.Client.Stat(ctx, name, p)
	if err == nil && c.quirks.canonical.Load() {
		fi.Path = "/srv/sandbox" + fi.Path
	}
	return fi, err
}

func (c *quirkClient) Walk(ctx context.Context, name, dir string) ([]runtime.FileInfo, error) {
	out, err := c.Client.Walk(ctx, name, dir)
	if err == nil && c.quirks.canonical.Load() {
		for _, p := range []string{"/srv/sandbox" + dir + "/canonical", dir + "-sibling", "/etc", dir} {
			out = append(out, runtime.FileInfo{Path: p, Dir: true, Mode: fs.ModeDir | 0o755})
		}
	}
	return out, err
}

// openTerminals is the number of terminals of the sandbox id the runtime rt
// has open.
func openTerminals(rt *runtimetest.Fake, id string) int {
	n := 0
	for _, s := range rt.Streams() {
		if s.Op == runtime.OpExecInteractive && s.Sandbox == id {
			n++
		}
	}
	return n
}

// attach opens GET …/attach?query of the sandbox id of ws as a WebSocket,
// as the owner and without an Origin header, and closes it when the test
// ends.
func (h *execHub) attach(t *testing.T, id, query string) *websocket.Conn {
	t.Helper()
	target := h.URL("/api/v1" + sandboxPath("ws", id) + "/attach?" + query)
	cfg, err := websocket.NewConfig("ws"+strings.TrimPrefix(target, "http"), "http://origin.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Header.Set("Authorization", "Bearer "+h.key)
	conn, err := net.Dial("tcp", cfg.Location.Host)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	ws, err := websocket.NewClient(cfg, &withoutOrigin{Conn: conn})
	if err != nil {
		_ = conn.Close()
		t.Fatalf("attach to %s: %v", id, err)
	}
	_ = conn.SetDeadline(time.Time{})
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// withoutOrigin is a connection that leaves the Origin header out of the
// opening handshake it writes first, which the WebSocket client always
// sends.
type withoutOrigin struct {
	net.Conn
	sent bool
}

func (c *withoutOrigin) Write(b []byte) (int, error) {
	if c.sent {
		return c.Conn.Write(b)
	}
	c.sent = true
	start := bytes.Index(b, []byte("\r\nOrigin: "))
	if start < 0 {
		return 0, errors.New("the handshake holds no Origin header to leave out")
	}
	end := start + 2 + bytes.Index(b[start+2:], []byte("\r\n"))
	if _, err := c.Conn.Write(append(slices.Clone(b[:start]), b[end:]...)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// wsFrame is a WebSocket message: its payload and whether it came as a
// binary frame.
type wsFrame struct {
	data   []byte
	binary bool
}

// frames receives WebSocket messages with their frame type.
var frames = websocket.Codec{
	Marshal: websocket.Message.Marshal,
	Unmarshal: func(data []byte, payloadType byte, v any) error {
		*v.(*wsFrame) = wsFrame{data: data, binary: payloadType == websocket.BinaryFrame}
		return nil
	},
}

// receiveWithin receives the next message of ws, waiting at most d.
func receiveWithin(ws *websocket.Conn, d time.Duration) (wsFrame, error) {
	_ = ws.SetReadDeadline(time.Now().Add(d))
	var f wsFrame
	err := frames.Receive(ws, &f)
	return f, err
}

// receiveBytes receives binary messages from ws until they hold n bytes,
// and returns them joined.
func receiveBytes(t *testing.T, ws *websocket.Conn, n int) []byte {
	t.Helper()
	var got []byte
	for len(got) < n {
		f, err := receiveWithin(ws, 5*time.Second)
		if err != nil {
			t.Fatalf("receive after %q: %v", got, err)
		}
		if !f.binary {
			t.Fatalf("got the text frame %q, want binary frames", f.data)
		}
		got = append(got, f.data...)
	}
	return got
}

// sendFrame sends v on ws: a string as a text frame, bytes as a binary one.
func sendFrame(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	if err := websocket.Message.Send(ws, v); err != nil {
		t.Fatalf("send %q: %v", v, err)
	}
}

// within waits up to d, polling, for cond to hold, and fails the test then.
func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", d, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// observed keeps the operation records an observer is given, in order.
type observed struct {
	mu   sync.Mutex
	list []sandbox.Operation
}

func (o *observed) observe(_ context.Context, op sandbox.Operation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.list = append(o.list, op)
	return nil
}

// records is the records given so far.
func (o *observed) records() []sandbox.Operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.list)
}

// lastActivity is the stored last_activity_at of the sandbox id.
func (h *execHub) lastActivity(t *testing.T, id string) time.Time {
	t.Helper()
	return storedRecord(t, h.Hub, id).LastActivityAt
}

// setLastActivity stores at as the last_activity_at of the sandbox id.
func (h *execHub) setLastActivity(t *testing.T, id string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return sandbox.Update(ctx, tx, id, sandbox.SetLastActivityAt(at)) }); err != nil {
		t.Fatalf("set the last activity of %s: %v", id, err)
	}
}

// activityAt waits for the stored last_activity_at of the sandbox id to be
// want, and fails the test with the value it holds then.
func (h *execHub) activityAt(t *testing.T, id string, want time.Time, when string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for got := h.lastActivity(t, id); !got.Equal(want); got = h.lastActivity(t, id) {
		if time.Now().After(deadline) {
			t.Fatalf("%s last_activity_at = %v, want %v", when, got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// noHostGit is the environment that keeps the host's git configuration out
// of a git command: the commands of a checkpoint hub's sandboxes and those
// a test runs in a working directory.
var noHostGit = map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"}

// startCheckpointHub starts a create hub with the profile agent and the
// create world, whose fake runtime keeps each sandbox's files in a
// directory of the host and runs the commands of its sandboxes there with
// the real git, with the dependencies of its stored sandboxes changed by
// adjust.
func startCheckpointHub(t *testing.T, adjust ...func(*sandbox.Deps)) *createHub {
	t.Helper()
	return startCheckpointHubWith(t, "", adjust...)
}

// startCheckpointHubWith is startCheckpointHub with the [sandbox] keys
// settings, as TOML, before the profile.
func startCheckpointHubWith(t *testing.T, settings string, adjust ...func(*sandbox.Deps)) *createHub {
	t.Helper()
	h := newCreateHub(t, runtimetest.New(t, runtimetest.WithHostDirs()), settings+agentProfile, false, adjust)
	h.rt.SetExec(nil)
	h.world(t)
	return h
}

// workSandbox stores a ready sandbox of demo--t1 created by dana with the
// profile agent's hub credential and checkpoint flag and the checkout of
// the work branch t1, as edit changes it when edit is not nil, creates it
// at the runtime, and returns its id and the host directory that holds its
// working directory, which is empty.
func (h *createHub) workSandbox(t *testing.T, edit func(*sandbox.Record)) (id, dir string) {
	t.Helper()
	id = h.seedRecord(t, "demo--t1", sandbox.StateReady, func(r *sandbox.Record) {
		r.SessionID = new(t1Session)
		r.Checkout = &sandbox.Checkout{Path: sandbox.WorkDir, Ref: "main", SHA: h.base, Branch: new("t1")}
		r.HubAccess = &sandbox.HubAccess{Scopes: allHubScopes, ExpiresAt: r.ExpiresAt}
		r.Snapshot = sandbox.Snapshot{TTL: 4 * time.Hour, IdleTimeout: 30 * time.Minute, Checkpoint: true, HubAccess: "fork"}
		if edit != nil {
			edit(r)
		}
	})
	spec := runtime.SandboxSpec{Name: id, WorkDir: sandbox.WorkDir, Env: noHostGit, Labels: map[string]string{sandbox.LabelSandbox: id}}
	if _, err := h.rt.Client("parley", nil).CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("create the sandbox at the runtime: %v", err)
	}
	return id, h.workDir(id)
}

// workDir is the host directory that holds the working directory of the
// sandbox id.
func (h *createHub) workDir(id string) string {
	return filepath.Join(h.rt.HostDir("parley", id), strings.TrimPrefix(sandbox.WorkDir, "/"))
}

// cloneWork clones demo into the empty directory dir and checks out the
// new branch t1 at main, as the checkout of a sandbox of demo--t1 does.
func (h *createHub) cloneWork(t *testing.T, dir string) {
	t.Helper()
	workGit(t, dir, "clone", "--quiet", h.workspaces.RepoPath("demo"), ".")
	workGit(t, dir, "checkout", "--quiet", "-b", "t1")
}

// workGit runs git in the working directory dir without the host's
// configuration, with a fixed identity, and returns its output, trimmed.
func workGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(workOutput(t, dir, args...))
}

// workStatus is what git status --porcelain lists in the working directory
// dir, sorted.
func workStatus(t *testing.T, dir string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(workOutput(t, dir, "status", "--porcelain"), "\n"), "\n")
	slices.Sort(lines)
	return slices.DeleteFunc(lines, func(l string) bool { return l == "" })
}

// workOutput is workGit's output as git wrote it.
func workOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dana", "GIT_AUTHOR_EMAIL=dana@example.invalid",
		"GIT_COMMITTER_NAME=Dana", "GIT_COMMITTER_EMAIL=dana@example.invalid")
	for k, v := range noHostGit {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// commitWork writes the files of files into the working directory dir and
// commits them on its current branch, returning the new head.
func commitWork(t *testing.T, dir, msg string, files map[string]string) string {
	t.Helper()
	writeWork(t, dir, files)
	workGit(t, dir, "add", "--all")
	workGit(t, dir, "commit", "--quiet", "-m", msg)
	return workGit(t, dir, "rev-parse", "HEAD")
}

// writeWork writes the files of files, by path, into the working directory
// dir.
func writeWork(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// workState is what a checkpoint must leave as it was in the working
// directory dir: every ref with its value, the status and the index's
// entries.
func workState(t *testing.T, dir string) string {
	t.Helper()
	return workGit(t, dir, "for-each-ref") + "\n--\n" + workGit(t, dir, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all") +
		"\n--\n" + workGit(t, dir, "ls-files", "--stage")
}

// checkpointRefs maps the branch of each checkpoint ref of the sandbox id in
// the repository of demo--t1 to its value.
func (h *createHub) checkpointRefs(t *testing.T, id string) map[string]string {
	t.Helper()
	return gitrepotest.Refs(t, h.workspaces.RepoPath("demo--t1"), sandbox.CheckpointRefPrefix+id+"/")
}

// storedCheckpoint is the checkpoint stored on the sandbox id; nil for
// none.
func (h *createHub) storedCheckpoint(t *testing.T, id string) *sandbox.CheckpointState {
	t.Helper()
	raw := storedRecord(t, h.Hub, id).Checkpoint
	if raw == nil {
		return nil
	}
	var cp sandbox.CheckpointState
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatalf("the stored checkpoint %s: %v", raw, err)
	}
	return &cp
}

// checkpointMeta is the metadata of the checkpoint event the checkpoint cp
// is written with, as a decoded event holds it.
func checkpointMeta(t *testing.T, cp *sandbox.CheckpointState) map[string]any {
	t.Helper()
	b, err := json.Marshal(map[string]any{"ok": cp.OK, "refs": cp.Refs, "error": cp.Error})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// metricDelta is what was added to each series between the values before
// and after of one metric.
func metricDelta(before, after map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			out[k] = d
		}
	}
	return out
}

// hubCredential mints a live hub credential for the stored sandbox id,
// which has hub access, through the hub's minting, as a create or a start
// does: it stores the token's id on the record, gives the service its
// value, and returns the value.
func (h *createHub) hubCredential(t *testing.T, id string) string {
	t.Helper()
	rec := storedRecord(t, h.Hub, id)
	var value string
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		pat, v, err := h.svc.MintHubCredential(ctx, tx, rec, rec.ExpiresAt)
		if err != nil {
			return err
		}
		value = v
		return sandbox.Update(ctx, tx, id, sandbox.SetHubTokenID(pat.TokenID))
	})
	h.svc.SetHubCredential(id, value)
	return value
}

// credentialAnswer is the answer to a read of demo--t1 that a hub
// credential of a sandbox of it may make, with cred as the bearer
// credential: 200 while cred authenticates.
func (h *createHub) credentialAnswer(t *testing.T, cred string) answer {
	t.Helper()
	return h.call(t, http.MethodGet, "/workspaces/demo--t1/claims", cred, nil)
}

// sandboxCalls are the operations the runtime was asked for on the sandbox
// id or a provider of it, from the n-th call of the runtime on, in order.
func (h *createHub) sandboxCalls(id string, n int) []string {
	var ops []string
	for _, c := range h.rt.Calls()[n:] {
		if strings.HasPrefix(c.Target, id) {
			ops = append(ops, c.Op)
		}
	}
	return ops
}

// startRestoreHub starts a create hub with the profiles given as TOML and
// the create world, with dana a member of acme and no sandbox limit that a
// test reaches, whose provisioner runs and whose fake runtime keeps each
// sandbox's files in a directory of the host (hostGit).
func startRestoreHub(t *testing.T, profiles string) *createHub {
	t.Helper()
	h := newCreateHub(t, runtimetest.New(t, runtimetest.WithHostDirs()), profiles, true, nil)
	h.rt.SetExec(h.hostGit)
	h.world(t)
	h.AddMember(t, h.org, h.dana)
	h.setLimits(limits{100, 100, 100, 100})
	return h
}

// hostGit runs the commands of a restore hub's sandboxes in their host
// directories, with the real git and without the host's git configuration,
// but for what the checkout cannot do there: its git config --global does
// nothing, and its clone of the hub's git URL makes the working directory
// and clones the repository of demo on disk into it.
func (h *createHub) hostGit(ctx context.Context, name string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
	dir := filepath.Join(h.rt.HostDir("parley", name), strings.TrimPrefix(req.WorkDir, "/"))
	argv := req.Command
	switch {
	case len(argv) > 2 && argv[0] == "git" && argv[1] == "config" && argv[2] == "--global":
		return 0
	case isClone(argv):
		if err := os.MkdirAll(dir, 0o755); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		argv = []string{"git", "clone", "--quiet", "--no-checkout", h.workspaces.RepoPath("demo"), "."}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for _, env := range []map[string]string{req.Env, noHostGit} {
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Stdin = bytes.NewReader(req.Stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	var exit *exec.ExitError
	switch err := cmd.Run(); {
	case errors.As(err, &exit):
		return exit.ExitCode()
	case err != nil:
		_, _ = fmt.Fprintln(stderr, err)
		return 127
	}
	return 0
}

// dirtySource stores a deleted sandbox of demo--t1 whose checkpoint holds
// the dirty ref t1 at a commit on main, and returns its id; with stored, the
// ref is in the repository of demo--t1.
func (h *createHub) dirtySource(t *testing.T, stored bool) string {
	t.Helper()
	dirty := gitrepotest.Commit(t, h.workspaces.RepoPath("demo"), "parley checkpoint", map[string]string{"README": "dirty\n"}, h.base)
	id := h.seedRecord(t, "demo--t1", sandbox.StateDeleted, func(r *sandbox.Record) {
		r.Checkpoint = json.RawMessage(`{"ok":true,"error":null,"refs":[{"branch":"t1","sha":"` + dirty + `","dirty":true}]}`)
	})
	if stored {
		gitrepotest.SetRef(t, h.workspaces.RepoPath("demo--t1"), sandbox.CheckpointRefPrefix+id+"/t1", dirty)
	}
	return id
}

// uploads is the number of files the runtime was asked to write into the
// sandbox id, from its n-th call on.
func (h *createHub) uploads(id string, n int) int {
	return len(slices.DeleteFunc(h.sandboxCalls(id, n), func(op string) bool { return op != runtime.OpUpload }))
}

// eventTypes are the types of the hub events about the sandbox id, oldest
// first.
func (h *createHub) eventTypes(t *testing.T, id string) []string {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT type FROM hub_events WHERE resource_id = ? ORDER BY seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		out = append(out, typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// runtimeSandbox stores a sandbox of demo--t1 in state, created by dana
// with the profile agent's hub credential and its checkpoint flag, as edit
// changes it when edit is not nil, creates it at the runtime with the
// provider id-model, both labelled with its id, mints its hub credential,
// and returns its id and the credential's value.
func (h *createHub) runtimeSandbox(t *testing.T, state string, edit func(*sandbox.Record)) (id, cred string) {
	t.Helper()
	id, dir := h.workSandbox(t, func(r *sandbox.Record) {
		r.State = state
		if edit != nil {
			edit(r)
		}
	})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := runtime.Provider{Name: id + "-model", Profile: "anthropic", Value: "v", Labels: map[string]string{sandbox.LabelSandbox: id}}
	if err := h.rt.Client("parley", nil).EnsureProvider(context.Background(), p); err != nil {
		t.Fatalf("create the provider at the runtime: %v", err)
	}
	return id, h.hubCredential(t, id)
}

// idLog counts the ids it is given.
type idLog struct {
	mu  sync.Mutex
	ids map[string]int
}

func (l *idLog) add(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ids == nil {
		l.ids = map[string]int{}
	}
	l.ids[id]++
}

// snapshot returns a copy of the counts.
func (l *idLog) snapshot() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.ids))
	for id, n := range l.ids {
		out[id] = n
	}
	return out
}

// denialRetry is the retry delay of the policy collectors of a collector
// hub, and collectorInterval the poll interval of their proposal listing.
const (
	denialRetry       = 5 * time.Second
	collectorInterval = 5 * time.Second
)

// closedProfile is a profile that does not allow proposals.
const closedProfile = "[[sandbox.profiles]]\nname = \"closed\"\nttl = \"4h\"\nallow_proposals = false\n"

// collectorHub is an exec hub, which runs no policy collector of its own,
// so that its test drives the passes of the collectors it builds, with a
// metrics recorder and a clock of its own for the collectors' retry delay
// and poll interval. Its log collector is held after its first round,
// which runs before the hub is handed to the test and so follows no
// sandbox: every log stream the runtime has open is a policy collector's.
// Besides agent, it has the profile closed (closedProfile).
type collectorHub struct {
	*execHub
	rec *recordingRecorder
	// resolver resolves the secrets of the hub's sandboxes.
	resolver *stubResolver
	// logs holds the hub's log collector.
	logs *logCollectorHold
	// retryClock is the clock the collectors count their retry delay and
	// their poll interval on.
	retryClock *hubtest.JobClock
}

// startCollectorHub starts a collector hub.
func startCollectorHub(t *testing.T) *collectorHub {
	t.Helper()
	h := &collectorHub{
		rec: &recordingRecorder{}, resolver: &stubResolver{}, logs: newLogCollectorHold(),
		retryClock: hubtest.NewJobClock(func() time.Time { return recordStart }),
	}
	h.execHub = newExecHub(t, closedProfile, func(cfg *hubtest.Config) {
		compose := cfg.Compose
		cfg.Compose = func(c *hub.Composition) {
			compose(c)
			c.SetSandboxRecorder(h.rec)
			c.AdjustSandbox(func(d *sandbox.Deps) {
				h.resolver.next, d.ResolveSecrets = d.ResolveSecrets, h.resolver.resolve
				d.NewTimer = h.logs.wrap(d.NewTimer)
			})
		}
	})
	// Registered after the hub's start, so run before its stop, which then
	// does not wait for the held collector.
	t.Cleanup(h.logs.release)
	hubtestWait(t, h.logs.held, "the log collector to end its first round")
	return h
}

// logCollectorHold holds a hub's log collector after its first round. The
// collector asks for the timer of each round's wait before it runs the
// round: the hold makes the first timer fire at once and keeps the call
// that asks for the second, which comes after the first round, until it is
// released. The policy collector, the one other user of timers of
// LogCollectInterval, is held by the hub and run by the test only after
// the hold took effect.
type logCollectorHold struct {
	mu       sync.Mutex
	asked    int
	held     chan struct{}
	released chan struct{}
	once     sync.Once
}

func newLogCollectorHold() *logCollectorHold {
	return &logCollectorHold{held: make(chan struct{}), released: make(chan struct{})}
}

// wrap returns next, the hub's timers, with the log collector's first two
// timers as the hold makes them.
func (l *logCollectorHold) wrap(next func(time.Duration) sandbox.Timer) func(time.Duration) sandbox.Timer {
	return func(d time.Duration) sandbox.Timer {
		if d != sandbox.LogCollectInterval {
			return next(d)
		}
		l.mu.Lock()
		l.asked++
		asked := l.asked
		l.mu.Unlock()
		switch asked {
		case 1:
			fired := make(chan time.Time, 1)
			fired <- recordStart
			return firedTimer(fired)
		case 2:
			close(l.held)
			<-l.released
		}
		return next(d)
	}
}

// release lets the held log collector go on.
func (l *logCollectorHold) release() { l.once.Do(func() { close(l.released) }) }

// firedTimer is a timer that has fired.
type firedTimer chan time.Time

func (t firedTimer) C() <-chan time.Time { return t }
func (t firedTimer) Stop() bool          { return false }

// collector returns a policy collector of the hub with the retry delay
// denialRetry and the poll interval collectorInterval on the hub's retry
// clock, stopped when the test ends.
func (h *collectorHub) collector(t *testing.T) *sandbox.PolicyCollector {
	t.Helper()
	c := h.svc.NewPolicyCollector(sandbox.PolicyCollectorOptions{Interval: collectorInterval, RetryDelay: denialRetry, Now: h.retryClock.Now})
	t.Cleanup(c.Stop)
	return c
}

// readySandbox is execHub.sandbox for one row of a table that shares the
// hub: the sandbox, edited by edit when it is not nil, is stopped in the
// store when the row ends, so that the collectors of later rows do not
// follow it.
func (h *collectorHub) readySandbox(t *testing.T, edit func(*sandbox.Record)) string {
	t.Helper()
	id := h.sandbox(t, edit)
	t.Cleanup(func() {
		if _, err := h.svc.Transition(context.Background(), nil, id, sandbox.StateReady, sandbox.StateStopped, "", nil); err != nil {
			t.Errorf("stop %s: %v", id, err)
		}
	})
	return id
}

// deny has the runtime write a denial of program to host:port to the
// policy log of the sandbox id, with method and path ("" for a blocked
// connection).
func (h *collectorHub) deny(id, program, host string, port int, method, path string) {
	h.rt.EmitPolicyLog("parley", id, runtime.LogEntry{Action: "deny", Program: program, Host: host, Port: port, Method: method, Path: path})
}

// counted is the value of the denials metric.
func (h *collectorHub) counted() float64 { return h.rec.values(sandbox.DenialsMetric)[""] }

// denials are the parley.sandbox.deny events of the sandbox id, oldest
// first.
func (h *collectorHub) denials(t *testing.T, id string) []hubEvent {
	t.Helper()
	return h.sandboxEvents(t, sandbox.EventDeny, id)
}

// reported waits until the sandbox id has n parley.sandbox.deny events.
func (h *collectorHub) reported(t *testing.T, id string, n int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d denial events of %s", n, id), func() bool { return len(h.denials(t, id)) >= n })
}

// followed are the ids of the sandboxes with an open log stream, sorted, a
// sandbox once per stream: in a collector hub, the streams of the policy
// collectors the test runs.
func (h *execHub) followed() []string {
	var out []string
	for _, s := range h.rt.Streams() {
		if s.Op == runtime.OpLogs {
			out = append(out, s.Sandbox)
		}
	}
	slices.Sort(out)
	return out
}

// allowProposals records a sandbox as created from a profile that allows
// proposals.
func allowProposals(r *sandbox.Record) { r.Snapshot.AllowProposals = true }

// propose adds a pending proposal of the rule body, with the summary
// "allow " and body and the findings, to the sandbox id at the runtime, and
// returns it with the runtime's id.
func (h *collectorHub) propose(id, body string, findings ...runtime.Finding) runtime.Proposal {
	p := runtime.Proposal{Sandbox: id, Rule: runtime.Rule{Name: "proposed", Body: json.RawMessage(body)}, Summary: "allow " + body, Findings: findings}
	p.ID = h.rt.AddProposal("parley", p)
	return p
}

// proposals are the stored proposals of the sandbox id in the state, by
// created_at and id.
func (h *collectorHub) proposals(t *testing.T, id, state string) []sandbox.Proposal {
	t.Helper()
	ps, err := sandbox.ListProposals(context.Background(), h.Store.Main(), id, state, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// runtimeCalls is how many calls of the operation op naming target the
// runtime received; "" counts those of every target.
func (h *collectorHub) runtimeCalls(op, target string) int {
	n := 0
	for _, c := range h.rt.Calls() {
		if c.Op == op && (target == "" || c.Target == target) {
			n++
		}
	}
	return n
}

// unheldSandbox creates a sandbox at the runtime of which the hub has no
// record, and returns its id.
func (h *collectorHub) unheldSandbox(t *testing.T) string {
	t.Helper()
	id := sandbox.NewID()
	if _, err := h.rt.Client("parley", nil).CreateSandbox(context.Background(), runtime.SandboxSpec{Name: id, WorkDir: sandbox.WorkDir}); err != nil {
		t.Fatalf("create a sandbox at the runtime: %v", err)
	}
	return id
}

// seedPolicyRows stores, for the sandbox id, one open proposal, one
// reported denial, with how far its policy log was taken, and one approved
// rule.
func (h *collectorHub) seedPolicyRows(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	rec, err := sandbox.Get(ctx, h.Store.Main(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ReportDenial(h.svc, ctx, rec, runtime.LogEntry{Action: "deny", Program: "curl", Host: "example.org", Port: 443}); err != nil {
		t.Fatal(err)
	}
	p := sandbox.Proposal{
		ID: sandbox.NewProposalID(), SandboxID: id, RuntimeID: "1", Rule: json.RawMessage(`{"host":"example.org"}`),
		Summary: "allow example.org", State: sandbox.ProposalOpen, CreatedAt: recordStart,
	}
	a := sandbox.ApprovedRule{SandboxID: id, ProposalID: sandbox.NewProposalID(), Rule: json.RawMessage(`{"host":"example.com"}`), DecidedAt: recordStart}
	err = h.Store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := sandbox.InsertProposal(ctx, tx, p); err != nil {
			return err
		}
		return sandbox.SaveApprovedRule(ctx, tx, a)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// policyRows counts the stored proposals, reported denials, approved rules
// and policy log positions of the sandbox id.
func (h *collectorHub) policyRows(t *testing.T, id string) [4]int {
	t.Helper()
	var n [4]int
	err := h.Store.Main().QueryRow(`SELECT
	(SELECT COUNT(*) FROM sandbox_proposals WHERE sandbox_id = ?),
	(SELECT COUNT(*) FROM sandbox_denials WHERE sandbox_id = ?),
	(SELECT COUNT(*) FROM sandbox_approved_rules WHERE sandbox_id = ?),
	(SELECT COUNT(*) FROM sandbox_policy_log WHERE sandbox_id = ?)`, id, id, id, id).Scan(&n[0], &n[1], &n[2], &n[3])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// startProposalHub starts a create hub whose dana owns the ready
// repositories demo and other, stored without a git repository, for the
// tests of the proposal and rule routes, with the dependencies of its
// stored sandboxes changed by adjust, in order.
func startProposalHub(t *testing.T, adjust ...func(*sandbox.Deps)) *createHub {
	t.Helper()
	h := startCreateHub(t, agentProfile, adjust...)
	h.storedRepositories(t, "demo", "other")
	return h
}

// storedRepositories adds the ready repositories slugs of dana in acme,
// stored without a git repository.
func (h *createHub) storedRepositories(t *testing.T, slugs ...string) {
	t.Helper()
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		for _, slug := range slugs {
			if err := workspace.Insert(ctx, tx, h.record(slug, access.KindRepository, workspace.StatusReady)); err != nil {
				return err
			}
		}
		return nil
	})
}

// seedProposal stores a proposal of the sandbox id in state, created at
// at, with the rule {"host": "<n>.test"}, the summary "allow <n>.test" and
// no findings, n being a counter of the hub's seeds, as edit changes it
// when edit is not nil, and returns it as stored.
func (h *createHub) seedProposal(t *testing.T, id, state string, at time.Time, edit func(*sandbox.Proposal)) sandbox.Proposal {
	t.Helper()
	n := h.proposalSeeds.Add(1)
	host := fmt.Sprintf("p%d.test", n)
	p := sandbox.Proposal{
		ID: sandbox.NewProposalID(), SandboxID: id, RuntimeID: strconv.FormatInt(n, 10),
		Rule: json.RawMessage(`{"host":"` + host + `"}`), Summary: "allow " + host, State: state, CreatedAt: at,
	}
	if edit != nil {
		edit(&p)
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := sandbox.InsertProposal(ctx, tx, p)
		return err
	})
	return p
}

// proposalMap is the proposal object of p as a decoded answer holds it:
// its twelve members, null where p leaves them unset, findings [] without
// findings and the timestamps in RFC 3339, UTC, whole seconds.
func proposalMap(t *testing.T, p sandbox.Proposal) map[string]any {
	t.Helper()
	var rule any
	if err := json.Unmarshal(p.Rule, &rule); err != nil {
		t.Fatal(err)
	}
	findings := []any{}
	for _, f := range p.Findings {
		findings = append(findings, map[string]any{"severity": f.Severity, "message": f.Message})
	}
	o := map[string]any{
		"id": p.ID, "sandbox_id": p.SandboxID, "rule": rule, "summary": p.Summary, "findings": findings,
		"state": p.State, "scope": nil, "workspace_rule_id": nil, "decided_by": nil, "decided_at": nil,
		"comment": nil, "created_at": stamp(p.CreatedAt),
	}
	if p.Scope != nil {
		o["scope"] = *p.Scope
	}
	if p.WorkspaceRuleID != nil {
		o["workspace_rule_id"] = *p.WorkspaceRuleID
	}
	if by := p.DecidedBy; by != nil {
		o["decided_by"] = map[string]any{"actor_type": by.ActorType, "actor_id": by.ActorID, "username": by.Username}
	}
	if p.DecidedAt != nil {
		o["decided_at"] = stamp(*p.DecidedAt)
	}
	if p.Comment != nil {
		o["comment"] = *p.Comment
	}
	return o
}

// listItems returns the items of a 200 list answer, decoded, and its
// next_cursor ("" for null).
func listItems(t *testing.T, a answer) (items []any, next string) {
	t.Helper()
	o := decoded(t, a, http.StatusOK)
	items, ok := o["items"].([]any)
	if !ok {
		t.Fatalf("the list %s has no items list", a.body)
	}
	if o["next_cursor"] != nil {
		next, ok = o["next_cursor"].(string)
		if !ok || next == "" {
			t.Fatalf("the list %s has a next_cursor that is neither null nor a cursor", a.body)
		}
	}
	return items, next
}

// liveSandbox stores a sandbox of demo in state and creates it at the
// runtime, which can then take rules into its policy, and returns its id.
func (h *createHub) liveSandbox(t *testing.T, state string) string {
	t.Helper()
	return h.liveSandboxIn(t, "demo", state, nil)
}

// liveSandboxIn is liveSandbox with the sandbox stored on the workspace ws
// as edit changes it when edit is not nil.
func (h *createHub) liveSandboxIn(t *testing.T, ws, state string, edit func(*sandbox.Record)) string {
	t.Helper()
	id := h.seedRecord(t, ws, state, edit)
	if _, err := h.rt.Client("parley", nil).CreateSandbox(context.Background(), runtime.SandboxSpec{Name: id, WorkDir: sandbox.WorkDir}); err != nil {
		t.Fatalf("create the sandbox at the runtime: %v", err)
	}
	return id
}

// addedRules returns the rules the runtime was asked to add to the policy
// of the sandbox id, as the policy it holds lists them, and how many
// add-rule calls named the sandbox, successful or not.
func (h *createHub) addedRules(t *testing.T, id string) (rules []runtime.Rule, calls int) {
	t.Helper()
	for _, c := range h.rt.Calls() {
		if c.Op == runtime.OpAddPolicyRule && c.Target == id {
			calls++
		}
	}
	if _, ok := h.rt.Spec("parley", id); !ok {
		return nil, calls
	}
	p, err := h.rt.Client("parley", nil).GetPolicy(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p.Policy.Added, calls
}

// undecided checks that nothing was decided of the proposal p: it is
// stored open, its sandbox's policy_revision is 0, the runtime was asked
// to add no rule to the sandbox's policy, and no decision event, workspace
// sandbox rule or rule_add event exists.
func (h *createHub) undecided(t *testing.T, p sandbox.Proposal) {
	t.Helper()
	got, err := sandbox.GetProposal(context.Background(), h.Store.Main(), p.SandboxID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != sandbox.ProposalOpen {
		t.Errorf("proposal state = %s; want open", got.State)
	}
	if n := storedRecord(t, h.Hub, p.SandboxID).PolicyRevision; n != 0 {
		t.Errorf("policy_revision = %d; want 0", n)
	}
	if rules, _ := h.addedRules(t, p.SandboxID); len(rules) != 0 {
		t.Errorf("the runtime added %v; want none", rules)
	}
	if e := h.sandboxEvents(t, sandbox.EventProposalDecided, p.SandboxID); len(e) != 0 {
		t.Errorf("decision events %v; want none", e)
	}
	if n := h.count(t, `SELECT COUNT(*) FROM sandbox_rules`); n != 0 {
		t.Errorf("workspace sandbox rules = %d; want 0", n)
	}
	if n := h.count(t, `SELECT COUNT(*) FROM hub_events WHERE type = 'parley.sandbox.rule_add'`); n != 0 {
		t.Errorf("rule_add events = %d; want 0", n)
	}
}

// pendingProposal adds a pending proposal to the sandbox id at the runtime,
// which must hold the sandbox, and stores it open as seedProposal does,
// made a minute before the hub's clock, under the runtime's id of it.
func (h *createHub) pendingProposal(t *testing.T, id string, edit func(*sandbox.Proposal)) sandbox.Proposal {
	t.Helper()
	return h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, func(p *sandbox.Proposal) {
		if edit != nil {
			edit(p)
		}
		p.RuntimeID = h.rt.AddProposal("parley", runtime.Proposal{Sandbox: id, Rule: runtime.Rule{Name: "proposed", Body: p.Rule}, Summary: p.Summary})
	})
}

// rejected returns how many reject calls named the sandbox id at the
// runtime, successful or not, and the runtime ids of the sandbox's
// proposals the runtime no longer lists as pending among those of ps.
func (h *createHub) rejected(t *testing.T, id string, ps ...sandbox.Proposal) (calls int, gone []string) {
	t.Helper()
	for _, c := range h.rt.Calls() {
		if c.Op == runtime.OpRejectProposal && c.Target == id {
			calls++
		}
	}
	pending, err := h.rt.Client("parley", nil).ListProposals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if !slices.ContainsFunc(pending, func(r runtime.Proposal) bool { return r.Sandbox == id && r.ID == p.RuntimeID }) {
			gone = append(gone, p.RuntimeID)
		}
	}
	return calls, gone
}
