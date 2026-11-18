package secret_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// countReads starts counting the secret reads in the scope id that c's
// resolutions make, and returns the count since its last call.
func countReads(c *secret.Component, scope secret.Scope, id string) func() int64 {
	var n atomic.Int64
	c.RecordSecretReads(func(s secret.Scope, sid, _ string) {
		if s == scope && sid == id {
			n.Add(1)
		}
	})
	return func() int64 { return n.Swap(0) }
}

// failingRecords is a record reader whose every read fails with err.
type failingRecords struct {
	access.RecordReader
	err error
}

func (f failingRecords) Record(context.Context, string) (access.Record, bool, error) {
	return access.Record{}, false, f.err
}

func found(name, value string, o secret.Origin) secret.ResolvedSecret {
	return secret.ResolvedSecret{Name: name, Found: true, Value: value, Origin: o}
}

func missing(name string) secret.ResolvedSecret { return secret.ResolvedSecret{Name: name} }

// addOwnFork adds acme-own, a task fork of acme that acme's owner created.
func (w *world) addOwnFork(t *testing.T) {
	t.Helper()
	w.AddWorkspace(t, access.Record{Slug: "acme-own", OwnerID: w.ownerID, OrgID: w.orgID,
		Kind: access.KindFork, ParentSlug: "acme", Status: access.StatusReady})
}

func TestResolveSecrets_AnswerEachNameFromTheFirstScopeOfTheChainThatHasIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addOwnFork(t)
	w.putSecrets(t, secret.ScopeUser, w.ownerID, "TOKEN", "u", "Only_User", "x")
	w.putSecrets(t, secret.ScopeOrg, w.orgID, "token", "o")
	w.putSecrets(t, secret.ScopeWorkspace, "acme", "EMPTY", "", "DEPLOY_KEY", "p", "Both", "p")
	w.putSecrets(t, secret.ScopeWorkspace, "acme-own", "both", "g")
	own := func(slug string) secret.Step {
		return secret.Step{Origin: secret.OriginWorkspace, Scope: secret.ScopeWorkspace, ScopeID: slug}
	}
	org := secret.Step{Origin: secret.OriginOrg, Scope: secret.ScopeOrg, ScopeID: w.orgID}
	user := secret.Step{Origin: secret.OriginUser, Scope: secret.ScopeUser, ScopeID: w.ownerID}
	parent := secret.Step{Origin: secret.OriginParent, Scope: secret.ScopeWorkspace, ScopeID: "acme"}
	for _, c := range []struct {
		name, slug string
		names      []string
		want       []secret.ResolvedSecret
		searched   []secret.Step
	}{
		{"a repository, a name asked twice answered once as first spelled", "acme",
			[]string{"TOKEN", "only_user", "EMPTY", "NOPE", "token"},
			[]secret.ResolvedSecret{found("TOKEN", "o", secret.OriginOrg), found("only_user", "x", secret.OriginUser),
				found("EMPTY", "", secret.OriginWorkspace), missing("NOPE")},
			[]secret.Step{own("acme"), org, user}},
		{"no names", "acme", []string{}, nil, []secret.Step{own("acme"), org, user}},
		{"a fork whose owner can write to the parent", "acme-own", []string{"DEPLOY_KEY", "BOTH"},
			[]secret.ResolvedSecret{found("DEPLOY_KEY", "p", secret.OriginParent), found("BOTH", "g", secret.OriginWorkspace)},
			[]secret.Step{own("acme-own"), parent, org, user}},
	} {
		got, err := w.secrets.ResolveSecrets(context.Background(), c.slug, c.names)
		if err != nil || !slices.Equal(got.Secrets, c.want) || !slices.Equal(got.Searched, c.searched) {
			t.Errorf("%s: resolve(%s, %v) = %+v, %v; want %+v searching %v", c.name, c.slug, c.names, got, err, c.want, c.searched)
		}
	}
}

