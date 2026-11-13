package identity_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// begin opens a transaction of the hub's store that the test commits or
// rolls back itself.
func (h *testHub) begin() *sql.Tx {
	h.t.Helper()
	tx, err := h.st.Main().BeginTx(context.Background(), nil)
	if err != nil {
		h.t.Fatalf("begin: %v", err)
	}
	return tx
}

// end commits tx, or rolls it back when commit is false.
func (h *testHub) end(tx *sql.Tx, commit bool) {
	h.t.Helper()
	end := tx.Rollback
	if commit {
		end = tx.Commit
	}
	if err := end(); err != nil {
		h.t.Fatalf("end the transaction: %v", err)
	}
}

// whoamiStatus is the status GET /test/whoami answers the PAT value.
func (h *testHub) whoamiStatus(value string) int {
	h.t.Helper()
	return h.get("/api/v1/test/whoami", "Bearer "+value).status
}

func TestRevokeTokenByID_RevokesThroughTheCallersTransaction(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	ctx := context.Background()
	t1, rec1 := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:write"}})
	_, rec2 := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:write"}})
	// T3 expires before the clock moves on to the revocations.
	expiry := h.now().Add(30 * time.Minute)
	_, rec3 := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, ExpiresAt: &expiry})
	_, rec4 := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}})
	h.revoke(rec4.TokenID)
	_, b1 := h.seedPAT(identity.PATSpec{UserID: h.seedUser("bob").ID, Permissions: []string{"git:read"}})
	h.advance(time.Hour)
	rec4 = h.storedPAT(rec4.TokenID)

	tx := h.begin()
	if err := identity.RevokeTokenByID(ctx, tx, rec1.TokenID, h.now()); err != nil {
		t.Fatalf("RevokeTokenByID: %v", err)
	}
	h.end(tx, false)
	if got := h.storedPAT(rec1.TokenID); !reflect.DeepEqual(got, rec1) || h.whoamiStatus(t1) != http.StatusOK {
		t.Errorf("after a rollback: T1 = %+v, want %+v, still authenticating", got, rec1)
	}

	for _, rec := range []identity.PAT{rec1, rec3, b1} {
		tx := h.begin()
		if err := identity.RevokeTokenByID(ctx, tx, rec.TokenID, h.now()); err != nil {
			t.Fatalf("RevokeTokenByID(%s): %v", rec.TokenID, err)
		}
		h.end(tx, true)
		want, now := rec, h.now()
		want.RevokedAt = &now
		if got := h.storedPAT(rec.TokenID); !reflect.DeepEqual(got, want) {
			t.Errorf("after the commit: %+v, want %+v", got, want)
		}
	}
	if h.whoamiStatus(t1) != http.StatusUnauthorized {
		t.Errorf("T1 still authenticates")
	}
	if got := h.storedPAT(rec2.TokenID); !reflect.DeepEqual(got, rec2) {
		t.Errorf("T2 = %+v, want it unchanged %+v", got, rec2)
	}

	tx = h.begin()
	if err := identity.RevokeTokenByID(ctx, tx, rec4.TokenID, h.now()); err != nil {
		t.Errorf("RevokeTokenByID of a revoked token: %v", err)
	}
	h.end(tx, true)
	if got := h.storedPAT(rec4.TokenID); !reflect.DeepEqual(got, rec4) {
		t.Errorf("a revoked token = %+v, want it unchanged %+v", got, rec4)
	}

	before := h.snapshot()
	tx = h.begin()
	if err := identity.RevokeTokenByID(ctx, tx, "zzzzzzzz", h.now()); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("RevokeTokenByID of a missing token = %v, want ErrNotFound", err)
	}
	h.end(tx, true)
	if h.snapshot() != before {
		t.Errorf("the store changed")
	}
}

func TestSetTokenExpiryByID_SetsOnlyTheExpiryThroughTheCallersTransaction(t *testing.T) {
	t.Parallel()
	h := newAliceHub(t)
	ctx := context.Background()
	soon := h.now().Add(time.Hour)
	t1, rec1 := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:write"}, ExpiresAt: &soon})
	t2, rec2 := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:write"}})
	h.advance(-time.Hour)
	t4, rec4 := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}})
	h.revoke(rec4.TokenID)
	h.advance(time.Hour)
	rec4 = h.storedPAT(rec4.TokenID)
	// The instant is kept to whole seconds.
	later := h.now().Add(6 * time.Hour)
	set := func(id string, at time.Time, commit bool) {
		t.Helper()
		tx := h.begin()
		if err := identity.SetTokenExpiryByID(ctx, tx, id, at); err != nil {
			t.Fatalf("SetTokenExpiryByID(%s): %v", id, err)
		}
		h.end(tx, commit)
	}

	set(rec1.TokenID, later.Add(500*time.Millisecond), false)
	if got := h.storedPAT(rec1.TokenID); !reflect.DeepEqual(got, rec1) {
		t.Errorf("after a rollback: T1 = %+v, want %+v", got, rec1)
	}
	set(rec1.TokenID, later.Add(500*time.Millisecond), true)
	want := rec1
	want.ExpiresAt = &later
	if got := h.storedPAT(rec1.TokenID); !reflect.DeepEqual(got, want) {
		t.Errorf("after the commit: T1 = %+v, want %+v", got, want)
	}
	h.advance(2 * time.Hour)
	if got := h.whoamiStatus(t1); got != http.StatusOK {
		t.Errorf("T1 before the new expiry = %d, want 200", got)
	}
	h.advance(later.Sub(h.now()))
	if got := h.whoamiStatus(t1); got != http.StatusUnauthorized {
		t.Errorf("T1 at the new expiry = %d, want 401", got)
	}

	set(rec2.TokenID, h.now().Add(-time.Second), true)
	if got := h.whoamiStatus(t2); got != http.StatusUnauthorized {
		t.Errorf("T2 after its expiry = %d, want 401", got)
	}
	set(rec4.TokenID, later.Add(time.Hour), true)
	if got := h.storedPAT(rec4.TokenID); !reflect.DeepEqual(got.RevokedAt, rec4.RevokedAt) || h.whoamiStatus(t4) != http.StatusUnauthorized {
		t.Errorf("revoked T4 = %+v, want revoked_at %v kept and 401", got, rec4.RevokedAt)
	}

	before := h.snapshot()
	tx := h.begin()
	if err := identity.SetTokenExpiryByID(ctx, tx, "zzzzzzzz", later); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("SetTokenExpiryByID of a missing token = %v, want ErrNotFound", err)
	}
	h.end(tx, true)
	if h.snapshot() != before {
		t.Errorf("the store changed")
	}
}
