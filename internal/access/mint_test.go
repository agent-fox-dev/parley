package access_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// minter is a guard hub whose service mints on clock, with alice, her API
// key's principal and the repository w1 of hers.
type minter struct {
	*guardHub
	clock *testClock
	alice string
	key   identity.Principal
}

func newMinter(t *testing.T) minter {
	h := newGuardHub(t)
	clock := newTestClock()
	h.svc.Bind(access.Deps{Store: h.st, Events: h.events, Now: clock.now, Records: access.StoreRecords(h.st.Main())})
	alice, org := h.user("alice")
	seed(t, h.st, repo("w1", alice, org, access.StatusReady))
	return minter{h, clock, alice, identity.Principal{
		Type: identity.TypeAPIKey, CredentialID: "k1", User: &identity.User{ID: alice, Role: identity.RoleUser}}}
}

// mint calls MintBound with in in a transaction of the store, which it
// commits when commit is set and rolls back otherwise.
func (m minter) mint(in access.MintInput, commit bool) (identity.PAT, string, *sql.Tx, error) {
	errRollback := errors.New("roll back")
	var pat identity.PAT
	var value string
	var mintTx *sql.Tx
	var mintErr error
	err := m.st.Tx(context.Background(), func(tx *sql.Tx) error {
		mintTx = tx
		pat, value, mintErr = m.svc.MintBound(context.Background(), tx, in)
		if mintErr != nil || !commit {
			return errRollback
		}
		return nil
	})
	if err != nil && !errors.Is(err, errRollback) {
		m.t.Fatalf("mint transaction: %v", err)
	}
	return pat, value, mintTx, mintErr
}

func TestMintBound_WritesABoundTokenAndItsEventInTheCallersTransaction(t *testing.T) {
	days := func(n int) *int { return &n }
	for _, commit := range []bool{true, false} {
		for _, tc := range []struct {
			name      string
			expires   *int
			expiresAt func(now time.Time) *time.Time
			want      func(now time.Time) *time.Time
		}{
			{name: "30 days", expires: days(30),
				want: func(now time.Time) *time.Time { at := now.Add(30 * 24 * time.Hour); return &at }},
			{name: "an instant", expiresAt: func(now time.Time) *time.Time { at := now.Add(2 * time.Hour); return &at },
				want: func(now time.Time) *time.Time { at := now.Add(2 * time.Hour); return &at }},
			{name: "never", expires: days(0), want: func(time.Time) *time.Time { return nil }},
		} {
			name := tc.name + map[bool]string{true: " committed", false: " rolled back"}[commit]
			t.Run(name, func(t *testing.T) {
				m := newMinter(t)
				now := m.clock.now()
				in := access.MintInput{Creator: m.key, Workspace: "w1", Name: "hub",
					Permissions: []string{"git:read", "git:write"}, Expires: tc.expires}
				if tc.expiresAt != nil {
					in.ExpiresAt = tc.expiresAt(now)
				}
				pat, value, tx, err := m.mint(in, commit)
				if err != nil {
					t.Fatalf("MintBound = %v, want no error", err)
				}
				if pat.Workspace != "w1" || pat.UserID != m.alice || !strings.Contains(value, "_"+pat.TokenID+"_") {
					t.Errorf("MintBound = %+v, %q, want a token of alice bound to w1 whose value holds its id", pat, value)
				}
				if want := tc.want(now); !reflect.DeepEqual(pat.ExpiresAt, want) {
					t.Errorf("expires_at = %v, want %v", pat.ExpiresAt, want)
				}
				stored := m.pats("token_id = ? AND workspace = 'w1'", pat.TokenID)
				evs := m.events.all()
				if want := bindEvent(pat, m.alice, "api_key"); len(evs) != 1 || evs[0].tx != tx || !reflect.DeepEqual(evs[0].event, want) {
					t.Fatalf("events = %+v, want one %+v in the mint's transaction", evs, want)
				}
				// The event was emitted in the mint's transaction, so it was
				// committed exactly when the token was.
				if commit && stored != 1 || !commit && stored != 0 {
					t.Errorf("committed %v: %d stored tokens %s bound to w1", commit, stored, pat.TokenID)
				}
			})
		}
	}
}

func TestMintBound_ReturnsTheErrorOfTheMintAndWritesNothing(t *testing.T) {
	days := func(n int) *int { return &n }
	pat := func(perms []string, paths []string) identity.Principal {
		return identity.Principal{Type: identity.TypePAT, CredentialID: "t1",
			User: &identity.User{ID: "alice", Role: identity.RoleUser}, Permissions: perms, Paths: paths}
	}
	for _, tc := range []struct {
		name     string
		creator  *identity.Principal
		in       access.MintInput
		wantType string
		wantIn   string
	}{
		{name: "no name", in: access.MintInput{Permissions: []string{"git:read"}}, wantType: "invalid_request"},
		{name: "no permissions", in: access.MintInput{Name: "hub", Permissions: []string{}}, wantType: "invalid_request"},
		{name: "an unknown permission", in: access.MintInput{Name: "hub", Permissions: []string{"nope:x"}}, wantType: "invalid_request"},
		{name: "45 days", in: access.MintInput{Name: "hub", Permissions: []string{"git:read"}, Expires: days(45)}, wantType: "invalid_request"},
		{name: "a PAT asking a permission it lacks", creator: new(pat([]string{"git:write"}, nil)),
			in: access.MintInput{Name: "hub", Permissions: []string{"git:read"}}, wantType: "insufficient_scope", wantIn: "git:read"},
		{name: "a PAT with paths asking none", creator: new(pat([]string{"git:read"}, []string{"src/**"})),
			in: access.MintInput{Name: "hub", Permissions: []string{"git:read"}}, wantType: "insufficient_scope", wantIn: "paths"},
		{name: "a PAT with paths asking wider ones", creator: new(pat([]string{"git:read"}, []string{"src/*"})),
			in: access.MintInput{Name: "hub", Permissions: []string{"git:read"}, Paths: []string{"src/**"}}, wantType: "insufficient_scope", wantIn: "src/**"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMinter(t)
			tc.in.Creator, tc.in.Workspace = m.key, "w1"
			if tc.creator != nil {
				tc.creator.User.ID = m.alice
				tc.in.Creator = *tc.creator
			}
			var inTx int
			err := m.st.Tx(context.Background(), func(tx *sql.Tx) error {
				_, _, err := m.svc.MintBound(context.Background(), tx, tc.in)
				if qerr := tx.QueryRow("SELECT count(*) FROM pats").Scan(&inTx); qerr != nil {
					t.Fatal(qerr)
				}
				return err
			})
			var se *server.Error
			if !errors.As(err, &se) || se.Type() != tc.wantType || !strings.Contains(se.Message(), tc.wantIn) {
				t.Errorf("MintBound = %v, want %s naming %q", err, tc.wantType, tc.wantIn)
			}
			if inTx != 0 || len(m.events.all()) != 0 {
				t.Errorf("the transaction holds %d tokens and %d events were emitted, want none", inTx, len(m.events.all()))
			}
		})
	}
}
