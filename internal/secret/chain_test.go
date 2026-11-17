package secret_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/store"
)

func TestChain_OrdersTheScopesOfAWorkspaceFromItsRecord(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.AddWorkspace(t, access.Record{Slug: "acme-own", OwnerID: w.ownerID, OrgID: w.orgID,
		Kind: access.KindFork, ParentSlug: "acme", Status: access.StatusReady})
	own := func(slug string) secret.Step {
		return secret.Step{Origin: secret.OriginWorkspace, Scope: secret.ScopeWorkspace, ScopeID: slug}
	}
	parent := secret.Step{Origin: secret.OriginParent, Scope: secret.ScopeWorkspace, ScopeID: "acme"}
	org := secret.Step{Origin: secret.OriginOrg, Scope: secret.ScopeOrg, ScopeID: w.orgID}
	user := func(id string) secret.Step {
		return secret.Step{Origin: secret.OriginUser, Scope: secret.ScopeUser, ScopeID: id}
	}
	for _, c := range []struct {
		name, slug string
		want       []secret.Step
	}{
		{"a repository", "acme", []secret.Step{own("acme"), org, user(w.ownerID)}},
		{"a fork a member created", "acme-fork", []secret.Step{own("acme-fork"), parent, org, user(w.memberID)}},
		{"a fork the parent's owner created", "acme-own", []secret.Step{own("acme-own"), parent, org, user(w.ownerID)}},
	} {
		got, err := w.secrets.Chain(context.Background(), c.slug)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: chain(%s) = %v, %v; want %v", c.name, c.slug, got, err, c.want)
		}
	}
}

