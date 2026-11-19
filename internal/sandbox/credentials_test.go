package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// fullProfile lists a credential and an exposed secret of each kind:
// credentials [model], optional_credentials [mirror], expose_secrets
// [NPM_TOKEN] and optional_expose_secrets [EXTRA, ALSO].
const fullProfile = "[[sandbox.profiles]]\nname = \"full\"\nttl = \"4h\"\ncredentials = [\"model\"]\noptional_credentials = [\"mirror\"]\n" +
	"expose_secrets = [\"NPM_TOKEN\"]\noptional_expose_secrets = [\"EXTRA\", \"ALSO\"]\n"

// needsProfile requires the credential model.
const needsProfile = "[[sandbox.profiles]]\nname = \"needs\"\nttl = \"4h\"\ncredentials = [\"model\"]\n"

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

func TestCreate_ResolvesCredentialsAndExposedSecrets(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile+fullProfile+credentialTables)
	h.world(t)
	h.AddMember(t, h.org, h.dana)
	h.putSecret(t, secret.ScopeOrg, h.org, "MODEL_KEY", "model-value")
	h.putSecret(t, secret.ScopeWorkspace, "demo--t1", "NPM_TOKEN", "npm-value")
	h.putSecret(t, secret.ScopeUser, h.dana, "ALSO", "also-value")
	for _, step := range []struct {
		name, profile        string
		setup                func()
		credentials, exposed []any
	}{
		{"the optional ones that resolve", "full", func() {}, nameList("model"), nameList("ALSO", "NPM_TOKEN")},
		{"every optional one resolves", "full", func() {
			h.putSecret(t, secret.ScopeWorkspace, "demo--t1", "MIRROR_KEY", "mirror-value")
			h.putSecret(t, secret.ScopeWorkspace, "demo--t1", "EXTRA", "extra-value")
		}, nameList("mirror", "model"), nameList("ALSO", "EXTRA", "NPM_TOKEN")},
		{"a profile that lists nothing", "agent", func() {}, nameList(), nameList()},
	} {
		t.Run(step.name, func(t *testing.T) {
			step.setup()
			s := sandboxObject(t, h.post(t, "demo--t1", map[string]any{"profile": step.profile}))
			wantMembers(t, s, map[string]any{"credentials": step.credentials, "exposed_secrets": step.exposed})
			rec, err := sandbox.Get(context.Background(), h.Store.Main(), s["id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(nameList(rec.Credentials...), step.credentials) || !reflect.DeepEqual(nameList(rec.ExposedSecrets...), step.exposed) {
				t.Errorf("stored credentials %q and exposed secrets %q; want %v and %v", rec.Credentials, rec.ExposedSecrets, step.credentials, step.exposed)
			}
		})
	}
}

func TestCreate_RefusesMissingRequiredSecret(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile+needsProfile+credentialTables+
		"[[sandbox.profiles]]\nname = \"exposes\"\nttl = \"4h\"\nexpose_secrets = [\"NPM_TOKEN\", \"AAA\"]\n"+
		"[[sandbox.profiles]]\nname = \"both\"\nttl = \"4h\"\ncredentials = [\"model\"]\nexpose_secrets = [\"NPM_TOKEN\", \"AAA\"]\n")
	h.world(t)
	h.AddMember(t, h.org, h.dana)
	chains := map[string][]any{
		"demo--t1": nameList("workspace", "parent", "org", "user"),
		"demo":     nameList("workspace", "org", "user"),
	}
	for _, slug := range []string{"demo--t1", "demo"} {
		for _, tc := range []struct {
			profile              string
			credentials, secrets []any
		}{
			{"needs", nameList("model"), nameList()},
			{"exposes", nameList(), nameList("AAA", "NPM_TOKEN")},
			{"both", nameList("model"), nameList("AAA", "NPM_TOKEN")},
		} {
			t.Run(tc.profile+" on "+slug, func(t *testing.T) {
				before := h.footprint(t)
				a := h.post(t, slug, map[string]any{"profile": tc.profile})
				h.wantRefused(t, a, http.StatusUnprocessableEntity, "credential_missing", before)
				wantMembers(t, errorObject(t, a), map[string]any{
					"credentials": tc.credentials, "secrets": tc.secrets, "scopes_searched": chains[slug],
				})
			})
		}
	}
}