func TestResolveSecrets_ReadTheParentOnlyForAForkItsOwnerCanWriteAndNeverForReservedNames(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addOwnFork(t)
	parentReads := countReads(w.secrets, secret.ScopeWorkspace, "acme")
	reserved := []string{"GIT_PAT", "GIT_USERNAME", "GIT_PASSWORD", "UPSTREAM_GIT_PAT", "UPSTREAM_GIT_USERNAME",
		"UPSTREAM_GIT_PASSWORD", "FORGE_WEBHOOK_SECRET"}
	w.putSecrets(t, secret.ScopeWorkspace, "acme", "DEPLOY_KEY", "p")
	for _, name := range reserved {
		w.putSecrets(t, secret.ScopeWorkspace, "acme", name, "parent's "+name)
	}
	w.putSecrets(t, secret.ScopeOrg, w.orgID, "ORG_KEY", "o")
	w.putSecrets(t, secret.ScopeWorkspace, "acme-fork", "FORK_KEY", "f")
	w.putSecrets(t, secret.ScopeUser, w.memberID, "MINE", "m")
	// check resolves names in the fork slug and wants the secrets want, read
	// with reads reads of the parent's secrets.
	check := func(slug string, names []string, reads int64, want ...secret.ResolvedSecret) {
		t.Helper()
		got, err := w.secrets.ResolveSecrets(context.Background(), slug, names)
		if n := parentReads(); err != nil || !slices.Equal(got.Secrets, want) || n != reads {
			t.Errorf("resolve(%s, %v) = %+v, %v with %d reads of the parent's secrets; want %+v with %d", slug, names, got.Secrets, err, n, want, reads)
		}
	}
	// acme-fork is a member's, who has no write access to acme.
	check("acme-fork", []string{"DEPLOY_KEY", "ORG_KEY", "FORK_KEY", "MINE"}, 0, missing("DEPLOY_KEY"),
		found("ORG_KEY", "o", secret.OriginOrg), found("FORK_KEY", "f", secret.OriginWorkspace), found("MINE", "m", secret.OriginUser))
	check("acme-own", []string{"DEPLOY_KEY"}, 1, found("DEPLOY_KEY", "p", secret.OriginParent))
	for _, name := range reserved {
		for _, asked := range []string{name, strings.ToLower(name)} {
			check("acme-own", []string{asked}, 0, missing(asked))
			check("acme-fork", []string{asked}, 0, missing(asked))
		}
	}
	// A reserved name is still searched in the fork's other scopes.
	w.putSecrets(t, secret.ScopeUser, w.ownerID, "GIT_PAT", "owner's pat")
	w.putSecrets(t, secret.ScopeOrg, w.orgID, "GIT_USERNAME", "org's name")
	w.putSecrets(t, secret.ScopeWorkspace, "acme-own", "Forge_Webhook_Secret", "fork's hook")
	check("acme-own", []string{"git_pat", "GIT_USERNAME", "FORGE_WEBHOOK_SECRET"}, 0, found("git_pat", "owner's pat", secret.OriginUser),
		found("GIT_USERNAME", "org's name", secret.OriginOrg), found("FORGE_WEBHOOK_SECRET", "fork's hook", secret.OriginWorkspace))
}

func TestResolveSecrets_ReadTheChainAndTheValuesAgainOnEveryCall(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addOwnFork(t)
	ctx := context.Background()
	w.putSecrets(t, secret.ScopeWorkspace, "acme", "K", "p")
	for _, c := range []struct {
		name   string
		change func()
		want   secret.ResolvedSecret
		parent bool
	}{
		{"at first", func() {}, found("K", "p", secret.OriginParent), true},
		{"the parent's value is updated", func() { w.putSecrets(t, secret.ScopeWorkspace, "acme", "K", "p2") },
			found("K", "p2", secret.OriginParent), true},
		{"the fork stores it", func() { w.putSecrets(t, secret.ScopeWorkspace, "acme-own", "k", "g") },
			found("K", "g", secret.OriginWorkspace), true},
		{"the fork deletes it", func() {
			err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
				_, err := w.secrets.Delete(ctx, tx, secret.ScopeWorkspace, "acme-own", "K")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}, found("K", "p2", secret.OriginParent), true},
		{"the parent passes to another owner", func() { w.exec(t, `UPDATE workspaces SET owner_id = ? WHERE slug = 'acme'`, w.member2ID) }, missing("K"), false},
		{"the parent passes back", func() { w.exec(t, `UPDATE workspaces SET owner_id = ? WHERE slug = 'acme'`, w.ownerID) },
			found("K", "p2", secret.OriginParent), true},
	} {
		c.change()
		got, err := w.secrets.ResolveSecrets(ctx, "acme-own", []string{"K"})
		if err != nil || !slices.Equal(got.Secrets, []secret.ResolvedSecret{c.want}) {
			t.Errorf("%s: resolve = %+v, %v; want %+v", c.name, got, err, c.want)
		}
		if has := slices.Contains(origins(got.Searched), secret.OriginParent); has != c.parent {
			t.Errorf("%s: searched %v holds the parent: %v, want %v", c.name, got.Searched, has, c.parent)
		}
	}
}

