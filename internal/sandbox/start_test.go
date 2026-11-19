package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// startProfile is the profile of the start tests: hub access, the required
// credential model, the optional credential mirror and the required exposed
// secret NPM_TOKEN.
const startProfile = "[[sandbox.profiles]]\nname = \"agent\"\ndefault = true\nimage = \"img:1\"\nttl = \"4h\"\nidle_timeout = \"30m\"\n" +
	"checkpoint = true\nhub_access = \"fork\"\ncredentials = [\"model\"]\noptional_credentials = [\"mirror\"]\nexpose_secrets = [\"NPM_TOKEN\"]\n"

// startedObject is what the start tests read of a sandbox object.
type startedObject struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	StateReason *string    `json:"state_reason"`
	ExitCode    *int       `json:"exit_code"`
	EndedAt     *time.Time `json:"ended_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Credentials []string   `json:"credentials"`
}

// The start route's behaviours share one hub whose provisioner runs. The
// rows change the hub's secrets, resolver, limits and runtime for the time
// they run, so none runs in parallel, and the refusals come first, while
// dana has no running sandbox the user limit would count.
func TestStartRoute(t *testing.T) {
	t.Parallel()
	h := startProvisionHub(t, startProfile+credentialTables)
	h.world(t)
	h.AddMember(t, h.org, h.dana)
	h.repository(t, "arch", workspace.StatusArchived)
	h.fork(t, "arch--t1", "arch", recordStart.Add(24*time.Hour), nil)
	for _, name := range []string{"MODEL_KEY", "NPM_TOKEN", "MIRROR_KEY"} {
		h.putSecret(t, secret.ScopeOrg, h.org, name, "value-of-"+name)
	}
	unlimited := limits{100, 100, 100, 100}
	h.setLimits(unlimited)
	path := func(slug, id string) string { return sandboxPath(slug, id) + "/start" }
	start := func(t *testing.T, slug, id string) (answer, startedObject) {
		t.Helper()
		a := h.call(t, http.MethodPost, path(slug, id), h.key, nil)
		var o startedObject
		if a.status == http.StatusOK {
			if err := json.Unmarshal(a.body, &o); err != nil {
				t.Fatalf("the answer %s: %v", a.body, err)
			}
		}
		return a, o
	}
	// stopped stores a stopped sandbox of ws as a create of the profile
	// would have, with a hub credential that was revoked at its stop.
	stopped := func(t *testing.T, ws, state string) string {
		t.Helper()
		return h.seedRecord(t, ws, state, func(r *sandbox.Record) {
			r.FamilyRoot = "demo"
			if ws != "demo" && ws != "demo--t1" {
				r.FamilyRoot = "arch"
			}
			r.StateReason = "api"
			r.Credentials, r.ExposedSecrets = []string{"mirror", "model"}, []string{"NPM_TOKEN"}
			r.HubAccess = &sandbox.HubAccess{Scopes: allHubScopes, ExpiresAt: r.ExpiresAt}
			r.Snapshot = sandbox.Snapshot{
				TTL: 4 * time.Hour, IdleTimeout: 30 * time.Minute, Checkpoint: true, HubAccess: "fork",
				Credentials: []string{"model"}, OptionalCredentials: []string{"mirror"}, ExposeSecrets: []string{"NPM_TOKEN"},
				ProviderProfiles: map[string]string{"model": "anthropic", "mirror": "mirror"},
			}
		})
	}
	deleteSecret := func(t *testing.T, name string) func() {
		t.Helper()
		h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
			_, err := h.secrets.Delete(ctx, tx, secret.ScopeOrg, h.org, name)
			return err
		})
		return func() { h.putSecret(t, secret.ScopeOrg, h.org, name, "value-of-"+name) }
	}

	t.Run("refuses the start and leaves the sandbox as it was", func(t *testing.T) {
		for _, tc := range []struct {
			name, ws, state string
			// set changes the hub for the row and returns what undoes it.
			set           func(t *testing.T) func()
			status        int
			errorType     string
			named, values string
		}{
			{name: "the sandbox is deleted", ws: "demo--t1", state: sandbox.StateDeleted, status: http.StatusConflict, errorType: "sandbox_deleted"},
			{name: "its repository is archived", ws: "arch", state: sandbox.StateStopped, status: http.StatusConflict, errorType: "workspace_archived"},
			{name: "the parent of its fork is archived", ws: "arch--t1", state: sandbox.StateStopped, status: http.StatusConflict, errorType: "parent_archived"},
			{name: "the secret of a required credential is gone", ws: "demo--t1", state: sandbox.StateStopped,
				set:    func(t *testing.T) func() { return deleteSecret(t, "MODEL_KEY") },
				status: http.StatusUnprocessableEntity, errorType: "credential_missing", named: "credentials", values: "model"},
			{name: "a required exposed secret is gone", ws: "demo--t1", state: sandbox.StateCompleted,
				set:    func(t *testing.T) func() { return deleteSecret(t, "NPM_TOKEN") },
				status: http.StatusUnprocessableEntity, errorType: "credential_missing", named: "secrets", values: "NPM_TOKEN"},
			{name: "the secret store cannot be read", ws: "demo--t1", state: sandbox.StateFailed,
				set: func(*testing.T) func() {
					h.resolver.setErr(secret.ErrUnavailable)
					return func() { h.resolver.setErr(nil) }
				},
				status: http.StatusServiceUnavailable, errorType: "secrets_unavailable"},
			{name: "dana has as many running sandboxes as she may", ws: "demo--t1", state: sandbox.StateStopped,
				set: func(t *testing.T) func() {
					other := h.seedRecord(t, "demo--t1", sandbox.StateReady, nil)
					h.setLimits(only("max_per_user", 1))
					return func() {
						h.setLimits(unlimited)
						h.exec(t, `UPDATE sandboxes SET state = 'deleted' WHERE id = ?`, other)
					}
				},
				status: http.StatusTooManyRequests, errorType: "sandbox_capacity"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id := stopped(t, tc.ws, tc.state)
				if tc.set != nil {
					t.Cleanup(tc.set(t))
				}
				before, tokens, calls := storedRecord(t, h.Hub, id), h.tokenIDs(t), len(h.rt.Calls())

				a, _ := start(t, tc.ws, id)
				if a.status != tc.status || a.err.ErrorType != tc.errorType {
					t.Fatalf("POST start = %d %s; want %d %s", a.status, a.body, tc.status, tc.errorType)
				}
				switch {
				case tc.named != "":
					o := errorObject(t, a)
					if !reflect.DeepEqual(o[tc.named], []any{tc.values}) || len(o["scopes_searched"].([]any)) == 0 {
						t.Errorf("the error object %v; want %s [%s] and the scopes searched", o, tc.named, tc.values)
					}
				case tc.errorType == "sandbox_capacity":
					wantCapacity(t, a, "max_per_user", 1, 1)
				}
				if after := storedRecord(t, h.Hub, id); !reflect.DeepEqual(after, before) {
					t.Errorf("the sandbox is %+v; want it as it was, %+v", after, before)
				}
				if evs := h.sandboxEvents(t, sandbox.EventStart, id); len(evs) != 0 {
					t.Errorf("the start events = %+v; want none", evs)
				}
				if minted := h.newTokens(t, tokens); len(minted) != 0 {
					t.Errorf("the refused start minted %d tokens", len(minted))
				}
				if got := h.rt.Calls()[calls:]; len(got) != 0 {
					t.Errorf("the runtime was called: %v", got)
				}
			})
		}
	})

	t.Run("answers a sandbox that is provisioning or ready as it is", func(t *testing.T) {
		for _, state := range []string{sandbox.StateProvisioning, sandbox.StateReady} {
			t.Run(state, func(t *testing.T) {
				id := h.seedRecord(t, "demo--t1", state, func(r *sandbox.Record) {
					r.HubAccess = &sandbox.HubAccess{Scopes: allHubScopes, ExpiresAt: r.ExpiresAt}
				})
				before, tokens := storedRecord(t, h.Hub, id), h.tokenIDs(t)

				a, o := start(t, "demo--t1", id)
				if a.status != http.StatusOK || o.State != state || !o.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatalf("POST start = %d %s; want 200 with the sandbox %s as it was", a.status, a.body, state)
				}
				if after := storedRecord(t, h.Hub, id); !reflect.DeepEqual(after, before) {
					t.Errorf("the sandbox is %+v; want it as it was, %+v", after, before)
				}
				if evs := h.sandboxEvents(t, sandbox.EventStart, id); len(evs) != 0 {
					t.Errorf("the start events = %+v; want none", evs)
				}
				if minted := h.newTokens(t, tokens); len(minted) != 0 {
					t.Errorf("the start minted %d tokens", len(minted))
				}
			})
		}
	})

	t.Run("counts an ended sandbox toward the quotas from its start on", func(t *testing.T) {
		// A family of its own, so that the sandboxes of the other rows do
		// not count; its sandboxes have neither a checkout nor hub access,
		// so its workspace needs no repository.
		h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
			return workspace.Insert(ctx, tx, h.record("solo", access.KindRepository, workspace.StatusReady))
		})
		rt := h.rt.Client("parley", nil)
		ids := map[string]string{}
		for name, state := range map[string]string{"A": sandbox.StateReady, "B": sandbox.StateStopped, "C": sandbox.StateStopped} {
			id := h.seedRecord(t, "solo", state, func(r *sandbox.Record) { r.FamilyRoot = "solo" })
			if _, err := rt.CreateSandbox(context.Background(), runtime.SandboxSpec{Name: id, Labels: map[string]string{sandbox.LabelSandbox: id}}); err != nil {
				t.Fatal(err)
			}
			if state == sandbox.StateStopped {
				if err := rt.StopSandbox(context.Background(), id); err != nil {
					t.Fatal(err)
				}
			}
			ids[name] = id
		}
		h.setLimits(only("max_per_workspace", 2))
		t.Cleanup(func() { h.setLimits(unlimited) })

		if a, o := start(t, "solo", ids["B"]); a.status != http.StatusOK || o.State != sandbox.StateProvisioning {
			t.Fatalf("POST start B = %d %s; want 200 provisioning", a.status, a.body)
		}
		a, _ := start(t, "solo", ids["C"])
		wantCapacity(t, a, "max_per_workspace", 2, 2)
		if a := h.call(t, http.MethodPost, sandboxPath("solo", ids["A"])+"/stop", h.key, nil); a.status != http.StatusOK {
			t.Fatalf("POST stop A = %d %s; want 200", a.status, a.body)
		}
		if a, o := start(t, "solo", ids["C"]); a.status != http.StatusOK || o.State != sandbox.StateProvisioning {
			t.Fatalf("POST start C after A stopped = %d %s; want 200 provisioning", a.status, a.body)
		}
		for _, name := range []string{"B", "C"} {
			if rec := h.settled(t, ids[name]); rec.State != sandbox.StateReady {
				t.Errorf("sandbox %s is %s (%q); want ready", name, rec.State, rec.StateReason)
			}
		}
	})

	t.Run("starts an ended sandbox again with fresh values and a new hub credential", func(t *testing.T) {
		command := []string{"agent", "run"}
		for _, tc := range []struct {
			name string
			// end brings the ready sandbox id into the state the row starts
			// from.
			end func(t *testing.T, id string)
		}{
			{name: "stopped by the stop route", end: func(t *testing.T, id string) {
				if a := h.call(t, http.MethodPost, sandboxPath("demo--t1", id)+"/stop", h.key, nil); a.status != http.StatusOK {
					t.Fatalf("POST stop = %d %s", a.status, a.body)
				}
			}},
			{name: "completed with exit code 0", end: func(t *testing.T, id string) { endMain(t, h, id, sandbox.StateCompleted, "", 0) }},
			{name: "failed with exit code 3", end: func(t *testing.T, id string) { endMain(t, h, id, sandbox.StateFailed, "exit", 3) }},
			{name: "whose profile is gone from the configuration", end: func(t *testing.T, id string) {
				endMain(t, h, id, sandbox.StateCompleted, "", 0)
				h.exec(t, `UPDATE sandboxes SET profile = 'removed' WHERE id = ?`, id)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h.putSecret(t, secret.ScopeOrg, h.org, "MODEL_KEY", "model-v1-"+tc.name)
				_, rec := h.provisioned(t, "demo--t1", "", map[string]any{"command": command})
				if rec.State != sandbox.StateReady {
					t.Fatalf("the created sandbox is %s (%q); want ready", rec.State, rec.StateReason)
				}
				id := rec.ID
				old, _ := h.svc.HubCredential(id)
				tc.end(t, id)
				rotated := "model-v2-" + tc.name
				h.putSecret(t, secret.ScopeOrg, h.org, "MODEL_KEY", rotated)

				a, o := start(t, "demo--t1", id)
				if a.status != http.StatusOK || o.State != sandbox.StateProvisioning || o.ExitCode != nil || o.StateReason != nil || o.EndedAt != nil {
					t.Fatalf("POST start = %d %s; want 200 provisioning without exit_code, state_reason and ended_at", a.status, a.body)
				}
				if evs := h.sandboxEvents(t, sandbox.EventStart, id); len(evs) != 1 || evs[0].actorType != "api_key" ||
					evs[0].actorID == nil || *evs[0].actorID != h.dana {
					t.Errorf("the start events = %+v; want one by dana's API key", evs)
				}
				got := h.settled(t, id)
				if got.State != sandbox.StateReady || got.Marker != "" {
					t.Fatalf("the started sandbox is %s (%q), marker %q; want ready, unmarked", got.State, got.StateReason, got.Marker)
				}
				if p, _ := h.rt.Provider("parley", id+"-model"); p.Value != rotated {
					t.Errorf("the runtime's provider of model holds %q; want the rotated value %q", p.Value, rotated)
				}
				var mains int
				for _, c := range h.hooks.runs(id) {
					if c.kind == "main" && slices.Equal(c.argv, command) {
						mains++
					}
				}
				if mains != 2 {
					t.Errorf("the main process was started %d times; want twice", mains)
				}
				if c := h.credentialAnswer(t, old); c.status != http.StatusUnauthorized {
					t.Errorf("a request with the earlier hub credential = %d %s; want 401", c.status, c.body)
				}
				cred, ok := h.svc.HubCredential(id)
				if c := h.credentialAnswer(t, cred); !ok || cred == old || c.status != http.StatusOK {
					t.Errorf("a request with the new hub credential = %d %s; want 200", c.status, c.body)
				}
				pat, err := identity.PATByID(context.Background(), h.Store.Main(), got.HubTokenID)
				if err != nil || !slices.Equal(pat.Permissions, got.HubAccess.Scopes) || pat.ExpiresAt == nil ||
					!pat.ExpiresAt.Equal(got.ExpiresAt) || pat.RevokedAt != nil || pat.UserID != h.dana {
					t.Errorf("the new hub credential = %+v, %v; want dana's, live, with scopes %v, expiring at %v", pat, err, got.HubAccess.Scopes, got.ExpiresAt)
				}
			})
		}
	})

	t.Run("fails a start the runtime refuses and keeps its runtime sandbox", func(t *testing.T) {
		_, rec := h.provisioned(t, "demo--t1", "", map[string]any{})
		id := rec.ID
		endMain(t, h, id, sandbox.StateCompleted, "", 0)
		h.rt.FailOperation(runtime.OpStartSandbox, runtime.ErrUnsupported, "start is not supported")
		t.Cleanup(func() { h.rt.FailOperation(runtime.OpStartSandbox, nil, "") })
		calls := len(h.rt.Calls())

		if a, o := start(t, "demo--t1", id); a.status != http.StatusOK || o.State != sandbox.StateProvisioning {
			t.Fatalf("POST start = %d %s; want 200 provisioning", a.status, a.body)
		}
		got := h.settled(t, id)
		if got.State != sandbox.StateFailed || got.StateReason != sandbox.ReasonProvisionFailed || got.Marker != "" {
			t.Fatalf("the sandbox is %s (%q), marker %q; want failed for provision_failed", got.State, got.StateReason, got.Marker)
		}
		if evs := h.sandboxEvents(t, sandbox.EventFail, id); len(evs) != 1 || evs[0].metadata["reason"] != sandbox.ReasonProvisionFailed {
			t.Errorf("the fail events = %+v; want one for provision_failed", evs)
		}
		if _, ok := h.rt.Spec("parley", id); !ok || slices.Contains(h.sandboxCalls(id, calls), runtime.OpDeleteSandbox) {
			t.Errorf("the runtime sandbox is gone (calls %v); want it kept with its working directory", h.sandboxCalls(id, calls))
		}
		if pat, err := identity.PATByID(context.Background(), h.Store.Main(), got.HubTokenID); err != nil || pat.RevokedAt == nil {
			t.Errorf("the start's hub credential = %+v, %v; want it revoked", pat, err)
		}
	})

	t.Run("drops an optional credential whose secret is gone", func(t *testing.T) {
		_, rec := h.provisioned(t, "demo--t1", "", map[string]any{})
		if !slices.Equal(rec.Credentials, []string{"mirror", "model"}) {
			t.Fatalf("the created sandbox has the credentials %v; want mirror and model", rec.Credentials)
		}
		endMain(t, h, rec.ID, sandbox.StateCompleted, "", 0)
		t.Cleanup(deleteSecret(t, "MIRROR_KEY"))

		if a, o := start(t, "demo--t1", rec.ID); a.status != http.StatusOK || !slices.Equal(o.Credentials, []string{"model"}) {
			t.Fatalf("POST start = %d %s; want 200 with the credentials [model]", a.status, a.body)
		}
		if got := h.settled(t, rec.ID); got.State != sandbox.StateReady || !slices.Equal(got.Credentials, []string{"model"}) {
			t.Errorf("the started sandbox is %s (%q) with the credentials %v; want ready with [model]", got.State, got.StateReason, got.Credentials)
		}
	})
}

// endMain ends the main process of the ready sandbox id with code, as its
// end is recorded: the runtime releases the compute and the record moves to
// state with reason and the exit code.
func endMain(t *testing.T, h *createHub, id, state, reason string, code int) {
	t.Helper()
	if err := h.rt.Client("parley", nil).StopSandbox(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		applied, err := h.svc.Transition(ctx, tx, id, sandbox.StateReady, state, reason, nil)
		if err == nil && !applied {
			err = errors.New("the sandbox is not ready")
		}
		if err != nil {
			return err
		}
		return sandbox.Update(ctx, tx, id, sandbox.SetExitCode(&code))
	})
}
