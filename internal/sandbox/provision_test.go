package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

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

func TestProvision_ReachesReady(t *testing.T) {
	t.Parallel()
	h := startProvisionWorld(t)
	for _, tc := range []struct {
		name    string
		command []string
		wantOps []string
	}{
		{"with a command", []string{"make", "test"}, []string{runtime.OpEnsureProvider, runtime.OpEnsureProvider, runtime.OpCreateSandbox, runtime.OpWaitReady, runtime.OpExec, runtime.OpRunMain}},
		{"without a command", nil, []string{runtime.OpEnsureProvider, runtime.OpEnsureProvider, runtime.OpCreateSandbox, runtime.OpWaitReady, runtime.OpExec}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"profile": "full"}
			if tc.command != nil {
				body["command"] = tc.command
			}
			id := sandboxObject(t, h.post(t, "demo--t1", body))["id"].(string)
			rec := h.settled(t, id)
			if rec.State != sandbox.StateReady || !rec.UpdatedAt.Equal(h.clock.Now()) || rec.Marker != "" {
				t.Fatalf("sandbox = %s %q, updated %v, marker %q; want ready, updated now, no marker", rec.State, rec.StateReason, rec.UpdatedAt, rec.Marker)
			}
			o := objectMap(t, rec)
			ev := h.sandboxEvents(t, sandbox.EventReady, id)
			if len(ev) != 1 || ev[0].actorType != event.ActorSystem || o["checkout"] == nil || !reflect.DeepEqual(ev[0].metadata["checkout"], o["checkout"]) {
				t.Errorf("ready events %+v; want one by the system with checkout %v", ev, o["checkout"])
			}

			spec, ok := h.rt.Spec("parley", id)
			if !ok {
				t.Fatal("the runtime holds no sandbox named after the sandbox id")
			}
			if spec.Image != "img:1" || spec.CPU != "2" || spec.Memory != "4Gi" || spec.GPU != 0 || spec.WorkDir != sandbox.WorkDir ||
				!reflect.DeepEqual(spec.Labels, map[string]string{"parley.sandbox": id}) {
				t.Errorf("runtime sandbox %+v; want img:1, 2 CPUs, 4Gi, /workspace, labelled with the id", spec)
			}
			if string(spec.Policy.Base) != policyContent || len(spec.Policy.Added) != 0 {
				t.Errorf("policy base %q, added %v; want the profile's policy file and no added rule", spec.Policy.Base, spec.Policy.Added)
			}
			if got, want := decodedRules(t, spec.Policy.Protected), []any{hubRule(id, "demo--t1"), credentialRule(id, "model")}; !reflect.DeepEqual(got, want) {
				t.Errorf("protected rules %#v; want %#v", got, want)
			}
			if got := providerNames(spec); !slices.Equal(got, []string{"hub", "model"}) {
				t.Errorf("providers %v; want [hub model]", got)
			}
			model, _ := h.rt.Provider("parley", id+"-model")
			hubp, _ := h.rt.Provider("parley", id+"-hub")
			if model.Value != modelSecret || model.Profile != "anthropic" || model.Labels["parley.sandbox"] != id {
				t.Errorf("provider model = %s %v; want the secret MODEL_KEY on anthropic, labelled with the id", model.Profile, model.Labels)
			}
			if hubp.Profile != sandbox.HubProviderProfile || hubp.Labels["parley.sandbox"] != id || !slices.Contains(h.minter.minted(), hubp.Value) {
				t.Errorf("provider hub = %s %v; want the minted hub credential on %s, labelled with the id", hubp.Profile, hubp.Labels, sandbox.HubProviderProfile)
			}
			if got := sandboxOps(h.rt.Calls(), id); !slices.Equal(got, tc.wantOps) {
				t.Errorf("runtime calls %v; want %v", got, tc.wantOps)
			}
			main, started := h.hooks.main(id)
			if started != (tc.command != nil) || !slices.Equal(main, tc.command) {
				t.Errorf("main process %v (started %t); want %v", main, started, tc.command)
			}
		})
	}
}

