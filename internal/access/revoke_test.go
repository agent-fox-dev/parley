package access_test

import (
	"context"
	"database/sql"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// revokeFixture is a guard hub's store with alice's tokens, by name: two
// live tokens bound to w1, one that never expires and one that expires
// later, a token bound to w1 revoked an hour ago, one bound to w1 that
// expires at the instant of the call, a live token bound to the fork f1,
// one bound to w5 and an unbound one. Its service runs on a clock that
// stands still half a second past a whole second.
type revokeFixture struct {
	h     *guardHub
	svc   *access.Service
	clock *testClock
	ids   map[string]string
}

func newRevokeFixture(t *testing.T) *revokeFixture {
	t.Helper()
	h := newGuardHub(t)
	alice, _ := h.user("alice")
	clock := newTestClock()
	clock.advance(1500 * time.Millisecond)
	now := clock.now()
	later := now.Add(time.Hour)
	f := &revokeFixture{h: h, svc: &access.Service{}, clock: clock, ids: map[string]string{}}
	for name, spec := range map[string]identity.PATSpec{
		"w1 live":     {Workspace: "w1"},
		"w1 expiring": {Workspace: "w1", ExpiresAt: &later},
		"w1 revoked":  {Workspace: "w1"},
		"w1 expired":  {Workspace: "w1", ExpiresAt: &now},
		"f1 live":     {Workspace: "f1"},
		"w5 live":     {Workspace: "w5"},
		"unbound":     {},
	} {
		spec.UserID, spec.Permissions = alice, []string{"git:read"}
		_, f.ids[name] = h.storePAT(spec)
	}
	err := h.st.Tx(context.Background(), func(tx *sql.Tx) error {
		return identity.RevokeTokenByID(context.Background(), tx, f.ids["w1 revoked"], now.Add(-time.Hour))
	})
	if err != nil {
		t.Fatalf("revoke a token: %v", err)
	}
	f.svc.Bind(access.Deps{Store: h.st, Events: h.events, Now: clock.now})
	return f
}

// begin begins a transaction of the fixture's store.
func (f *revokeFixture) begin() *sql.Tx {
	f.h.t.Helper()
	tx, err := f.h.st.Main().BeginTx(context.Background(), nil)
	if err != nil {
		f.h.t.Fatalf("begin: %v", err)
	}
	return tx
}

func TestRevokeBoundTo_RevokesTheLiveTokensOfTheSlugsAndReturnsTheirIDs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		slugs   []string
		revoked []string
	}{
		{"one workspace", []string{"w1"}, []string{"w1 live", "w1 expiring"}},
		{"a workspace and its fork", []string{"w1", "f1"}, []string{"w1 live", "w1 expiring", "f1 live"}},
		{"a workspace without bound tokens", []string{"w3"}, nil},
		{"no slug", []string{}, nil},
		{"a nil slug list", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRevokeFixture(t)
			before := f.h.revokedAt()
			tx := f.begin()
			ids, err := f.svc.RevokeBoundTo(context.Background(), tx, tc.slugs)
			if cerr := tx.Commit(); cerr != nil {
				t.Fatalf("commit: %v", cerr)
			}
			if err != nil {
				t.Fatalf("RevokeBoundTo(%q) = %v", tc.slugs, err)
			}
			want := []string{}
			for _, name := range tc.revoked {
				want = append(want, f.ids[name])
			}
			slices.Sort(want)
			if ids == nil || !slices.Equal(ids, want) {
				t.Fatalf("RevokeBoundTo(%q) = %#v, want %#v", tc.slugs, ids, want)
			}
			now := f.clock.now().Truncate(time.Second)
			for _, id := range ids {
				p, err := identity.PATByID(context.Background(), f.h.st.Main(), id)
				if err != nil {
					t.Fatalf("read PAT %s: %v", id, err)
				}
				if p.RevokedAt == nil || !p.RevokedAt.Truncate(time.Second).Equal(now) {
					t.Errorf("PAT %s (%s) revoked_at = %v, want %v", id, p.Workspace, p.RevokedAt, now)
				}
			}
			after := f.h.revokedAt()
			for name, id := range f.ids {
				if !slices.Contains(tc.revoked, name) && after[id] != before[id] {
					t.Errorf("%s: revoked_at = %v, want it unchanged, %v", name, after[id], before[id])
				}
			}
			if !before[f.ids["w1 revoked"]].Valid || before[f.ids["w1 expired"]].Valid {
				t.Fatalf("fixture: the revoked token must be revoked and the expired one not: %v", before)
			}
			if got := f.h.events.all(); len(got) != 0 {
				t.Errorf("events = %+v, want none", got)
			}
		})
	}
}

func TestRevokeBoundTo_LeavesEveryTokenAsItWasWhenRolledBackOrWhenTheWriteFails(t *testing.T) {
	for _, tc := range []struct {
		name       string
		writeFails bool
	}{
		{"the caller rolls back after the call", false},
		{"the write fails", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRevokeFixture(t)
			if tc.writeFails {
				if _, err := f.h.st.Main().Exec(`CREATE TRIGGER fail_update BEFORE UPDATE ON pats
BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
					t.Fatalf("create the failing trigger: %v", err)
				}
			}
			before := f.h.revokedAt()
			tx := f.begin()
			ids, err := f.svc.RevokeBoundTo(context.Background(), tx, []string{"w1"})
			if tc.writeFails && (err == nil || ids != nil) {
				t.Errorf("RevokeBoundTo with a failing write = %#v, %v, want nil and an error", ids, err)
			}
			if !tc.writeFails && (err != nil || len(ids) != 2) {
				t.Errorf("RevokeBoundTo = %#v, %v, want the two live tokens of w1", ids, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatalf("roll back: %v", err)
			}
			if after := f.h.revokedAt(); !maps.Equal(after, before) {
				t.Errorf("revoked_at after the rollback = %v, want %v", after, before)
			}
		})
	}
}

func TestRevokeBoundTo_RevokedTokenIsRefusedAtAuthenticationOnEveryRoute(t *testing.T) {
	r := newRefusalHub(t)
	tok, id := r.pat(r.alice, "w1", "git:read")
	if a := r.do(http.MethodGet, "/workspaces/w1/probe", tok); a.status != http.StatusOK {
		t.Fatalf("GET w1 probe before the revocation = %d %s, want 200", a.status, a.body)
	}
	var ids []string
	err := r.st.Tx(context.Background(), func(tx *sql.Tx) (err error) {
		ids, err = r.svc.RevokeBoundTo(context.Background(), tx, []string{"w1"})
		return err
	})
	if err != nil || !slices.Equal(ids, []string{id}) {
		t.Fatalf("RevokeBoundTo = %#v, %v, want [%s]", ids, err, id)
	}
	for _, path := range []string{"/workspaces/w1/probe", "/workspaces/w5/probe", "/workspaces"} {
		a := r.do(http.MethodGet, path, tok)
		if e := decode(t, a); a.status != http.StatusUnauthorized || e.Error.ErrorType != "unauthenticated" {
			t.Errorf("GET %s with the revoked token = %d %s, want 401 unauthenticated", path, a.status, a.body)
		}
	}
	if got := append(r.events.all(), r.guardHub.events.all()...); len(got) != 0 {
		t.Errorf("events = %+v, want none", got)
	}
}
