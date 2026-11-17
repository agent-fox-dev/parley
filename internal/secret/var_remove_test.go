package secret_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// seedOwnedVars stores variables in every kind of scope of the world: three
// in the workspace acme, two in its fork acme-fork, two in owner's user
// scope and two in the org acme's.
func (w *world) seedOwnedVars(t *testing.T) {
	t.Helper()
	w.seedVars(t, secret.ScopeWorkspace, "acme", "W_ONE", "W_TWO", "SHARED")
	w.seedVars(t, secret.ScopeWorkspace, "acme-fork", "F_ONE", "SHARED")
	w.seedVars(t, secret.ScopeUser, w.ownerID, "U_ONE", "U_TWO")
	w.seedVars(t, secret.ScopeOrg, w.orgID, "O_ONE", "O_TWO")
}

// without is rows, as allVars gives them, less those of the scopes whose
// "scope/scope id/" prefixes are given.
func without(rows []string, prefixes ...string) []string {
	return slices.DeleteFunc(slices.Clone(rows), func(r string) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(r, p) })
	})
}

// refuseDeletes makes every delete from table fail until the function it
// returns is called.
func (w *world) refuseDeletes(t *testing.T, table string) (allow func()) {
	t.Helper()
	ctx := context.Background()
	exec := func(query string) {
		t.Helper()
		if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, query); return err }); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`CREATE TRIGGER refuse_deletes BEFORE DELETE ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'delete refused'); END`)
	return func() { exec(`DROP TRIGGER refuse_deletes`) }
}

