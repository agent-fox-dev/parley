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

// createOwnedVars creates variables in every kind of scope of the world:
// three in the workspace acme, two in its fork acme-fork, two in owner's
// user scope and two in the org acme's.
func (w *world) createOwnedVars(t *testing.T) {
	t.Helper()
	w.create(t, w.ownerKey, "/workspaces/acme/vars", "W_ONE", "W_TWO", "SHARED")
	w.create(t, w.memberKey, "/workspaces/acme-fork/vars", "F_ONE", "SHARED")
	w.create(t, w.ownerKey, "/user/vars", "U_ONE", "U_TWO")
	w.create(t, w.ownerKey, "/orgs/acme/vars", "O_ONE", "O_TWO")
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
	w.exec(t, `CREATE TRIGGER refuse_deletes BEFORE DELETE ON `+table+` BEGIN SELECT RAISE(ABORT, 'delete refused'); END`)
	return func() { w.exec(t, `DROP TRIGGER refuse_deletes`) }
}

func TestRemoveWorkspaceVars_RemovesOneWorkspacesVariablesInTheCallersTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.createOwnedVars(t)
	_, seq := w.varEventsAfter(t, 0)
	ctx := context.Background()
	errRollback := errors.New("roll back")
	// remove removes the variables of slug in a transaction that it commits,
	// or rolls back after the removal when commit is false.
	remove := func(slug string, commit bool) error {
		err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := secret.RemoveWorkspaceVars(ctx, tx, slug); err != nil || commit {
				return err
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	}
	want := w.allVars(t)
	check := func(what string, err error) {
		t.Helper()
		if got := w.allVars(t); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %v, variables %v; want %v", what, err, got, want)
		}
	}
	check("a removal rolled back keeps every variable", remove("acme", false))
	want = without(want, "workspace/acme-fork/")
	check("a removal committed takes the fork's variables and no other", remove("acme-fork", true))

	tx, err := w.Store.Main().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(secret.RemoveWorkspaceVars(ctx, tx, "acme"), secret.RemoveVarScope(ctx, tx, secret.ScopeWorkspace, "acme")); err == nil {
		t.Error("a removal in a transaction already closed returned no error")
	}
	allow := w.refuseDeletes(t, "variables")
	if err := remove("acme", true); err == nil {
		t.Error("a removal whose delete the store refuses returned no error")
	}
	allow()
	check("the failed removals", nil)

	// A workspace made again under a removed slug starts with no variables.
	err = w.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := secret.RemoveWorkspaceVars(ctx, tx, "acme"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE slug = 'acme'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	w.AddWorkspace(t, access.Record{Slug: "acme", OwnerID: w.ownerID, OrgID: w.orgID, Kind: access.KindRepository, Status: access.StatusReady})
	if got := w.list(t, w.ownerKey, "/workspaces/acme/vars"); len(got) != 0 {
		t.Errorf("the variables of the new acme = %v, want none", keysOf(got))
	}
	var origins []string
	for _, r := range w.resolve(t, w.ownerKey, "acme") {
		origins = append(origins, r.Origin)
	}
	if want := []string{"org", "org", "user", "user"}; !slices.Equal(origins, want) {
		t.Errorf("the origins of the new acme's resolved variables = %v, want %v", origins, want)
	}
	if recs, _ := w.varEventsAfter(t, seq); len(recs) != 0 {
		t.Errorf("the removals wrote %d variable events, want none", len(recs))
	}
}

func TestWorkspaceDelete_TakesTheVariablesOfTheWorkspaceAndOfItsForks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.AddWorkspace(t, access.Record{Slug: "gone", OwnerID: w.ownerID, OrgID: w.orgID, Kind: access.KindRepository, Status: access.StatusArchived})
	w.AddWorkspace(t, access.Record{Slug: "gone-task", OwnerID: w.memberID, OrgID: w.orgID,
		Kind: access.KindFork, ParentSlug: "gone", Status: access.StatusReady})
	w.createOwnedVars(t)
	w.create(t, w.ownerKey, "/workspaces/gone/vars", "G_ONE", "SHARED")
	w.create(t, w.memberKey, "/workspaces/gone-task/vars", "T_ONE", "SHARED")
	_, seq := w.varEventsAfter(t, 0)
	left := w.allVars(t)
	// A task fork's delete, then an archived workspace's, which takes its
	// fork.
	for _, s := range []struct {
		cred, slug string
		gone       []string
	}{
		{w.memberKey, "acme-fork", []string{"workspace/acme-fork/"}},
		{w.ownerKey, "gone", []string{"workspace/gone/", "workspace/gone-task/"}},
	} {
		w.must(t, s.cred, http.MethodDelete, "/workspaces/"+s.slug, nil, http.StatusNoContent)
		if left = without(left, s.gone...); !slices.Equal(w.allVars(t), left) {
			t.Errorf("after the delete of %s the variables are %v, want %v", s.slug, w.allVars(t), left)
		}
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
	w.CreateOrg(t, "beta")
	w.createOwnedVars(t)
	w.create(t, w.outsiderKey, "/user/vars", "X_ONE", "X_TWO")
	w.create(t, w.member2Key, "/user/vars", "M2_ONE")
	w.create(t, w.Token, "/orgs/beta/vars", "B_ONE")
	_, seq := w.varEventsAfter(t, 0)
	left := w.allVars(t)
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
		allow := func() {}
		if s.refuse != "" {
			allow = w.refuseDeletes(t, s.refuse)
		}
		a := w.must(t, w.Token, http.MethodDelete, s.path, nil, s.status)
		allow()
		if s.status == http.StatusInternalServerError {
			wantError(t, s.name, a, s.status, "internal", "")
			if got := keysOf(w.list(t, w.memberKey, "/orgs/acme/vars")); !slices.Equal(got, []string{"O_ONE", "O_TWO"}) {
				t.Errorf("%s: the org's variables listed to a member = %v, want O_ONE and O_TWO", s.name, got)
			}
		}
		if left = without(left, s.gone...); !slices.Equal(w.allVars(t), left) {
			t.Errorf("%s: the variables are %v, want %v", s.name, w.allVars(t), left)
		}
	}
	if recs, _ := w.varEventsAfter(t, seq); len(recs) != 0 {
		t.Errorf("the deletes wrote %d variable events, want none", len(recs))
	}
}
