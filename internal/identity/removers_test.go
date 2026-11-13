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

// refusal is a request and the error answer it must get.
type refusal struct {
	what, path       string
	status           int
	errType, message string
}

func TestDeletes_CallTheirRemoversInOrderInTheTransactionOfTheDelete(t *testing.T) {
	t.Parallel()
	deletes := []struct {
		name string
		// path is the target's URL, of the DELETE and of the GET after it.
		path, table, notFound string
		// own registers a remover the delete calls, other one it must not.
		own, other func(*identity.Service, identity.Remover)
		// target prepares the target in h and returns its id.
		target func(h teamHub, acme string) string
		// refused are deletes answered without calling a remover.
		refused []refusal
	}{
		{
			name: "user delete", path: "/api/v1/users/alice", table: "users", notFound: "user not found",
			own: (*identity.Service).RegisterUserRemover, other: (*identity.Service).RegisterOrgRemover,
			// alice owns acme, so that the org she owns is at hand for a
			// wrongly called org remover.
			target: func(h teamHub, acme string) string {
				h.exec(`UPDATE orgs SET owner_id = ? WHERE id = ?`, h.alice.ID, acme)
				h.exec(`INSERT INTO owned (owner_id) VALUES (?)`, h.alice.ID)
				return h.alice.ID
			},
			refused: []refusal{
				{"DELETE of a missing user", "/api/v1/users/nobody", http.StatusNotFound, "not_found", "user not found"},
				{"DELETE of the only active admin", "/api/v1/users/root", http.StatusConflict, "conflict",
					"the user is the only active admin"},
			},
		},
		{
			name: "org delete", path: "/api/v1/orgs/acme", table: "orgs", notFound: "org not found",
			own: (*identity.Service).RegisterOrgRemover, other: (*identity.Service).RegisterUserRemover,
			target: func(_ teamHub, acme string) string { return acme },
			refused: []refusal{
				{"DELETE of a missing org", "/api/v1/orgs/no-such-org", http.StatusNotFound, "not_found", "org not found"},
			},
		},
	}
	outcomes := []struct {
		name string
		bErr error
	}{
		{"every remover succeeds", nil},
		{"a remover fails", errors.New("b failed")},
	}
	for _, d := range deletes {
		for _, c := range outcomes {
			t.Run(d.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				h, acme := newOwnedHub(t)
				id := d.target(h, acme)
				var calls []string
				otherCalls, targetGone := 0, false
				d.other(h.svc, func(context.Context, *sql.Tx, string) error { otherCalls++; return nil })
				d.own(h.svc, func(ctx context.Context, tx *sql.Tx, id string) error {
					calls = append(calls, "a "+id)
					var n int
					err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+d.table+` WHERE id = ?`, id).Scan(&n)
					if err != nil || n != 1 {
						targetGone = true
					}
					_, err = tx.ExecContext(ctx, `DELETE FROM owned WHERE owner_id = ?`, id)
					return err
				})
				d.own(h.svc, func(_ context.Context, _ *sql.Tx, id string) error {
					calls = append(calls, "b "+id)
					return c.bErr
				})
				before := h.snapshot()
				r := h.call(http.MethodDelete, d.path, h.admin, nil)
				if want := []string{"a " + id, "b " + id}; !slices.Equal(calls, want) {
					t.Errorf("calls = %v, want %v", calls, want)
				}
				if targetGone {
					t.Errorf("a remover ran after the target was removed")
				}
				owned := h.count(`SELECT count(*) FROM owned WHERE owner_id = ?`, id)
				if c.bErr != nil {
					checkRefusal(t, "answer", r, http.StatusInternalServerError, "internal", "internal error")
					if h.snapshot() != before || owned != 1 {
						t.Errorf("the store changed (owned rows of the target: %d, want 1)", owned)
					}
				} else {
					if r.status != http.StatusNoContent || len(r.body) != 0 {
						t.Errorf("answer = %d %s, want 204 without a body", r.status, r.body)
					}
					if owned != 0 {
						t.Errorf("owned rows of the target = %d, want the one the remover deleted gone", owned)
					}
					checkRefusal(t, "GET after the delete", h.get(d.path, h.admin), http.StatusNotFound, "not_found", d.notFound)
				}
				if otherCalls != 0 {
					t.Errorf("a %s called a remover of the other kind %d times, want none", d.name, otherCalls)
				}
				n := len(calls)
				for _, f := range d.refused {
					checkRefusal(t, f.what, h.call(http.MethodDelete, f.path, h.admin, nil), f.status, f.errType, f.message)
				}
				if len(calls) != n {
					t.Errorf("a refused delete called a remover: calls = %v", calls)
				}
			})
		}
	}
}