func TestProvision_Environment(t *testing.T) {
	t.Parallel()
	h := startProvisionWorld(t)
	const session = "0c9e4b6a-1111-4222-8333-444455556666"
	provision := func(t *testing.T, slug string, body map[string]any) (string, runtime.SandboxSpec) {
		t.Helper()
		id := sandboxObject(t, h.post(t, slug, body))["id"].(string)
		if rec := h.settled(t, id); rec.State != sandbox.StateReady {
			t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
		}
		spec, _ := h.rt.Spec("parley", id)
		return id, spec
	}

	id, spec := provision(t, "demo--t1", map[string]any{
		"profile": "full", "session_id": session,
		"env": map[string]string{"CI": "1", "TIER": "caller", "ANTHROPIC_API_KEY": "from-the-caller"},
	})
	// The runtime puts each attached provider's placeholder into the
	// variables its provider profile declares (ANTHROPIC_API_KEY for model,
	// PARLEY_API_KEY for the hub credential), so the hub leaves them to it.
	want := map[string]string{
		"PARLEY_ENDPOINT_URL": "https://hub.test", "PARLEY_WORKSPACE": "demo--t1",
		"PARLEY_REMOTE": "https://hub.test/git/acme/demo--t1.git", "PARLEY_SANDBOX_ID": id, "PARLEY_SESSION_ID": session,
		"NPM_TOKEN": npmSecret, "TIER": "profile", "CI": "1",
	}
	if !reflect.DeepEqual(spec.Env, want) {
		t.Errorf("env = %v; want %v", spec.Env, want)
	}
	if got := providerNames(spec); !slices.Equal(got, []string{"hub", "model"}) {
		t.Errorf("providers %v; want [hub model], whose placeholders the runtime sets", got)
	}
	for _, v := range append(h.minter.minted(), modelSecret) {
		if slices.Contains(slices.Collect(maps.Values(spec.Env)), v) {
			t.Error("a variable holds the real value of a credential's secret or of the hub credential")
		}
	}

	for _, tc := range []struct {
		name, slug string
		body       map[string]any
		check      func(env map[string]string) bool
	}{
		{"without a session", "demo--t2", map[string]any{"profile": "full"}, func(env map[string]string) bool {
			_, has := env["PARLEY_SESSION_ID"]
			return !has && env["PARLEY_WORKSPACE"] == "demo--t2"
		}},
		{"without hub access", "demo--t1", map[string]any{"profile": "plain"}, func(env map[string]string) bool {
			_, url := env["PARLEY_ENDPOINT_URL"]
			_, remote := env["PARLEY_REMOTE"]
			_, key := env["PARLEY_API_KEY"]
			return !url && !remote && !key && env["PARLEY_WORKSPACE"] == "demo--t1" && env["PARLEY_SANDBOX_ID"] != ""
		}},
		{"with no profile env, secret or credential", "demo--t2", map[string]any{"profile": "agent", "env": map[string]string{"CI": "1"}}, func(env map[string]string) bool {
			for k := range env {
				if !strings.HasPrefix(k, "PARLEY_") && k != "CI" {
					return false
				}
			}
			return env["CI"] == "1"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, spec := provision(t, tc.slug, tc.body)
			if !tc.check(spec.Env) {
				t.Errorf("env = %v", spec.Env)
			}
		})
	}
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

func TestProvision_FailureAtEachStep(t *testing.T) {
	t.Parallel()
	h := startProvisionWorld(t)
	ctx := context.Background()
	creates := func(result string) float64 { return h.rec.values(sandbox.CreatesMetric)["result="+result] }
	failOn := func(op, name, text string) func(ctx context.Context, o, n string) error {
		return func(_ context.Context, o, n string) error {
			if o == op && strings.HasSuffix(n, name) {
				return runtime.NewError(runtime.ErrUnavailable, op, text)
			}
			return nil
		}
	}
	for _, tc := range []struct {
		name string
		// fault sets the fault before the create and returns what ends it.
		fault func(t *testing.T) func()
		// create creates the sandbox and returns its id; nil posts body.
		create  func(t *testing.T) string
		body    map[string]any
		reason  string
		wantErr string
		// errHas is a text the event's error holds.
		errHas string
		// noCreate says the runtime received no create call.
		noCreate bool
	}{
		{name: "a required secret gone after the create", reason: "credential_missing", create: func(t *testing.T) string {
			ws, _, err := workspace.BySlug(ctx, h.Store.Main(), "demo--t1")
			if err != nil {
				t.Fatal(err)
			}
			var rec sandbox.Record
			caller := identity.Principal{Type: identity.TypeAPIKey, User: &identity.User{ID: h.dana, Username: "dana"}}
			h.insert(t, func(ctx context.Context, tx *sql.Tx) (err error) {
				rec, err = h.svc.Create(ctx, tx, sandbox.CreateInput{Caller: caller, Workspace: ws, Body: []byte(`{"profile":"full"}`), SessionID: ws.SessionID})
				return err
			})
			h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
				_, err := h.secrets.Delete(ctx, tx, secret.ScopeOrg, h.org, "MODEL_KEY")
				return err
			})
			t.Cleanup(func() { h.putSecret(t, secret.ScopeOrg, h.org, "MODEL_KEY", modelSecret) })
			h.svc.WakeProvisioner()
			return rec.ID
		}},
		{name: "the runtime refuses the second provider", reason: "provision_failed", fault: func(*testing.T) func() {
			h.hooks.set(failOn(runtime.OpEnsureProvider, "-hub", "provider refused"))
			return func() { h.hooks.set(nil) }
		}},
		{name: "the runtime cannot be reached", reason: "provision_failed", fault: func(*testing.T) func() {
			h.rt.SetUnreachable(true)
			return func() { h.rt.SetUnreachable(false) }
		}},
		{name: "the runtime refuses the policy", reason: "provision_failed", wantErr: "the composer explains: sandbox runtime: create_sandbox: invalid: policy rejected: rule parley-hub is not allowed", fault: func(*testing.T) func() {
			h.svc.RegisterPolicyComposer(&stubComposer{policy: runtime.Policy{Protected: []runtime.Rule{{Name: "parley-hub", Body: json.RawMessage(`{}`)}}}})
			h.rt.RejectPolicies(func(runtime.Policy) bool { return true }, "rule parley-hub is not allowed")
			return func() { h.rt.RejectPolicies(nil, ""); h.svc.RegisterPolicyComposer(nil) }
		}},
		{name: "the composer fails", reason: "provision_failed", noCreate: true, fault: func(*testing.T) func() {
			h.svc.RegisterPolicyComposer(&stubComposer{err: errors.New("no policy for you")})
			return func() { h.svc.RegisterPolicyComposer(nil) }
		}},
		{name: "the git clone exits 128", reason: "checkout_failed", errHas: "fatal: repository not found", fault: func(*testing.T) func() {
			h.hooks.setExec(func(argv []string) (int, string, error) {
				if len(argv) > 1 && argv[1] == "clone" {
					return 128, "fatal: repository not found", nil
				}
				return 0, "", nil
			})
			return func() { h.hooks.setExec(nil) }
		}},
		{name: "the git checkout cannot be run", reason: "checkout_failed", body: map[string]any{"profile": "full", "command": []string{"make"}}, fault: func(*testing.T) func() {
			h.hooks.setExec(func(argv []string) (int, string, error) {
				if len(argv) > 1 && argv[1] == "checkout" {
					return 0, "", runtime.NewError(runtime.ErrUnavailable, runtime.OpExec, "exec refused")
				}
				return 0, "", nil
			})
			return func() { h.hooks.setExec(nil) }
		}},
		{name: "the restorer fails", reason: "provision_failed", fault: func(t *testing.T) func() {
			h.svc.RegisterRestorer(&stubRestorer{hooks: h.hooks, err: errors.New("no checkpoint data")})
			return func() { h.svc.RegisterRestorer(nil) }
		}, create: func(t *testing.T) string {
			old := h.seedIn(t, "demo--t1", json.RawMessage(`{"ok":true,"refs":[]}`))
			return sandboxObject(t, h.post(t, "demo--t1", map[string]any{"profile": "full", "restore_from": old}))["id"].(string)
		}},
		{name: "the main process cannot be started", reason: "provision_failed", body: map[string]any{"profile": "full", "command": []string{"make"}}, fault: func(*testing.T) func() {
			h.rt.FailOperation(runtime.OpRunMain, runtime.ErrUnavailable, "cannot start")
			return func() { h.rt.FailOperation(runtime.OpRunMain, nil, "") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := creates("failed")
			if tc.fault != nil {
				t.Cleanup(tc.fault(t))
			}
			var id string
			if tc.create != nil {
				id = tc.create(t)
			} else {
				body := tc.body
				if body == nil {
					body = map[string]any{"profile": "full"}
				}
				id = sandboxObject(t, h.post(t, "demo--t1", body))["id"].(string)
			}
			rec := h.settled(t, id)
			if rec.State != sandbox.StateFailed || rec.StateReason != tc.reason || rec.EndedAt == nil || rec.Marker != "" {
				t.Errorf("sandbox = %s %q, ended %v, marker %q; want failed %q, ended, no marker", rec.State, rec.StateReason, rec.EndedAt, rec.Marker, tc.reason)
			}
			ev := h.sandboxEvents(t, sandbox.EventFail, id)
			if len(ev) != 1 {
				t.Fatalf("fail events %+v; want one", ev)
			}
			text, _ := ev[0].metadata["error"].(string)
			if ev[0].actorType != event.ActorSystem || ev[0].metadata["reason"] != tc.reason || text == "" || len(text) > 1024 {
				t.Errorf("fail event %+v; want one by the system with reason %s and an error of at most 1024 bytes", ev[0], tc.reason)
			}
			if tc.wantErr != "" && text != tc.wantErr {
				t.Errorf("fail event error %q; want %q", text, tc.wantErr)
			}
			if !strings.Contains(text, tc.errHas) {
				t.Errorf("fail event error %q; want it to hold %q", text, tc.errHas)
			}
			if main, started := h.hooks.main(id); started && tc.reason == "checkout_failed" {
				t.Errorf("the main process %v was started", main)
			}
			if n := len(h.sandboxEvents(t, sandbox.EventReady, id)); n != 0 {
				t.Errorf("%d ready events; want none", n)
			}
			if left := h.runtimeLeft(t, id); len(left) != 0 {
				t.Errorf("the runtime still holds %v", left)
			}
			if tc.noCreate && slices.Contains(sandboxOps(h.rt.Calls(), id), runtime.OpCreateSandbox) {
				t.Error("the runtime received a create call")
			}
			tok, err := identity.PATByID(ctx, h.Store.Main(), rec.HubTokenID)
			if err != nil || tok.RevokedAt == nil {
				t.Errorf("hub credential %+v, %v; want it revoked", tok, err)
			}
			if got := creates("failed"); got != before+1 {
				t.Errorf("creates_total{failed} = %v; want %v", got, before+1)
			}
		})
	}
}