func TestCreate_ReadOnlyForkOwnerGetsNoParentSecret(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile+
		"[[sandbox.profiles]]\nname = \"needs-npm\"\nttl = \"4h\"\nexpose_secrets = [\"NPM_TOKEN\"]\n"+
		"[[sandbox.profiles]]\nname = \"maybe-npm\"\nttl = \"4h\"\noptional_expose_secrets = [\"NPM_TOKEN\"]\n")
	h.world(t)
	omar := h.CreateUser(t, "omar")
	h.AddMember(t, h.org, omar)
	h.forkBy(t, "demo--m1", "demo", omar, recordStart.Add(24*time.Hour), nil)
	omarKey := h.IssueKey(t, omar)
	h.putSecret(t, secret.ScopeWorkspace, "demo", "NPM_TOKEN", "npm-value")

	before := h.footprint(t)
	a := h.call(t, http.MethodPost, "/workspaces/demo--m1/sandboxes", omarKey, map[string]any{"profile": "needs-npm"})
	h.wantRefused(t, a, http.StatusUnprocessableEntity, "credential_missing", before)
	wantMembers(t, errorObject(t, a), map[string]any{
		"credentials": nameList(), "secrets": nameList("NPM_TOKEN"), "scopes_searched": nameList("workspace", "org", "user"),
	})
	s := sandboxObject(t, h.call(t, http.MethodPost, "/workspaces/demo--m1/sandboxes", omarKey, map[string]any{"profile": "maybe-npm"}))
	wantMembers(t, s, map[string]any{"exposed_secrets": nameList()})

	// The owner of the parent gets the parent's secret on her fork.
	for _, profile := range []string{"needs-npm", "maybe-npm"} {
		s := sandboxObject(t, h.post(t, "demo--t1", map[string]any{"profile": profile}))
		wantMembers(t, s, map[string]any{"exposed_secrets": nameList("NPM_TOKEN")})
	}
}

func TestCreate_SecretStoreUnreadable(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile+fullProfile+credentialTables)
	h.world(t)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"no encryption key", fmt.Errorf("resolve the secrets of demo: %w", secret.ErrUnavailable)},
		{"a value that cannot be decrypted", fmt.Errorf("resolve the secrets of demo, org scope x: secret %q %w", "MODEL_KEY", secret.ErrUnreadable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.resolver.setErr(tc.err)
			before := h.footprint(t)
			h.wantRefused(t, h.post(t, "demo", map[string]any{"profile": "full"}), http.StatusServiceUnavailable, "secrets_unavailable", before)
			sandboxObject(t, h.post(t, "demo", map[string]any{"profile": "agent"}))
		})
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

