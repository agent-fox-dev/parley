package identity_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// newOwnedHub is a team hub with the org acme, of which alice and bob are
// members, and a table owned(owner_id) holding one row owned by acme: the
// data a later package keeps for an org. It returns acme's id.
func newOwnedHub(t *testing.T) (teamHub, string) {
	t.Helper()
	h := newTeamHub(t)
	acme := h.seedOrg("Acme Inc", "acme")
	h.addMember(acme, h.alice.ID)
	h.addMember(acme, h.createUser("bob", identity.RoleUser).ID)
	h.exec(`CREATE TABLE owned (owner_id TEXT)`)
	h.exec(`INSERT INTO owned (owner_id) VALUES (?)`, acme)
	return h, acme
}

func TestDeleteOrg_CallsTheOrgRemoversInOrderInTheTransactionOfTheDelete(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		bErr error
	}{
		{"every remover succeeds", nil},
		{"a remover fails", errors.New("b failed")},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, acme := newOwnedHub(t)
			var calls []string
			userCalls, orgGone := 0, false
			h.svc.RegisterUserRemover(func(context.Context, *sql.Tx, string) error { userCalls++; return nil })
			h.svc.RegisterOrgRemover(func(ctx context.Context, tx *sql.Tx, id string) error {
				calls = append(calls, "a "+id)
				var n int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM orgs WHERE id = ?`, id).Scan(&n); err != nil || n != 1 {
					orgGone = true
				}
				_, err := tx.ExecContext(ctx, `DELETE FROM owned WHERE owner_id = ?`, id)
				return err
			})
			h.svc.RegisterOrgRemover(func(_ context.Context, _ *sql.Tx, id string) error {
				calls = append(calls, "b "+id)
				return c.bErr
			})
			before := h.snapshot()
			r := h.call(http.MethodDelete, "/api/v1/orgs/acme", h.admin, nil)
			if want := []string{"a " + acme, "b " + acme}; !slices.Equal(calls, want) {
				t.Errorf("calls = %v, want %v", calls, want)
			}
			if orgGone {
				t.Errorf("a remover ran after the org was removed")
			}
			owned := h.count(`SELECT count(*) FROM owned`)
			if c.bErr != nil {
				checkRefusal(t, "answer", r, http.StatusInternalServerError, "internal", "internal error")
				if h.snapshot() != before || owned != 1 {
					t.Errorf("the store changed (owned rows: %d, want 1)", owned)
				}
			} else {
				if r.status != http.StatusNoContent || len(r.body) != 0 {
					t.Errorf("answer = %d %s, want 204 without a body", r.status, r.body)
				}
				if owned != 0 {
					t.Errorf("owned rows = %d, want the one the remover deleted gone", owned)
				}
				checkRefusal(t, "GET /orgs/acme", h.get("/api/v1/orgs/acme", h.admin), http.StatusNotFound, "not_found", "org not found")
			}
			if userCalls != 0 {
				t.Errorf("an org delete called a user remover %d times, want none", userCalls)
			}
			n := len(calls)
			checkRefusal(t, "DELETE of a missing org", h.call(http.MethodDelete, "/api/v1/orgs/no-such-org", h.admin, nil),
				http.StatusNotFound, "not_found", "org not found")
			if len(calls) != n {
				t.Errorf("a delete answered 404 called a remover: calls = %v", calls)
			}
		})
	}
}