func TestProvision_TimesOutAfterTenMinutes(t *testing.T) {
	t.Parallel()
	h := startProvisionWorld(t)
	var (
		mu      sync.Mutex
		ctxErr  error
		blocked = make(chan struct{})
	)
	h.hooks.set(func(ctx context.Context, op, _ string) error {
		if op != runtime.OpCreateSandbox {
			return nil
		}
		close(blocked)
		<-ctx.Done()
		mu.Lock()
		ctxErr = ctx.Err()
		mu.Unlock()
		return runtime.NewError(runtime.ErrUnavailable, op, ctx.Err().Error())
	})
	id := sandboxObject(t, h.post(t, "demo--t1", map[string]any{"profile": "full"}))["id"].(string)
	hubtestWait(t, h.clock.Armed(sandbox.ProvisionBound), "the provisioning's bound")
	hubtestWait(t, blocked, "the create call")

	h.clock.Advance(sandbox.ProvisionBound - time.Second)
	if rec := h.get(t, id); rec.State != sandbox.StateProvisioning {
		t.Fatalf("after 9m59s the sandbox is %s; want provisioning", rec.State)
	}
	h.clock.Advance(time.Second)
	rec := h.settled(t, id)
	if rec.State != sandbox.StateFailed || rec.StateReason != "provision_failed" {
		t.Errorf("sandbox = %s %q; want failed provision_failed", rec.State, rec.StateReason)
	}
	if n := len(h.sandboxEvents(t, sandbox.EventFail, id)); n != 1 {
		t.Errorf("%d fail events; want 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(ctxErr, context.Canceled) && !errors.Is(ctxErr, context.DeadlineExceeded) {
		t.Errorf("the blocked create call's context ended with %v; want it cancelled", ctxErr)
	}
	if left := h.runtimeLeft(t, id); len(left) != 0 {
		t.Errorf("the runtime still holds %v", left)
	}
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

// get reads the stored sandbox id.
func (h *createHub) get(t *testing.T, id string) sandbox.Record {
	t.Helper()
	rec, err := sandbox.Get(context.Background(), h.Store.Main(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestProvision_RecordDeletedMeanwhile(t *testing.T) {
	t.Parallel()
	h := startProvisionWorld(t)
	ctx := context.Background()
	p := newPause()
	h.hooks.set(func(ctx context.Context, op, _ string) error {
		if op == runtime.OpCreateSandbox {
			return p.wait(ctx, op)
		}
		return nil
	})
	creates := h.rec.values(sandbox.CreatesMetric)
	id := sandboxObject(t, h.post(t, "demo--t1", map[string]any{"profile": "full"}))["id"].(string)
	hubtestWait(t, p.entered, "the create call")
	applied, err := h.svc.Transition(ctx, nil, id, sandbox.StateProvisioning, sandbox.StateDeleted, "api",
		&event.Input{Type: "parley.sandbox.delete", ActorType: event.ActorAPIKey, ActorID: &h.dana})
	if !applied || err != nil {
		t.Fatalf("delete = %t, %v", applied, err)
	}
	rec := h.get(t, id)
	close(p.release)

	eventually(t, "the provisioning's clean-up", func() bool {
		tok, err := identity.PATByID(ctx, h.Store.Main(), rec.HubTokenID)
		return err == nil && tok.RevokedAt != nil && len(h.runtimeLeft(t, id)) == 0
	})
	if got := h.get(t, id); got.State != sandbox.StateDeleted || got.StateReason != "api" || !got.UpdatedAt.Equal(rec.UpdatedAt) {
		t.Errorf("sandbox = %s %q; want it deleted as the delete left it", got.State, got.StateReason)
	}
	for _, typ := range []string{sandbox.EventReady, sandbox.EventFail} {
		if n := len(h.sandboxEvents(t, typ, id)); n != 0 {
			t.Errorf("%d %s events; want none", n, typ)
		}
	}
	if got := h.rec.values(sandbox.CreatesMetric); !maps.Equal(got, creates) {
		t.Errorf("creates_total = %v; want it unchanged from %v", got, creates)
	}
}

func TestProvision_ResumesAfterRestart(t *testing.T) {
	t.Parallel()
	h := startProvisionWorld(t)
	ctx := context.Background()
	p := newPause()
	var first sync.Once
	h.hooks.set(func(ctx context.Context, op, _ string) error {
		held := false
		if op == runtime.OpWaitReady {
			first.Do(func() { held = true })
		}
		if held {
			return p.wait(ctx, op)
		}
		return nil
	})
	pat := h.IssuePAT(t, identity.PATSpec{UserID: h.dana, Permissions: []string{"sandboxes:write", "git:read", "git:write"}, Paths: []string{"src/**"}})
	a := h.call(t, "POST", "/workspaces/demo--t1/sandboxes", pat, map[string]any{})
	s := sandboxObject(t, a)
	id := s["id"].(string)
	hubAccess := s["hub_access"]
	t1 := h.get(t, id).HubTokenID
	hubtestWait(t, p.entered, "the provisioning to reach the runtime's sandbox")

	h.Restart(t)
	rec := h.settled(t, id)
	if rec.State != sandbox.StateReady {
		t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
	}
	if n := len(h.sandboxEvents(t, sandbox.EventReady, id)); n != 1 {
		t.Errorf("%d ready events; want 1", n)
	}
	if left := h.runtimeLeft(t, id); !slices.Equal(left, []string{id, id + "-hub"}) {
		t.Errorf("the runtime holds %v; want the sandbox and its one provider", left)
	}
	old, err := identity.PATByID(ctx, h.Store.Main(), t1)
	if err != nil || old.RevokedAt == nil {
		t.Errorf("the first hub credential %+v, %v; want it revoked", old, err)
	}
	var live []identity.PAT
	for tid := range h.tokenIDs(t) {
		tok, err := identity.PATByID(ctx, h.Store.Main(), tid)
		if err != nil {
			t.Fatal(err)
		}
		if tok.Workspace == "demo--t1" && tok.RevokedAt == nil {
			live = append(live, tok)
		}
	}
	ha, _ := hubAccess.(map[string]any)
	if len(live) != 1 || ha == nil {
		t.Fatalf("live tokens bound to demo--t1: %+v; want one", live)
	}
	tok := live[0]
	if got := slices.Sorted(slices.Values(tok.Permissions)); !reflect.DeepEqual(jsonList(got), ha["scopes"]) ||
		!reflect.DeepEqual(jsonList(tok.Paths), ha["paths"]) || tok.ExpiresAt == nil || stamp(*tok.ExpiresAt) != ha["expires_at"] {
		t.Errorf("replacement %v %v %v; want the hub access %v", tok.Permissions, tok.Paths, tok.ExpiresAt, ha)
	}
	if tok.TokenID != rec.HubTokenID {
		t.Errorf("the record keeps token %s; want the replacement %s", rec.HubTokenID, tok.TokenID)
	}
	if got := objectMap(t, rec)["hub_access"]; !reflect.DeepEqual(got, hubAccess) {
		t.Errorf("hub_access = %v; want it unchanged, %v", got, hubAccess)
	}
}

func TestCreate_RecordsWorkspaceRules(t *testing.T) {
	t.Parallel()
	h := startProvisionWorld(t)
	r1, r2 := json.RawMessage(`{"host":"a.test"}`), json.RawMessage(`{"host":"b.test"}`)
	rules := &stubRules{rules: []sandbox.WorkspaceRule{{ID: "sbr_a", Body: r1}, {ID: "sbr_b", Body: r2}}}
	composed := runtime.Policy{Base: []byte("composed"), Added: []runtime.Rule{{Name: "sbr_a", Body: r1}}}
	composer := &stubComposer{policy: composed}
	h.svc.RegisterWorkspaceRuleProvider(rules)
	h.svc.RegisterPolicyComposer(composer)

	s := sandboxObject(t, h.post(t, "demo--t1", map[string]any{}))
	id := s["id"].(string)
	ids := nameList("sbr_a", "sbr_b")
	if !reflect.DeepEqual(s["workspace_rules"], ids) || !slices.Equal(rules.asked(), []string{"demo"}) {
		t.Errorf("workspace_rules %v, provider asked for %v; want %v for [demo]", s["workspace_rules"], rules.asked(), ids)
	}
	rules.set(nil)
	if rec := h.settled(t, id); rec.State != sandbox.StateReady {
		t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
	}
	read := composer.rulesRead()
	if len(read) != 1 || len(read[0]) != 2 || string(read[0][0].Body) != string(r1) || string(read[0][1].Body) != string(r2) {
		t.Errorf("the composer read %v; want the copies of R1 and R2", read)
	}
	if spec, _ := h.rt.Spec("parley", id); !reflect.DeepEqual(spec.Policy, composed) {
		t.Errorf("runtime policy %+v; want the composer's %+v", spec.Policy, composed)
	}
	if got := objectMap(t, h.get(t, id))["workspace_rules"]; !reflect.DeepEqual(got, ids) {
		t.Errorf("workspace_rules read later = %v; want %v", got, ids)
	}
	if ev := h.sandboxEvents(t, sandbox.EventCreate, id); len(ev) != 1 || !reflect.DeepEqual(ev[0].metadata["workspace_rules"], ids) {
		t.Errorf("create events %+v; want workspace_rules %v", ev, ids)
	}

	h.svc.RegisterWorkspaceRuleProvider(nil)
	h.svc.RegisterPolicyComposer(nil)
	s = sandboxObject(t, h.post(t, "demo--t1", map[string]any{}))
	id = s["id"].(string)
	if !reflect.DeepEqual(s["workspace_rules"], nameList()) {
		t.Errorf("workspace_rules without a provider = %v; want []", s["workspace_rules"])
	}
	h.settled(t, id)
	if rec := h.get(t, id); len(rec.WorkspaceRules) != 0 {
		t.Errorf("stored rules %v; want none", rec.WorkspaceRules)
	}
	if spec, _ := h.rt.Spec("parley", id); len(spec.Policy.Added) != 0 {
		t.Errorf("added rules %v; want none", spec.Policy.Added)
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