func TestNoValueLeavesTheHub(t *testing.T) {
	t.Parallel()
	const (
		modelValue = "sk-model-0123456789"
		npmValue   = "npm_SECRETVALUE42"
		hubValue   = "parley-hub-value-0001"
		session    = "6f1c2f0e-0000-4000-8000-000000000002"
	)
	h := startProvisionHub(t, agentProfile+fullProfile+credentialTables)
	h.world(t)
	h.AddMember(t, h.org, h.dana)
	h.putSecret(t, secret.ScopeOrg, h.org, "MODEL_KEY", modelValue)
	h.putSecret(t, secret.ScopeWorkspace, "demo--t1", "NPM_TOKEN", npmValue)
	ctx := context.Background()

	created := h.post(t, "demo--t1", map[string]any{"profile": "full"})
	s := sandboxObject(t, created)
	wantMembers(t, s, map[string]any{"credentials": nameList("model"), "exposed_secrets": nameList("NPM_TOKEN")})
	id := s["id"].(string)
	if rec := h.settled(t, id); rec.State != sandbox.StateReady {
		t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
	}
	got := h.call(t, http.MethodGet, "/workspaces/demo--t1/sandboxes/"+id, h.key, nil)
	listed := h.call(t, http.MethodGet, "/workspaces/demo--t1/sandboxes?state=ready&state=failed", h.key, nil)
	decoded(t, got, http.StatusOK)
	decoded(t, listed, http.StatusOK)

	// A second sandbox fails at the runtime with a text that holds both
	// secret values and its hub credential's: its fail event carries none.
	h.hooks.set(func(_ context.Context, op, name string) error {
		if op != runtime.OpCreateSandbox {
			return nil
		}
		hubValue, _ := h.rt.Provider("parley", name+"-hub")
		return runtime.NewError(runtime.ErrInvalid, op, "refused "+modelValue+" "+npmValue+" "+hubValue.Value)
	})
	second := sandboxObject(t, h.post(t, "demo--t1", map[string]any{"profile": "full"}))["id"].(string)
	if rec := h.settled(t, second); rec.State != sandbox.StateFailed {
		t.Fatalf("second sandbox = %s; want failed", rec.State)
	}
	h.hooks.set(nil)
	ev := h.sandboxEvents(t, sandbox.EventFail, second)
	text := ""
	if len(ev) == 1 {
		text, _ = ev[0].metadata["error"].(string)
	}
	if strings.Count(text, "[redacted]") != 3 {
		t.Errorf("fail events %+v; want one whose error has the three values redacted", ev)
	}

	// A failure after the secrets were resolved, whose text holds them: the
	// error the create returns carries neither value.
	h.linker.setFail(fmt.Errorf("the session store answered %s and %s", modelValue, npmValue))
	failed := h.post(t, "demo--t1", map[string]any{"profile": "full", "session_id": session})
	if failed.status != http.StatusInternalServerError {
		t.Errorf("create with a failing session link = %d %s; want 500", failed.status, failed.body)
	}
	ws, _, err := workspace.BySlug(ctx, h.Store.Main(), "demo--t1")
	if err != nil {
		t.Fatal(err)
	}
	caller := identity.Principal{Type: identity.TypeAPIKey, User: &identity.User{ID: h.dana, Username: "dana"}}
	_, err = h.svc.Create(ctx, nil, sandbox.CreateInput{Caller: caller, Workspace: ws, Body: []byte(`{"profile":"full","session_id":"` + session + `"}`)})
	if err == nil || strings.Contains(err.Error(), modelValue) || strings.Contains(err.Error(), npmValue) || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("Create = %v; want an error with both values redacted", err)
	}
	h.linker.setFail(nil)

	// The sandbox's own scrubbing knows its values without being told, as
	// after a restart, and those it is given.
	if got, want := h.svc.ScrubSandbox(ctx, id, "a "+modelValue+" b "+npmValue), "a [redacted] b [redacted]"; got != want {
		t.Errorf("ScrubSandbox = %q; want %q", got, want)
	}
	h.svc.SetValues(id, sandbox.Values{Credentials: []string{modelValue}, Exposed: []string{npmValue}, HubCredential: hubValue})
	if got, want := h.svc.ScrubSandbox(ctx, id, modelValue+" "+npmValue+" Bearer "+hubValue), "[redacted] [redacted] Bearer [redacted]"; got != want {
		t.Errorf("ScrubSandbox = %q; want %q", got, want)
	}

	all := strings.Join([]string{
		string(created.body), string(failed.body), string(got.body), string(listed.body),
		dump(t, h.Store.Main(), "hub_events"), dump(t, h.Store.Main(), "sandboxes"), h.Log.String(),
	}, "\n")
	for _, v := range []string{modelValue, npmValue} {
		if strings.Contains(all, v) {
			t.Errorf("the value %q left the hub in a body, an event, a record or the log", v)
		}
	}
	for _, v := range h.minter.minted() {
		if strings.Contains(all, v) {
			t.Error("a hub credential's value left the hub in a body, an event, a record or the log")
		}
	}
}