func TestRemoveWorkspaceVars_RemovesOneWorkspacesVariablesInTheCallersTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seedOwnedVars(t)
	_, seq := w.varEventsAfter(t, 0)
	ctx := context.Background()
	errRollback := errors.New("roll back")
	// in runs fn in a transaction it commits, or rolls back after fn when
	// commit is false.
	in := func(commit bool, fn func(tx *sql.Tx) error) error {
		err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			if !commit {
				return errRollback
			}
			return nil
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	}
	remove := func(slug string) func(tx *sql.Tx) error {
		return func(tx *sql.Tx) error { return secret.RemoveWorkspaceVars(ctx, tx, slug) }
	}
	before := w.allVars(t)

	for _, s := range []struct {
		name   string
		commit bool
		slug   string
		gone   []string
	}{
		{"a removal rolled back keeps every variable", false, "acme", nil},
		{"a removal committed takes the fork's variables and no other", true, "acme-fork", []string{"workspace/acme-fork/"}},
	} {
		t.Run(s.name, func(t *testing.T) {
			if err := in(s.commit, remove(s.slug)); err != nil {
				t.Fatal(err)
			}
			if got, want := w.allVars(t), without(before, s.gone...); !slices.Equal(got, want) {
				t.Errorf("variables = %v, want %v", got, want)
			}
		})
	}

	before = w.allVars(t)
	for _, c := range []struct {
		name string
		call func(t *testing.T) error
	}{
		{"a transaction already closed", func(t *testing.T) error {
			tx, err := w.Store.Main().BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			return errors.Join(secret.RemoveWorkspaceVars(ctx, tx, "acme"),
				secret.RemoveVarScope(ctx, tx, secret.ScopeWorkspace, "acme"))
		}},
		{"a delete the store refuses", func(t *testing.T) error {
			defer w.refuseDeletes(t, "variables")()
			return in(true, remove("acme"))
		}},
	} {
		t.Run(c.name+" fails and removes nothing", func(t *testing.T) {
			if err := c.call(t); err == nil {
				t.Error("the removal returned no error")
			}
			if got := w.allVars(t); !slices.Equal(got, before) {
				t.Errorf("variables = %v, want %v", got, before)
			}
		})
	}

	t.Run("a workspace made again under a removed slug starts with no variables", func(t *testing.T) {
		err := in(true, func(tx *sql.Tx) error {
			if err := secret.RemoveWorkspaceVars(ctx, tx, "acme"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE slug = 'acme'`)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		w.AddWorkspace(t, access.Record{Slug: "acme", OwnerID: w.ownerID, OrgID: w.orgID,
			Kind: access.KindRepository, Status: access.StatusReady})
		if got := w.listVars(t, w.ownerKey, "/workspaces/acme/vars"); len(got) != 0 {
			t.Errorf("the variables of the new acme = %v, want none", keysOf(got))
		}
		var origins []string
		for _, r := range w.resolve(t, w.ownerKey, "acme") {
			origins = append(origins, r.Origin)
		}
		if want := []string{"org", "org", "user", "user"}; !slices.Equal(origins, want) {
			t.Errorf("the origins of the new acme's resolved variables = %v, want %v", origins, want)
		}
	})

	if recs, _ := w.varEventsAfter(t, seq); len(recs) != 0 {
		t.Errorf("the removals wrote %d variable events, want none", len(recs))
	}
}

func TestWorkspaceDelete_TakesTheVariablesOfTheWorkspaceAndOfItsForks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.AddWorkspace(t, access.Record{Slug: "gone", OwnerID: w.ownerID, OrgID: w.orgID,
		Kind: access.KindRepository, Status: access.StatusArchived})
	w.AddWorkspace(t, access.Record{Slug: "gone-task", OwnerID: w.memberID, OrgID: w.orgID,
		Kind: access.KindFork, ParentSlug: "gone", Status: access.StatusReady})
	w.seedOwnedVars(t)
	w.seedVars(t, secret.ScopeWorkspace, "gone", "G_ONE", "SHARED")
	w.seedVars(t, secret.ScopeWorkspace, "gone-task", "T_ONE", "SHARED")
	_, seq := w.varEventsAfter(t, 0)
	left := w.allVars(t)

	for _, s := range []struct {
		name, cred, slug string
		gone             []string
	}{
		{"a task fork's delete", w.memberKey, "acme-fork", []string{"workspace/acme-fork/"}},
		{"an archived workspace's delete, which takes its fork", w.ownerKey, "gone",
			[]string{"workspace/gone/", "workspace/gone-task/"}},
	} {
		t.Run(s.name, func(t *testing.T) {
			if a := w.do(t, s.cred, http.MethodDelete, "/workspaces/"+s.slug, nil); a.status != http.StatusNoContent {
				t.Fatalf("DELETE /workspaces/%s = %d %.300s, want 204", s.slug, a.status, a.body)
			}
			left = without(left, s.gone...)
			if got := w.allVars(t); !slices.Equal(got, left) {
				t.Errorf("variables = %v, want %v", got, left)
			}
		})
	}
	if recs, _ := w.varEventsAfter(t, seq); len(recs) != 0 {
		t.Errorf("the deletes wrote %d variable events, want none", len(recs))
	}
}

func TestOwnerDelete_RemovesTheOwnersVariablesInItsTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// owner becomes the hub's only active admin, whom no delete removes.
	w.admin(t, http.MethodPost, "/users/"+w.ownerID+"/promote")
	beta := w.CreateOrg(t, "beta")
	w.seedOwnedVars(t)
	w.seedVars(t, secret.ScopeUser, w.outsiderID, "X_ONE", "X_TWO")
	w.seedVars(t, secret.ScopeUser, w.member2ID, "M2_ONE")
	w.seedVars(t, secret.ScopeOrg, beta, "B_ONE")
	_, seq := w.varEventsAfter(t, 0)
	left := w.allVars(t)
	orgVars := []string{"O_ONE", "O_TWO"}

	for _, s := range []struct {
		name   string
		refuse string // the table whose deletes fail during the step, if any
		path   string
		status int
		gone   []string
	}{
		{"the refused delete of the last admin", "", "/users/" + w.ownerID, http.StatusConflict, nil},
		{"an org delete whose variables cannot be deleted", "variables", "/orgs/" + w.orgID, http.StatusInternalServerError, nil},
		{"an org delete that fails after the variables were deleted", "orgs", "/orgs/" + w.orgID, http.StatusInternalServerError, nil},
		{"a user's delete", "", "/users/" + w.outsiderID, http.StatusNoContent, []string{"user/" + w.outsiderID + "/"}},
		{"an org's delete", "", "/orgs/" + w.orgID, http.StatusNoContent, []string{"org/" + w.orgID + "/"}},
	} {
		t.Run(s.name, func(t *testing.T) {
			if s.refuse != "" {
				defer w.refuseDeletes(t, s.refuse)()
			}
			a := w.do(t, w.Token, http.MethodDelete, s.path, nil)
			if a.status != s.status {
				t.Fatalf("DELETE %s = %d %.300s, want %d", s.path, a.status, a.body, s.status)
			}
			if s.status == http.StatusInternalServerError {
				if typ, _ := errorOf(t, a); typ != "internal" {
					t.Errorf("DELETE %s answered %s, want internal", s.path, a.body)
				}
				if got := keysOf(w.listVars(t, w.memberKey, "/orgs/acme/vars")); !slices.Equal(got, orgVars) {
					t.Errorf("the org's variables listed to a member = %v, want %v", got, orgVars)
				}
			}
			left = without(left, s.gone...)
			if got := w.allVars(t); !slices.Equal(got, left) {
				t.Errorf("variables = %v, want %v", got, left)
			}
		})
	}
	if recs, _ := w.varEventsAfter(t, seq); len(recs) != 0 {
		t.Errorf("the deletes wrote %d variable events, want none", len(recs))
	}
}
