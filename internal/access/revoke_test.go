package access_test

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestRevokeBoundTo_RevokesTheLiveTokensOfTheSlugsAndReturnsTheirIDs(t *testing.T) {
	for _, c := range []struct {
		name    string
		slugs   []string
		revoked []string
	}{
		{"a workspace without a record", []string{"gone"}, []string{"gone live", "gone expiring"}},
		{"a workspace and its fork", []string{"w1", "f1"}, []string{"w1 live", "f1 live"}},
		{"a workspace without bound tokens", []string{"w3"}, nil},
		{"no slug", []string{}, nil},
		{"a nil slug list", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBindings(t)
			before := b.revokedAt()
			tx, err := b.st.Main().BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			ids, err := b.svc.RevokeBoundTo(context.Background(), tx, c.slugs)
			if cerr := tx.Commit(); err != nil || cerr != nil {
				t.Fatalf("RevokeBoundTo(%q) = %v, commit %v", c.slugs, err, cerr)
			}
			want := []string{}
			for _, name := range c.revoked {
				want = append(want, b.tok[name].id)
			}
			slices.Sort(want)
			if ids == nil || !slices.Equal(ids, want) {
				t.Fatalf("RevokeBoundTo(%q) = %#v, want %#v", c.slugs, ids, want)
			}
			b.checkRevoked(t, before, c.revoked...)
			for _, name := range c.revoked {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/user", nil)
				r.Header.Set("Authorization", "Bearer "+b.tok[name].value)
				if _, err := b.ids.Authenticate(r); err == nil {
					t.Errorf("%s still authenticates", name)
				}
			}
		})
	}
}

func TestRevokeBoundTo_LeavesEveryTokenAsItWasWhenRolledBackOrWhenTheWriteFails(t *testing.T) {
	for _, writeFails := range []bool{false, true} {
		b := newBindings(t)
		if writeFails {
			if _, err := b.st.Main().Exec(`CREATE TRIGGER fail_update BEFORE UPDATE ON pats
BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
				t.Fatalf("create the failing trigger: %v", err)
			}
		}
		before := b.revokedAt()
		tx, err := b.st.Main().BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := b.svc.RevokeBoundTo(context.Background(), tx, []string{"gone"})
		if writeFails && (err == nil || ids != nil) || !writeFails && (err != nil || len(ids) != 2) {
			t.Errorf("write fails %v: RevokeBoundTo = %#v, %v", writeFails, ids, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("roll back: %v", err)
		}
		if after := b.revokedAt(); !maps.Equal(after, before) {
			t.Errorf("write fails %v: revoked_at after the rollback = %v, want %v", writeFails, after, before)
		}
	}
}
