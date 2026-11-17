package secret_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// secretReads records the secret reads the resolutions of a component make,
// each as scope/scope id/name.
type secretReads struct {
	mu   sync.Mutex
	seen []string
}

// recordSecretReads starts recording the secret reads of c's resolutions.
func recordSecretReads(c *secret.Component) *secretReads {
	r := &secretReads{}
	c.RecordSecretReads(func(scope secret.Scope, id, name string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.seen = append(r.seen, fmt.Sprintf("%s/%s/%s", scope, id, name))
	})
	return r
}

// take returns the reads recorded since the last take and forgets them.
func (r *secretReads) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := r.seen
	r.seen = nil
	return seen
}

// of are those of reads made in the scope id.
func of(reads []string, scope secret.Scope, id string) []string {
	out := []string{}
	for _, r := range reads {
		if strings.HasPrefix(r, string(scope)+"/"+id+"/") {
			out = append(out, r)
		}
	}
	return out
}

// origins are the origins of steps, in order.
func origins(steps []secret.Step) []secret.Origin {
	out := []secret.Origin{}
	for _, s := range steps {
		out = append(out, s.Origin)
	}
	return out
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
	ctx := context.Background()
	w.putSecrets(t, secret.ScopeUser, w.ownerID, "TOKEN", "u", "Only_User", "x")
	w.putSecrets(t, secret.ScopeOrg, w.orgID, "token", "o")
	w.putSecrets(t, secret.ScopeWorkspace, "acme", "EMPTY", "", "DEPLOY_KEY", "p", "Both", "p")
	w.putSecrets(t, secret.ScopeWorkspace, "acme-own", "both", "g")
	own := func(slug string) secret.Step {
		return secret.Step{Origin: secret.OriginWorkspace, Scope: secret.ScopeWorkspace, ScopeID: slug}
	}
	parent := secret.Step{Origin: secret.OriginParent, Scope: secret.ScopeWorkspace, ScopeID: "acme"}
	org := secret.Step{Origin: secret.OriginOrg, Scope: secret.ScopeOrg, ScopeID: w.orgID}
	user := secret.Step{Origin: secret.OriginUser, Scope: secret.ScopeUser, ScopeID: w.ownerID}
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
		got, err := w.secrets.ResolveSecrets(ctx, c.slug, c.names)
		if err != nil || !slices.Equal(got.Secrets, c.want) || !slices.Equal(got.Searched, c.searched) {
			t.Errorf("%s: resolve(%s, %v) = %+v, %v; want %+v searching %v", c.name, c.slug, c.names, got, err, c.want, c.searched)
		}
	}
}

