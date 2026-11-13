package hub_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
)

func TestRun_HoldsAPATToTheScopeAComposedIdentityRouteDeclares(t *testing.T) {
	t.Parallel()
	var st *store.Store
	r := startHub(t, hub.Options{WorkDir: workDir(t, localConfig), Compose: func(c *hub.Composition) {
		c.AddStartHook("test-store", func(_ context.Context, rt *hub.Runtime) error {
			st = rt.Store
			return nil
		})
	}})
	ctx := context.Background()
	var without, with string
	err := st.Tx(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		u, _, err := identity.CreateUserAndPersonalOrg(ctx, tx, identity.NewUser{
			Username: "alice", Email: "alice@example.com", Provider: "github", ProviderID: "alice", CreatedAt: now,
		}, identity.RoleUser)
		if err != nil {
			return err
		}
		if without, _, err = identity.StorePAT(ctx, tx, identity.PATSpec{UserID: u.ID, Permissions: []string{"orgs:read"}, CreatedAt: now}); err != nil {
			return err
		}
		with, _, err = identity.StorePAT(ctx, tx, identity.PATSpec{UserID: u.ID, Permissions: []string{"users:read"}, CreatedAt: now})
		return err
	})
	if err != nil {
		t.Fatalf("seed a user and two PATs: %v", err)
	}
	for _, c := range []struct {
		name, token string
		status      int
		errorType   any
	}{
		{"without users:read", without, http.StatusForbidden, "insufficient_scope"},
		{"with users:read", with, http.StatusOK, nil},
	} {
		req, err := http.NewRequest(http.MethodGet, r.url("/api/v1/user"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/v1/user: %v", err)
		}
		var body struct {
			Error struct {
				ErrorType any `json:"error_type"`
			} `json:"error"`
		}
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if err != nil || res.StatusCode != c.status || body.Error.ErrorType != c.errorType {
			t.Errorf("%s: GET /api/v1/user = %d with error_type %v (decode error %v), want %d with %v",
				c.name, res.StatusCode, body.Error.ErrorType, err, c.status, c.errorType)
		}
	}
}
