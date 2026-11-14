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
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// creator is a principal of userID: an API key, or with perms a PAT with
// those permissions and paths.
func creator(userID string, perms, paths []string) identity.Principal {
	p := identity.Principal{Type: identity.TypeAPIKey, CredentialID: "k1", User: &identity.User{ID: userID, Role: identity.RoleUser}}
	if perms != nil {
		p.Type, p.CredentialID, p.Permissions, p.Paths = identity.TypePAT, "t1", perms, paths
	}
	return p
}

func TestMintBound_WritesABoundTokenAndItsEventInTheCallersTransaction(t *testing.T) {
	errRollback := errors.New("roll back")
	for _, commit := range []bool{true, false} {
		b := newBindings(t)
		alice := b.tok["unbound"].user
		at := b.clock.now().Add(2 * time.Hour).Truncate(time.Second)
		var pat identity.PAT
		var value string
		var mintTx *sql.Tx
		err := b.st.Tx(context.Background(), func(tx *sql.Tx) (err error) {
			mintTx = tx
			pat, value, err = b.svc.MintBound(context.Background(), tx, access.MintInput{Creator: creator(alice, nil, nil),
				Workspace: "w1", Name: "hub", Permissions: []string{"git:read", "git:write"}, ExpiresAt: &at})
			if err == nil && !commit {
				err = errRollback
			}
			return err
		})
		if err != nil && !errors.Is(err, errRollback) {
			t.Fatalf("MintBound = %v, want no error", err)
		}
		if pat.Workspace != "w1" || pat.UserID != alice || pat.ExpiresAt == nil || !pat.ExpiresAt.Equal(at) || !strings.Contains(value, "_"+pat.TokenID+"_") {
			t.Errorf("MintBound = %+v, %q, want a token of alice bound to w1 until %v whose value holds its id", pat, value, at)
		}
		evs := b.events.all()
		if want := bindEvent(pat.TokenID, alice, "api_key", pat.Permissions, pat.Paths, timefmt.Format(at)); len(evs) != 1 || evs[0].tx != mintTx || !reflect.DeepEqual(evs[0].event, want) {
			t.Errorf("events = %+v, want one %+v in the mint's transaction", evs, want)
		}
		// The event was emitted in the mint's transaction, so it was
		// committed exactly when the token was.
		if n := b.pats("token_id = ? AND workspace = 'w1'", pat.TokenID); n != map[bool]int{true: 1}[commit] {
			t.Errorf("committed %v: %d stored tokens %s bound to w1", commit, n, pat.TokenID)
		}
	}
}

func TestMintBound_ReturnsTheErrorOfTheMintAndWritesNothing(t *testing.T) {
	days := func(n int) *int { return &n }
	for _, c := range []struct {
		name            string
		perms, paths    []string // of a PAT creator; nil for an API key
		in              access.MintInput
		errorType, text string
	}{
		{name: "no name", in: access.MintInput{Permissions: []string{"git:read"}}, errorType: "invalid_request"},
		{name: "no permissions", in: access.MintInput{Name: "hub", Permissions: []string{}}, errorType: "invalid_request"},
		{name: "an unknown permission", in: access.MintInput{Name: "hub", Permissions: []string{"nope:x"}}, errorType: "invalid_request"},
		{name: "45 days", in: access.MintInput{Name: "hub", Permissions: []string{"git:read"}, Expires: days(45)}, errorType: "invalid_request"},
		{name: "a PAT asking a permission it lacks", perms: []string{"git:write"},
			in: access.MintInput{Name: "hub", Permissions: []string{"git:read"}}, errorType: "insufficient_scope", text: "git:read"},
		{name: "a PAT with paths asking none", perms: []string{"git:read"}, paths: []string{"src/**"},
			in: access.MintInput{Name: "hub", Permissions: []string{"git:read"}}, errorType: "insufficient_scope", text: "paths"},
		{name: "a PAT with paths asking wider ones", perms: []string{"git:read"}, paths: []string{"src/*"},
			in: access.MintInput{Name: "hub", Permissions: []string{"git:read"}, Paths: []string{"src/**"}}, errorType: "insufficient_scope", text: "src/**"},
	} {
		b := newBindings(t)
		c.in.Creator, c.in.Workspace = creator(b.tok["unbound"].user, c.perms, c.paths), "w1"
		before, inTx := b.pats("1"), 0
		err := b.st.Tx(context.Background(), func(tx *sql.Tx) error {
			_, _, err := b.svc.MintBound(context.Background(), tx, c.in)
			if qerr := tx.QueryRow("SELECT count(*) FROM pats").Scan(&inTx); qerr != nil {
				t.Fatal(qerr)
			}
			return err
		})
		var se *server.Error
		if !errors.As(err, &se) || se.Type() != c.errorType || !strings.Contains(se.Message(), c.text) {
			t.Errorf("%s: MintBound = %v, want %s naming %q", c.name, err, c.errorType, c.text)
		}
		if inTx != before || len(b.events.all()) != 0 {
			t.Errorf("%s: the transaction holds %d tokens and %d events were emitted, want %d and none", c.name, inTx, len(b.events.all()), before)
		}
	}
}
