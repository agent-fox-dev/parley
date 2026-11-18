package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestStore_KeepsWhatItIsGiven(t *testing.T) {
	t.Parallel()
	h := startRecordHub(t)
	ctx := context.Background()
	at := recordStart
	exit, session, source, branch := 3, recordSession, sandbox.NewID(), "t1"
	full := sandbox.Record{
		ID: sandbox.NewID(), Workspace: "ws--t1", SessionID: &session, Profile: "agent", Purpose: "fix it",
		State: sandbox.StateReady, StateReason: "why", Command: []string{"agent", "run"}, Env: map[string]string{"A": "1"},
		ExitCode: &exit, Checkout: &sandbox.Checkout{Path: sandbox.WorkDir, Ref: "main", SHA: "abc", Branch: &branch},
		Image: "agent:1", CPU: "2", Memory: "4Gi", GPU: 1,
		Credentials: []string{"model"}, ExposedSecrets: []string{"Z_TOKEN"},
		WorkspaceRules: []sandbox.WorkspaceRule{{ID: "rule-1", Body: json.RawMessage(`{"host":"a.test"}`)}},
		HubAccess:      &sandbox.HubAccess{Scopes: []string{"git:read", "git:write"}, Paths: []string{"src/"}, ExpiresAt: at.Add(time.Hour)},
		PolicyRevision: 2, ExpiresAt: at.Add(time.Hour), IdleTimeout: 30 * time.Minute, LastActivityAt: at,
		Checkpoint: json.RawMessage(`{"ok":true}`), RestoredFrom: &source, Labels: map[string]string{"team": "core"},
		CreatedBy: sandbox.Creator{ActorType: "pat", ActorID: "u1", Username: "owner"},
		CreatedAt: at, UpdatedAt: at, EndedAt: &at,
		Snapshot: sandbox.Snapshot{
			TTL: 4 * time.Hour, IdleTimeout: 30 * time.Minute, Checkpoint: true, AllowProposals: true, HubAccess: "fork",
			Env: map[string]string{"B": "2"}, BasePolicy: []byte("network_policies: {}\n"),
			Credentials: []string{"model"}, OptionalCredentials: []string{"search"},
			ExposeSecrets: []string{"Z_TOKEN"}, OptionalExposeSecrets: []string{"A_TOKEN"},
			ProviderProfiles: map[string]string{"model": "anthropic", "search": "brave"},
		},
		HubTokenID: "tok1", UserID: "u1", OrgID: "org1", FamilyRoot: "ws", Marker: sandbox.MarkerStart,
		RuntimeCreated: true, RuntimeProvisioning: true, ReadyAt: &at,
	}
	tx := func(fn func(*sql.Tx) error) error { return h.Store.Tx(ctx, fn) }
	if err := tx(func(tx *sql.Tx) error { return sandbox.Insert(ctx, tx, full) }); err != nil {
		t.Fatalf("insert: %v", err)
	}
	other := h.seed(t, sandbox.StateProvisioning)

	t.Run("every field comes back", func(t *testing.T) {
		got, err := sandbox.GetIn(ctx, h.Store.Main(), "ws--t1", full.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, full) {
			t.Errorf("read back\n %+v\nwant\n %+v", got, full)
		}
		if _, err := sandbox.GetIn(ctx, h.Store.Main(), "ws", full.ID); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("read from another workspace: %v; want the not-found error", err)
		}
	})
	t.Run("list by state", func(t *testing.T) {
		recs, err := sandbox.ListByState(ctx, h.Store.Main(), sandbox.StateProvisioning)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range recs {
			ids = append(ids, r.ID)
		}
		if !slices.Contains(ids, other) || slices.Contains(ids, full.ID) {
			t.Errorf("provisioning sandboxes %v; want %s and not %s", ids, other, full.ID)
		}
	})
	t.Run("update changes the given fields only", func(t *testing.T) {
		later := at.Add(time.Minute)
		code := 0
		err := tx(func(tx *sql.Tx) error {
			return sandbox.Update(ctx, tx, full.ID,
				sandbox.SetCheckout(nil), sandbox.SetCheckpoint(nil), sandbox.SetExitCode(&code), sandbox.SetExpiresAt(later),
				sandbox.SetPurpose("other"), sandbox.SetLabels(map[string]string{}), sandbox.SetHubAccess(nil),
				sandbox.SetCredentials([]string{}), sandbox.SetExposedSecrets([]string{}), sandbox.SetPolicyRevision(3),
				sandbox.SetLastActivityAt(later), sandbox.SetHubTokenID(""), sandbox.SetMarker(""),
				sandbox.SetRuntimeCreated(false), sandbox.SetRuntimeProvisioning(false), sandbox.SetUpdatedAt(later))
		})
		if err != nil {
			t.Fatal(err)
		}
		want := full
		want.Checkout, want.Checkpoint, want.ExitCode, want.ExpiresAt = nil, nil, &code, later
		want.Purpose, want.Labels, want.HubAccess = "other", map[string]string{}, nil
		want.Credentials, want.ExposedSecrets, want.PolicyRevision = []string{}, []string{}, 3
		want.LastActivityAt, want.HubTokenID, want.Marker = later, "", ""
		want.RuntimeCreated, want.RuntimeProvisioning, want.UpdatedAt = false, false, later
		if got := h.get(t, full.ID); !reflect.DeepEqual(got, want) {
			t.Errorf("after the update\n %+v\nwant\n %+v", got, want)
		}
		err = tx(func(tx *sql.Tx) error { return sandbox.Update(ctx, tx, "sbx_none", sandbox.SetPurpose("x")) })
		if !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("update of an unknown id: %v; want the not-found error", err)
		}
	})
	t.Run("remove", func(t *testing.T) {
		if err := tx(func(tx *sql.Tx) error { return sandbox.Remove(ctx, tx, other) }); err != nil {
			t.Fatal(err)
		}
		if _, err := sandbox.Get(ctx, h.Store.Main(), other); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("read after the removal: %v; want the not-found error", err)
		}
		if err := tx(func(tx *sql.Tx) error { return sandbox.Remove(ctx, tx, other) }); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("second removal: %v; want the not-found error", err)
		}
	})
}