func TestResolveSecrets_ReadTheParentOnlyForAForkItsOwnerCanWriteAndNeverForReservedNames(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addOwnFork(t)
	ctx := context.Background()
	reads := recordSecretReads(w.secrets)
	w.putSecrets(t, secret.ScopeWorkspace, "acme", "DEPLOY_KEY", "p")
	w.putSecrets(t, secret.ScopeOrg, w.orgID, "ORG_KEY", "o")
	w.putSecrets(t, secret.ScopeWorkspace, "acme-fork", "FORK_KEY", "f")
	w.putSecrets(t, secret.ScopeUser, w.memberID, "MINE", "m")

	// acme-fork is a member's, who has no write access to acme.
	got, err := w.secrets.ResolveSecrets(ctx, "acme-fork", []string{"DEPLOY_KEY", "ORG_KEY", "FORK_KEY", "MINE"})
	want := []secret.ResolvedSecret{missing("DEPLOY_KEY"), found("ORG_KEY", "o", secret.OriginOrg),
		found("FORK_KEY", "f", secret.OriginWorkspace), found("MINE", "m", secret.OriginUser)}
	searched := []secret.Origin{secret.OriginWorkspace, secret.OriginOrg, secret.OriginUser}
	if err != nil || !slices.Equal(got.Secrets, want) || !slices.Equal(origins(got.Searched), searched) {
		t.Errorf("a member's fork: resolve = %+v, %v; want %+v searching %v", got, err, want, searched)
	}
	if parentReads := of(reads.take(), secret.ScopeWorkspace, "acme"); len(parentReads) != 0 {
		t.Errorf("a member's fork read the parent's secrets %v, want none", parentReads)
	}

	// The fork of acme's owner reads the parent, for every name but a
	// reserved one.
	got, err = w.secrets.ResolveSecrets(ctx, "acme-own", []string{"DEPLOY_KEY"})
	if want := []secret.ResolvedSecret{found("DEPLOY_KEY", "p", secret.OriginParent)}; err != nil || !slices.Equal(got.Secrets, want) {
		t.Errorf("the owner's fork: resolve(DEPLOY_KEY) = %+v, %v; want %+v", got, err, want)
	}
	if parentReads := of(reads.take(), secret.ScopeWorkspace, "acme"); len(parentReads) != 1 {
		t.Errorf("the owner's fork read the parent's secrets %v, want one read", parentReads)
	}
	reserved := []string{"GIT_PAT", "GIT_USERNAME", "GIT_PASSWORD", "UPSTREAM_GIT_PAT", "UPSTREAM_GIT_USERNAME",
		"UPSTREAM_GIT_PASSWORD", "FORGE_WEBHOOK_SECRET"}
	for _, name := range reserved {
		w.putSecrets(t, secret.ScopeWorkspace, "acme", name, "parent's "+name)
	}
	for _, name := range reserved {
		for _, asked := range []string{name, strings.ToLower(name)} {
			for _, fork := range []string{"acme-own", "acme-fork"} {
				got, err := w.secrets.ResolveSecrets(ctx, fork, []string{asked})
				if want := []secret.ResolvedSecret{missing(asked)}; err != nil || !slices.Equal(got.Secrets, want) {
					t.Errorf("%s: resolve(%s) = %+v, %v; want %+v", fork, asked, got, err, want)
				}
				if parentReads := of(reads.take(), secret.ScopeWorkspace, "acme"); len(parentReads) != 0 {
					t.Errorf("%s: resolve(%s) read the parent's secrets %v, want none", fork, asked, parentReads)
				}
			}
		}
	}
	// A reserved name is still searched in the fork's other scopes.
	w.putSecrets(t, secret.ScopeUser, w.ownerID, "GIT_PAT", "owner's pat")
	w.putSecrets(t, secret.ScopeOrg, w.orgID, "GIT_USERNAME", "org's name")
	w.putSecrets(t, secret.ScopeWorkspace, "acme-own", "Forge_Webhook_Secret", "fork's hook")
	got, err = w.secrets.ResolveSecrets(ctx, "acme-own", []string{"git_pat", "GIT_USERNAME", "FORGE_WEBHOOK_SECRET"})
	want = []secret.ResolvedSecret{found("git_pat", "owner's pat", secret.OriginUser),
		found("GIT_USERNAME", "org's name", secret.OriginOrg), found("FORGE_WEBHOOK_SECRET", "fork's hook", secret.OriginWorkspace)}
	if err != nil || !slices.Equal(got.Secrets, want) {
		t.Errorf("reserved names in the fork's other scopes: resolve = %+v, %v; want %+v", got, err, want)
	}
	if parentReads := of(reads.take(), secret.ScopeWorkspace, "acme"); len(parentReads) != 0 {
		t.Errorf("reserved names read the parent's secrets %v, want none", parentReads)
	}
}