func TestChain_LeavesOutAStepTheWorkspaceNoLongerReaches(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	// A family is an org of its own, with its owner's repository, named as
	// the org, and the fork <org>-fork of it that a second member created,
	// so that what one case changes reaches no other.
	type family struct{ org, orgID, owner, forker string }
	newFamily := func(name string) family {
		f := family{org: name, orgID: w.CreateOrg(t, name), owner: w.CreateUser(t, name+"-owner"), forker: w.CreateUser(t, name+"-forker")}
		w.AddMember(t, f.orgID, f.owner)
		w.AddMember(t, f.orgID, f.forker)
		w.AddWorkspace(t, access.Record{Slug: name, OwnerID: f.owner, OrgID: f.orgID, Kind: access.KindRepository, Status: access.StatusReady})
		w.AddWorkspace(t, access.Record{Slug: name + "-fork", OwnerID: f.forker, OrgID: f.orgID,
			Kind: access.KindFork, ParentSlug: name, Status: access.StatusReady})
		return f
	}
	ws, parent, org, user := secret.OriginWorkspace, secret.OriginParent, secret.OriginOrg, secret.OriginUser
	for _, c := range []struct {
		name, org string
		fork      bool
		change    func(f family)
		want      []secret.Origin
		// undo, when set, gives the step back, and the next chain has it
		// again.
		undo func(f family)
	}{
		{"the fork's owner left the org", "leaver", true,
			func(f family) { w.admin(t, "DELETE", "/orgs/"+f.orgID+"/members/"+f.forker) },
			[]secret.Origin{ws, parent, user},
			func(f family) { w.AddMember(t, f.orgID, f.forker) }},
		{"the org is blocked", "blocked", true,
			func(f family) { w.admin(t, "POST", "/orgs/"+f.orgID+"/block") },
			[]secret.Origin{ws, parent, user},
			func(f family) { w.admin(t, "POST", "/orgs/"+f.orgID+"/unblock") }},
		{"the org is deleted", "deleted", true,
			func(f family) { w.admin(t, "DELETE", "/orgs/"+f.orgID) },
			[]secret.Origin{ws, parent, user}, nil},
		{"the repository's owner left the org", "orgless", false,
			func(f family) { w.admin(t, "DELETE", "/orgs/"+f.orgID+"/members/"+f.owner) },
			[]secret.Origin{ws, user}, nil},
		{"the fork's owner is deleted", "ownerless", true,
			func(f family) { w.admin(t, "DELETE", "/users/"+f.forker) },
			[]secret.Origin{ws, parent}, nil},
		{"the parent's record is gone", "orphan", true,
			func(f family) {
				err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE slug = ?`, f.org)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			},
			[]secret.Origin{ws, org, user}, nil},
	} {
		f := newFamily(c.org)
		slug, whole := f.org, []secret.Origin{ws, org, user}
		if c.fork {
			slug, whole = f.org+"-fork", []secret.Origin{ws, parent, org, user}
		}
		chain := func() []secret.Origin {
			steps, err := w.secrets.Chain(ctx, slug)
			if err != nil {
				t.Fatalf("%s: chain(%s): %v", c.name, slug, err)
			}
			out := []secret.Origin{}
			for _, s := range steps {
				out = append(out, s.Origin)
			}
			return out
		}
		if got := chain(); !slices.Equal(got, whole) {
			t.Fatalf("%s: before the change chain(%s) = %v, want %v", c.name, slug, got, whole)
		}
		c.change(f)
		if got := chain(); !slices.Equal(got, c.want) {
			t.Errorf("%s: chain(%s) = %v, want %v", c.name, slug, got, c.want)
		}
		if c.undo != nil {
			c.undo(f)
			if got := chain(); !slices.Equal(got, whole) {
				t.Errorf("%s: once undone chain(%s) = %v, want %v", c.name, slug, got, whole)
			}
		}
	}
}

// failingRecords is a record reader whose every read fails with err.
type failingRecords struct {
	access.RecordReader
	err error
}

func (f failingRecords) Record(context.Context, string) (access.Record, bool, error) {
	return access.Record{}, false, f.err
}

// foundRecords is a record reader that holds the repository rec only.
type foundRecords struct {
	access.RecordReader
	rec access.Record
}

func (f foundRecords) Record(_ context.Context, slug string) (access.Record, bool, error) {
	return f.rec, slug == f.rec.Slug, nil
}

func TestChain_TellsAnUnknownWorkspaceFromAFailedRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, store.Options{DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "workspaces")},
		[]store.Step{access.SchemaStep, access.CreatingTokenStep})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rec := access.Record{Slug: "w", OwnerID: "u", OrgID: "o", Kind: access.KindRepository, Status: access.StatusReady}
	if err := st.Tx(ctx, func(tx *sql.Tx) error { return access.InsertRecord(ctx, tx, rec) }); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("the store is broken")
	member := access.MembershipsFunc(func(context.Context, string) ([]string, error) { return []string{"o"}, nil })
	exists := func(context.Context, string) (bool, error) { return true, nil }
	for _, c := range []struct {
		name     string
		deps     secret.Deps
		slug     string
		notFound bool
	}{
		{"a slug no record has", secret.Deps{Records: access.StoreRecords(st.Main()), Memberships: member, UserExists: exists}, "nosuch", true},
		{"a record that cannot be read", secret.Deps{Records: failingRecords{err: broken}, Memberships: member, UserExists: exists}, "w", false},
		{"memberships that cannot be read", secret.Deps{Records: foundRecords{rec: rec},
			Memberships: access.MembershipsFunc(func(context.Context, string) ([]string, error) { return nil, broken }), UserExists: exists}, "w", false},
		{"an owner who cannot be read", secret.Deps{Records: foundRecords{rec: rec}, Memberships: member,
			UserExists: func(context.Context, string) (bool, error) { return false, broken }}, "w", false},
	} {
		steps, err := secret.New(c.deps).Chain(ctx, c.slug)
		switch {
		case err == nil || steps != nil:
			t.Errorf("%s: chain(%s) = %v, %v; want no steps and an error", c.name, c.slug, steps, err)
		case errors.Is(err, secret.ErrWorkspaceNotFound) != c.notFound:
			t.Errorf("%s: chain(%s) error %q is the not-found error: %v, want %v", c.name, c.slug, err, !c.notFound, c.notFound)
		case c.notFound && err.Error() != "workspace not found":
			t.Errorf("%s: chain(%s) error %q, want workspace not found", c.name, c.slug, err)
		}
	}
	if steps, err := secret.New(secret.Deps{Records: access.StoreRecords(st.Main()), Memberships: member, UserExists: exists}).Chain(ctx, "w"); err != nil || len(steps) != 3 {
		t.Errorf("chain(w) with every read answered = %v, %v; want three steps", steps, err)
	}
}