func TestResolveSecrets_FailWholeWithoutAValue(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	lowerReads := countReads(w.secrets, secret.ScopeUser, w.ownerID)
	w.putSecrets(t, secret.ScopeWorkspace, "acme", "K", "k1", "OTHER", "OTHER-MARKER")
	w.putSecrets(t, secret.ScopeUser, w.ownerID, "K", "LOWER-MARKER")
	// The workspace's K is sealed bytes no key opens.
	w.exec(t, `UPDATE secrets SET sealed = x'00' WHERE scope = 'workspace' AND scope_id = 'acme' AND key = 'K'`)
	db := w.Store.Main()
	broken := errors.New("the store is broken 7d2a")
	// with is a component on the hub's store whose readers are the hub's,
	// but for those deps sets.
	with := func(deps secret.Deps) *secret.Component {
		deps.Store = w.Store
		if deps.Records == nil {
			deps.Records = access.StoreRecords(db)
		}
		if deps.Memberships == nil {
			deps.Memberships = access.MembershipsFunc(func(ctx context.Context, id string) ([]string, error) { return identity.ActiveOrgIDs(ctx, db, id) })
		}
		if deps.UserExists == nil {
			deps.UserExists = func(context.Context, string) (bool, error) { return true, nil }
		}
		if deps.CanWrite == nil {
			deps.CanWrite = func(context.Context, string, string) (bool, error) { return false, nil }
		}
		return secret.New(deps)
	}
	if got, err := with(secret.Deps{}).ResolveSecrets(ctx, "acme-fork", []string{"OTHER"}); err != nil || len(got.Searched) != 3 {
		t.Fatalf("resolve(acme-fork) with every read answered = %+v, %v; want three steps searched", got, err)
	}
	for _, c := range []struct {
		name, slug string
		c          *secret.Component
		// unreadable: the error is the unreadable one, which names K and
		// its scope; notFound: it is workspace not found, and says so.
		unreadable, notFound bool
	}{
		{"an unreadable secret in the first scope that has the name", "acme", w.secrets, true, false},
		{"a slug no workspace has", "nosuch", w.secrets, false, true},
		{"a record that cannot be read", "acme", with(secret.Deps{Records: failingRecords{err: broken}}), false, false},
		{"memberships that cannot be read", "acme", with(secret.Deps{
			Memberships: access.MembershipsFunc(func(context.Context, string) ([]string, error) { return nil, broken })}), false, false},
		{"an owner who cannot be read", "acme", with(secret.Deps{UserExists: func(context.Context, string) (bool, error) { return false, broken }}), false, false},
		{"a write access that cannot be read", "acme-fork", with(secret.Deps{
			CanWrite: func(context.Context, string, string) (bool, error) { return false, broken }}), false, false},
	} {
		got, err := c.c.ResolveSecrets(ctx, c.slug, []string{"OTHER", "K"})
		if err == nil || got.Secrets != nil || got.Searched != nil {
			t.Errorf("%s: resolve(%s) = %+v, %v; want no values and an error", c.name, c.slug, got, err)
			continue
		}
		msg := err.Error()
		if errors.Is(err, secret.ErrUnreadable) != c.unreadable || errors.Is(err, secret.ErrWorkspaceNotFound) != c.notFound || c.notFound && msg != "workspace not found" ||
			c.unreadable && (!strings.Contains(msg, `"K"`) || !strings.Contains(msg, "workspace")) || strings.Contains(msg, "MARKER") {
			t.Errorf("%s: resolve(%s) error %q, want unreadable %v naming K and its scope, not found %v, and no value", c.name, c.slug, err, c.unreadable, c.notFound)
		}
	}
	if n := lowerReads(); n != 0 {
		t.Errorf("the unreadable secret read %d secrets in the user scope below it, want none", n)
	}
}

// An org member's fork runs with its parent's variables, read through the
// resolved route, and without its parent's secrets, which only a fork of
// someone who may write to the parent receives.
func TestResolution_AMembersForkGetsTheParentsVariablesAndNotItsSecrets(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addOwnFork(t)
	w.create(t, w.ownerKey, "/workspaces/acme/vars", "REGION=eu")
	w.must(t, w.ownerKey, http.MethodPost, "/workspaces/acme/secrets", entries(map[string]any{"key": "DEPLOY_KEY", "value": "s3"}), http.StatusCreated)
	got := w.resolve(t, w.memberKey, "acme-fork")
	if i := slices.IndexFunc(got, func(r variable) bool { return r.Key == "REGION" }); i < 0 || got[i].Value != "eu" || got[i].Origin != string(secret.OriginParent) {
		t.Errorf("the member's resolved variables of acme-fork = %+v, want REGION = eu from the parent", got)
	}
	for _, c := range []struct {
		slug     string
		want     secret.ResolvedSecret
		searched []secret.Origin
	}{
		{"acme-fork", missing("DEPLOY_KEY"), []secret.Origin{secret.OriginWorkspace, secret.OriginOrg, secret.OriginUser}},
		{"acme-own", found("DEPLOY_KEY", "s3", secret.OriginParent), []secret.Origin{secret.OriginWorkspace, secret.OriginParent, secret.OriginOrg, secret.OriginUser}},
	} {
		res, err := w.secrets.ResolveSecrets(context.Background(), c.slug, []string{"DEPLOY_KEY"})
		if err != nil || !slices.Equal(res.Secrets, []secret.ResolvedSecret{c.want}) || !slices.Equal(origins(res.Searched), c.searched) {
			t.Errorf("DEPLOY_KEY for %s = %+v, %v; want %+v searching %v", c.slug, res, err, c.want, c.searched)
		}
	}
}