func TestResolveSecrets_ReadTheChainAndTheValuesAgainOnEveryCall(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addOwnFork(t)
	ctx := context.Background()
	setOwner := func(id string) {
		t.Helper()
		err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE workspaces SET owner_id = ? WHERE slug = 'acme'`, id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
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
		{"the parent passes to another owner", func() { setOwner(w.member2ID) }, missing("K"), false},
		{"the parent passes back", func() { setOwner(w.ownerID) }, found("K", "p2", secret.OriginParent), true},
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
	reads := recordSecretReads(w.secrets)
	w.putSecrets(t, secret.ScopeWorkspace, "acme", "K", "k1", "OTHER", "OTHER-MARKER")
	w.putSecrets(t, secret.ScopeUser, w.ownerID, "K", "LOWER-MARKER")
	// The workspace's K is sealed bytes no key opens.
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE secrets SET sealed = x'00' WHERE scope = 'workspace' AND scope_id = 'acme' AND key = 'K'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	db := w.Store.Main()
	broken := errors.New("the store is broken 7d2a")
	memberships := access.MembershipsFunc(func(ctx context.Context, id string) ([]string, error) {
		return identity.ActiveOrgIDs(ctx, db, id)
	})
	exists := func(context.Context, string) (bool, error) { return true, nil }
	nobodyWrites := func(context.Context, string, string) (bool, error) { return false, nil }
	for _, c := range []struct {
		name, slug string
		c          *secret.Component
		check      func(err error) string
	}{
		{"an unreadable secret in the first scope that has the name", "acme", w.secrets, func(err error) string {
			switch msg := err.Error(); {
			case !errors.Is(err, secret.ErrUnreadable) || errors.Is(err, secret.ErrWorkspaceNotFound):
				return "want an unreadable error, not workspace not found"
			case !strings.Contains(msg, `"K"`) || !strings.Contains(msg, "workspace"):
				return "want the key K and the workspace scope named"
			case strings.Contains(msg, "MARKER"):
				return "want no value in it"
			}
			if lower := of(reads.take(), secret.ScopeUser, w.ownerID); len(lower) != 0 {
				return fmt.Sprintf("read %v in the user scope below it, want none", lower)
			}
			return ""
		}},
		{"a slug no workspace has", "nosuch", w.secrets, func(err error) string {
			if !errors.Is(err, secret.ErrWorkspaceNotFound) {
				return "want workspace not found"
			}
			return ""
		}},
		{"a record that cannot be read", "acme", secret.New(secret.Deps{Store: w.Store, Records: failingRecords{err: broken},
			Memberships: memberships, UserExists: exists, CanWrite: nobodyWrites}), notNotFound},
		{"a write access that cannot be read", "acme-fork", secret.New(secret.Deps{Store: w.Store, Records: access.StoreRecords(db),
			Memberships: memberships, UserExists: exists,
			CanWrite: func(context.Context, string, string) (bool, error) { return false, broken }}), notNotFound},
	} {
		got, err := c.c.ResolveSecrets(ctx, c.slug, []string{"OTHER", "K"})
		if err == nil || got.Secrets != nil || got.Searched != nil {
			t.Errorf("%s: resolve(%s) = %+v, %v; want no values and an error", c.name, c.slug, got, err)
			continue
		}
		if fault := c.check(err); fault != "" {
			t.Errorf("%s: resolve(%s) error %q: %s", c.name, c.slug, err, fault)
		}
	}
}

// notNotFound is the fault of an error that is the not-found error.
func notNotFound(err error) string {
	if errors.Is(err, secret.ErrWorkspaceNotFound) {
		return "want an error other than workspace not found"
	}
	return ""
}

// An org member's fork runs with its parent's variables, read through the
// resolved route, and without its parent's secrets, which only a fork of
// someone who may write to the parent receives.
func TestResolution_AMembersForkGetsTheParentsVariablesAndNotItsSecrets(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addOwnFork(t)
	w.putVars(t, w.ownerKey, "/workspaces/acme/vars", "REGION", "eu")
	if a := w.do(t, w.ownerKey, http.MethodPost, "/workspaces/acme/secrets",
		entries(map[string]any{"key": "DEPLOY_KEY", "value": "s3"})); a.status != http.StatusCreated {
		t.Fatalf("store the secret DEPLOY_KEY in acme = %d %s, want 201", a.status, a.body)
	}

	got := w.resolve(t, w.memberKey, "acme-fork")
	if i := slices.IndexFunc(got, func(r resolved) bool { return r.Key == "REGION" }); i < 0 ||
		got[i].Value != "eu" || got[i].Origin != string(secret.OriginParent) {
		t.Errorf("the member's resolved variables of acme-fork = %+v, want REGION = eu from the parent", got)
	}

	ctx := context.Background()
	member, err := w.secrets.ResolveSecrets(ctx, "acme-fork", []string{"DEPLOY_KEY"})
	if err != nil {
		t.Fatalf("resolve DEPLOY_KEY for acme-fork: %v", err)
	}
	if want := []secret.ResolvedSecret{missing("DEPLOY_KEY")}; !slices.Equal(member.Secrets, want) {
		t.Errorf("DEPLOY_KEY for the member's fork = %+v, want missing", member.Secrets)
	}
	if got, want := origins(member.Searched), []secret.Origin{secret.OriginWorkspace, secret.OriginOrg, secret.OriginUser}; !slices.Equal(got, want) {
		t.Errorf("the member's fork searched %v, want %v", got, want)
	}

	owners, err := w.secrets.ResolveSecrets(ctx, "acme-own", []string{"DEPLOY_KEY"})
	if err != nil {
		t.Fatalf("resolve DEPLOY_KEY for acme-own: %v", err)
	}
	if want := []secret.ResolvedSecret{found("DEPLOY_KEY", "s3", secret.OriginParent)}; !slices.Equal(owners.Secrets, want) {
		t.Errorf("DEPLOY_KEY for the owner's fork = %+v, want s3 from the parent", owners.Secrets)
	}
}
